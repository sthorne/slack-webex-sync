// Package redisstore is the Redis store backend, registered as "redis".
//
// DSN: a go-redis URL, plus an optional key prefix (default "sws:"):
//
//	redis://user:pass@host:6379/0?prefix=sws:
//	rediss://...   (TLS)
//
// Layout (all keys under the prefix, fields joined by \x1f):
//
//	link:{pairing}\x1f{ts}        hash   the link
//	linkw:{webexID}               string key of the link above
//	note:{pairing}\x1f{ts}\x1f{user}\x1f{reaction}  string  note's Webex id
//	wr:{activityID}               hash   a Webex reaction
//	wrs:{webexID}\x1f{reaction}   set    activity ids with that reaction
//	kv:{key}                      string settings (never expire)
//	idx:links, idx:notes, idx:wr  zsets  record keys scored by creation time
//
// Expiry uses both mechanisms. Every record key gets a native Redis TTL
// equal to the retention. Purge walks the creation-time indexes to remove
// expired records and whatever they left behind in the secondary keys.
// Multi-key updates run as Lua scripts, so each one is atomic. The scripts
// touch keys they derive at run time, so Redis Cluster is not supported;
// standalone Redis and Sentinel both work.
package redisstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sthorne/slack-webex-sync/internal/model"
	"github.com/sthorne/slack-webex-sync/internal/store"
)

func init() { store.Register("redis", Open) }

const sep = "\x1f"

// Store implements store.Store on Redis.
type Store struct {
	rdb       redis.UniversalClient
	prefix    string
	retention time.Duration
	now       func() time.Time
}

// Open connects using a redis:// or rediss:// URL.
func Open(ctx context.Context, opts store.Options) (store.Store, error) {
	u, err := url.Parse(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse redis dsn: %w", err)
	}
	q := u.Query()
	prefix := "sws:"
	if q.Has("prefix") {
		prefix = q.Get("prefix")
		q.Del("prefix")
		u.RawQuery = q.Encode()
	}
	ropts, err := redis.ParseURL(u.String())
	if err != nil {
		return nil, fmt.Errorf("parse redis dsn: %w", err)
	}
	rdb := redis.NewClient(ropts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("connect to redis: %w", err)
	}
	return New(rdb, prefix, opts), nil
}

// New wraps an existing client.
func New(rdb redis.UniversalClient, prefix string, opts store.Options) *Store {
	return &Store{rdb: rdb, prefix: prefix, retention: opts.Retention, now: opts.Clock()}
}

func (s *Store) Close() error { return s.rdb.Close() }

func (s *Store) key(kind string, parts ...string) string {
	return s.prefix + kind + ":" + strings.Join(parts, sep)
}

// ttlMillis is the native expiry for new records; 0 means none.
func (s *Store) ttlMillis() int64 { return s.retention.Milliseconds() }

func (s *Store) score() float64 { return float64(s.now().UnixMicro()) }

// ---------------------------------------------------------------------------
// Links

// KEYS: link, linkw(new), idx:links   ARGV: 6 link fields, score, ttl, prefix
var putLinkScript = redis.NewScript(`
local old = redis.call('HGET', KEYS[1], 'webex_id')
if old and old ~= ARGV[3] then
  redis.call('DEL', ARGV[9] .. 'linkw:' .. old)
end
redis.call('HSET', KEYS[1], 'pairing', ARGV[1], 'slack_ts', ARGV[2], 'webex_id', ARGV[3],
  'origin', ARGV[4], 'slack_thread_ts', ARGV[5], 'webex_parent_id', ARGV[6])
redis.call('SET', KEYS[2], KEYS[1])
redis.call('ZADD', KEYS[3], ARGV[7], KEYS[1])
local ttl = tonumber(ARGV[8])
if ttl > 0 then
  redis.call('PEXPIRE', KEYS[1], ttl)
  redis.call('PEXPIRE', KEYS[2], ttl)
end
return 1
`)

func (s *Store) PutLink(ctx context.Context, l store.Link) error {
	return putLinkScript.Run(ctx, s.rdb,
		[]string{s.key("link", l.Pairing, l.SlackTS), s.key("linkw", l.WebexID), s.key("idx", "links")},
		l.Pairing, l.SlackTS, l.WebexID, string(l.Origin), l.SlackThreadTS, l.WebexParentID,
		s.score(), s.ttlMillis(), s.prefix,
	).Err()
}

func (s *Store) readLink(ctx context.Context, key string) (*store.Link, error) {
	fields, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(fields) == 0 {
		return nil, err
	}
	return &store.Link{
		Pairing:       fields["pairing"],
		SlackTS:       fields["slack_ts"],
		WebexID:       fields["webex_id"],
		Origin:        model.Platform(fields["origin"]),
		SlackThreadTS: fields["slack_thread_ts"],
		WebexParentID: fields["webex_parent_id"],
	}, nil
}

func (s *Store) LinkBySlack(ctx context.Context, pairing, ts string) (*store.Link, error) {
	return s.readLink(ctx, s.key("link", pairing, ts))
}

func (s *Store) LinkByWebex(ctx context.Context, webexID string) (*store.Link, error) {
	key, err := s.rdb.Get(ctx, s.key("linkw", webexID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l, err := s.readLink(ctx, key)
	if err != nil || l == nil || l.WebexID != webexID {
		return nil, err // the pointer outlived its link
	}
	return l, nil
}

// KEYS: link, idx:links   ARGV: prefix
var deleteLinkScript = redis.NewScript(`
local webex = redis.call('HGET', KEYS[1], 'webex_id')
if webex then
  redis.call('DEL', ARGV[1] .. 'linkw:' .. webex)
end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], KEYS[1])
return 1
`)

func (s *Store) DeleteLink(ctx context.Context, l store.Link) error {
	return deleteLinkScript.Run(ctx, s.rdb,
		[]string{s.key("link", l.Pairing, l.SlackTS), s.key("idx", "links")}, s.prefix).Err()
}

// ---------------------------------------------------------------------------
// Reaction notes

func (s *Store) PutReactionNote(ctx context.Context, n store.ReactionNote) error {
	key := s.key("note", n.Pairing, n.SlackTS, n.User, n.Reaction)
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, key, n.WebexID, s.retention)
	pipe.ZAdd(ctx, s.key("idx", "notes"), redis.Z{Score: s.score(), Member: key})
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) TakeReactionNote(ctx context.Context, pairing, slackTS, user, reaction string) (*store.ReactionNote, error) {
	key := s.key("note", pairing, slackTS, user, reaction)
	// GETDEL is atomic: of two racing callers, only one gets the value.
	id, err := s.rdb.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.rdb.ZRem(ctx, s.key("idx", "notes"), key)
	return &store.ReactionNote{Pairing: pairing, SlackTS: slackTS, User: user, Reaction: reaction, WebexID: id}, nil
}

// ---------------------------------------------------------------------------
// Webex reactions

const liveCount = `
-- live_count counts set members whose reaction hash still exists, dropping
-- members that native TTL has already expired.
local function live_count(set, prefix)
  local n = 0
  for _, activity in ipairs(redis.call('SMEMBERS', set)) do
    if redis.call('EXISTS', prefix .. 'wr:' .. activity) == 1 then
      n = n + 1
    else
      redis.call('SREM', set, activity)
    end
  end
  return n
end
`

// KEYS: wr, wrs, idx:wr   ARGV: activity, webex, person, reaction, score, ttl, prefix
var addReactionScript = redis.NewScript(liveCount + `
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'webex_id', ARGV[2], 'person_id', ARGV[3], 'reaction', ARGV[4])
  redis.call('SADD', KEYS[2], ARGV[1])
  redis.call('ZADD', KEYS[3], ARGV[5], KEYS[1])
  local ttl = tonumber(ARGV[6])
  if ttl > 0 then
    redis.call('PEXPIRE', KEYS[1], ttl)
  end
end
return live_count(KEYS[2], ARGV[7])
`)

func (s *Store) AddWebexReaction(ctx context.Context, r store.WebexReaction) (int, error) {
	n, err := addReactionScript.Run(ctx, s.rdb,
		[]string{s.key("wr", r.ActivityID), s.key("wrs", r.WebexID, r.Reaction), s.key("idx", "wr")},
		r.ActivityID, r.WebexID, r.PersonID, r.Reaction, s.score(), s.ttlMillis(), s.prefix,
	).Int()
	return n, err
}

// KEYS: wr, idx:wr   ARGV: activity, prefix, sep
var removeReactionScript = redis.NewScript(liveCount + `
local f = redis.call('HMGET', KEYS[1], 'webex_id', 'person_id', 'reaction')
if not f[1] then
  return false
end
local set = ARGV[2] .. 'wrs:' .. f[1] .. ARGV[3] .. f[3]
redis.call('DEL', KEYS[1])
redis.call('SREM', set, ARGV[1])
redis.call('ZREM', KEYS[2], KEYS[1])
return {f[1], f[2], f[3], live_count(set, ARGV[2])}
`)

func (s *Store) RemoveWebexReaction(ctx context.Context, activityID string) (*store.WebexReaction, int, error) {
	res, err := removeReactionScript.Run(ctx, s.rdb,
		[]string{s.key("wr", activityID), s.key("idx", "wr")}, activityID, s.prefix, sep).Slice()
	if errors.Is(err, redis.Nil) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if len(res) != 4 {
		return nil, 0, fmt.Errorf("redis: unexpected reply %v", res)
	}
	remaining, _ := res[3].(int64)
	return &store.WebexReaction{
		ActivityID: activityID,
		WebexID:    fmt.Sprint(res[0]),
		PersonID:   fmt.Sprint(res[1]),
		Reaction:   fmt.Sprint(res[2]),
	}, int(remaining), nil
}

// ---------------------------------------------------------------------------
// Values

func (s *Store) GetValue(ctx context.Context, key string) (string, error) {
	v, err := s.rdb.Get(ctx, s.key("kv", key)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetValue(ctx context.Context, key, value string) error {
	return s.rdb.Set(ctx, s.key("kv", key), value, 0).Err()
}

// ---------------------------------------------------------------------------
// Purge

// Each script removes one expired record plus its secondary keys; records
// that native TTL already removed still get their leftovers cleaned.

// KEYS: link, idx:links   ARGV: prefix
var purgeLinkScript = deleteLinkScript

// KEYS: wr, idx:wr   ARGV: prefix, sep, activity
// The set membership is removed via the stored fields when the hash still
// exists. Otherwise the leftover set member is left for the wrs cleanup below.
var purgeReactionScript = redis.NewScript(`
local f = redis.call('HMGET', KEYS[1], 'webex_id', 'reaction')
if f[1] then
  local set = ARGV[1] .. 'wrs:' .. f[1] .. ARGV[2] .. f[2]
  redis.call('SREM', set, ARGV[3])
end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], KEYS[1])
return 1
`)

const purgeBatch = 500

func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	max := strconv.FormatInt(before.UnixMicro(), 10)
	removed := 0
	expired := func(index string) ([]string, error) {
		return s.rdb.ZRangeByScore(ctx, index, &redis.ZRangeBy{Min: "-inf", Max: "(" + max, Count: purgeBatch}).Result()
	}

	for {
		keys, err := expired(s.key("idx", "links"))
		if err != nil {
			return removed, err
		}
		for _, k := range keys {
			if err := purgeLinkScript.Run(ctx, s.rdb, []string{k, s.key("idx", "links")}, s.prefix).Err(); err != nil {
				return removed, err
			}
		}
		removed += len(keys)
		if len(keys) < purgeBatch {
			break
		}
	}

	for {
		keys, err := expired(s.key("idx", "notes"))
		if err != nil {
			return removed, err
		}
		if len(keys) > 0 {
			members := make([]any, len(keys))
			for i, k := range keys {
				members[i] = k
			}
			pipe := s.rdb.TxPipeline()
			pipe.Del(ctx, keys...)
			pipe.ZRem(ctx, s.key("idx", "notes"), members...)
			if _, err := pipe.Exec(ctx); err != nil {
				return removed, err
			}
		}
		removed += len(keys)
		if len(keys) < purgeBatch {
			break
		}
	}

	activityPrefix := s.key("wr", "")
	for {
		keys, err := expired(s.key("idx", "wr"))
		if err != nil {
			return removed, err
		}
		for _, k := range keys {
			activity := strings.TrimPrefix(k, activityPrefix)
			if err := purgeReactionScript.Run(ctx, s.rdb, []string{k, s.key("idx", "wr")}, s.prefix, sep, activity).Err(); err != nil {
				return removed, err
			}
		}
		removed += len(keys)
		if len(keys) < purgeBatch {
			break
		}
	}
	return removed, s.pruneReactionSets(ctx)
}

// pruneReactionSets drops set members whose reaction hash no longer exists,
// for example because native TTL removed the hash before Purge saw it.
func (s *Store) pruneReactionSets(ctx context.Context) error {
	iter := s.rdb.Scan(ctx, 0, s.key("wrs", "")+"*", 500).Iterator()
	for iter.Next(ctx) {
		set := iter.Val()
		members, err := s.rdb.SMembers(ctx, set).Result()
		if err != nil {
			return err
		}
		for _, activity := range members {
			exists, err := s.rdb.Exists(ctx, s.key("wr", activity)).Result()
			if err != nil {
				return err
			}
			if exists == 0 {
				s.rdb.SRem(ctx, set, activity)
			}
		}
	}
	return iter.Err()
}

// ---------------------------------------------------------------------------
// Event queue
//
//	ev:{id}                  hash  the event
//	evq:pending, evq:parked  zsets event ids scored by enqueue time
//
// Queued events have no TTL and are never purged.

// KEYS: ev, evq:pending   ARGV: id, payload, status, enqueued, attempts, next, last_error
var enqueueScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
redis.call('HSET', KEYS[1], 'payload', ARGV[2], 'status', ARGV[3], 'enqueued', ARGV[4],
  'attempts', ARGV[5], 'next', ARGV[6], 'last_error', ARGV[7])
redis.call('ZADD', KEYS[2], ARGV[4], ARGV[1])
return 1
`)

func (s *Store) EnqueueEvent(ctx context.Context, e store.QueuedEvent) error {
	return enqueueScript.Run(ctx, s.rdb, []string{s.key("ev", e.ID), s.key("evq", string(store.EventPending))},
		e.ID, e.Payload, string(e.Status), e.Enqueued.UnixMicro(), e.Attempts, e.NextAttempt.UnixMicro(), e.LastError,
	).Err()
}

func (s *Store) readEvent(id string, f map[string]string) store.QueuedEvent {
	enqueued, _ := strconv.ParseInt(f["enqueued"], 10, 64)
	next, _ := strconv.ParseInt(f["next"], 10, 64)
	attempts, _ := strconv.Atoi(f["attempts"])
	return store.QueuedEvent{
		ID: id, Payload: []byte(f["payload"]), Status: store.EventStatus(f["status"]),
		Enqueued: time.UnixMicro(enqueued), Attempts: attempts, NextAttempt: time.UnixMicro(next), LastError: f["last_error"],
	}
}

// eventsIn reads events from a status index in order, keeping those that
// match, until limit are found or the index is exhausted.
func (s *Store) eventsIn(ctx context.Context, status store.EventStatus, limit int, match func(store.QueuedEvent) bool) ([]store.QueuedEvent, error) {
	const batch = 200
	var out []store.QueuedEvent
	for start := int64(0); len(out) < limit; start += batch {
		ids, err := s.rdb.ZRange(ctx, s.key("evq", string(status)), start, start+batch-1).Result()
		if err != nil {
			return nil, err
		}
		pipe := s.rdb.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(ids))
		for i, id := range ids {
			cmds[i] = pipe.HGetAll(ctx, s.key("ev", id))
		}
		if len(ids) > 0 {
			if _, err := pipe.Exec(ctx); err != nil {
				return nil, err
			}
		}
		for i, id := range ids {
			f := cmds[i].Val()
			if len(f) == 0 {
				continue
			}
			if e := s.readEvent(id, f); match(e) && len(out) < limit {
				out = append(out, e)
			}
		}
		if len(ids) < batch {
			break
		}
	}
	return out, nil
}

func (s *Store) DueEvents(ctx context.Context, now time.Time, limit int) ([]store.QueuedEvent, error) {
	return s.eventsIn(ctx, store.EventPending, limit, func(e store.QueuedEvent) bool {
		return !e.NextAttempt.After(now)
	})
}

func (s *Store) ParkedEvents(ctx context.Context, limit int) ([]store.QueuedEvent, error) {
	return s.eventsIn(ctx, store.EventParked, limit, func(store.QueuedEvent) bool { return true })
}

// KEYS: ev   ARGV: id, status, attempts, next, last_error, index prefix
var updateEventScript = redis.NewScript(`
local f = redis.call('HMGET', KEYS[1], 'status', 'enqueued')
if not f[1] then
  return 0
end
redis.call('HSET', KEYS[1], 'status', ARGV[2], 'attempts', ARGV[3], 'next', ARGV[4], 'last_error', ARGV[5])
if f[1] ~= ARGV[2] then
  redis.call('ZREM', ARGV[6] .. f[1], ARGV[1])
  redis.call('ZADD', ARGV[6] .. ARGV[2], f[2], ARGV[1])
end
return 1
`)

func (s *Store) UpdateEvent(ctx context.Context, e store.QueuedEvent) error {
	return updateEventScript.Run(ctx, s.rdb, []string{s.key("ev", e.ID)},
		e.ID, string(e.Status), e.Attempts, e.NextAttempt.UnixMicro(), e.LastError, s.key("evq", ""),
	).Err()
}

func (s *Store) DeleteEvent(ctx context.Context, id string) error {
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, s.key("ev", id))
	pipe.ZRem(ctx, s.key("evq", string(store.EventPending)), id)
	pipe.ZRem(ctx, s.key("evq", string(store.EventParked)), id)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) GetEvent(ctx context.Context, id string) (*store.QueuedEvent, error) {
	f, err := s.rdb.HGetAll(ctx, s.key("ev", id)).Result()
	if err != nil || len(f) == 0 {
		return nil, err
	}
	e := s.readEvent(id, f)
	return &e, nil
}

func (s *Store) CountEvents(ctx context.Context) (pending, parked int, err error) {
	p, err := s.rdb.ZCard(ctx, s.key("evq", string(store.EventPending))).Result()
	if err != nil {
		return 0, 0, err
	}
	k, err := s.rdb.ZCard(ctx, s.key("evq", string(store.EventParked))).Result()
	return int(p), int(k), err
}
