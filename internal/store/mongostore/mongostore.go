// Package mongostore is the MongoDB store backend, registered as "mongodb".
//
// DSN: a standard MongoDB connection string. The database comes from the
// path (default "slack_webex_sync"). One extra query parameter is read and
// removed before connecting:
//
//	mongodb://user:pass@host:27017/slack_webex_sync?authSource=admin
//	mongodb+srv://cluster.example.net/slack_webex_sync
//	...&ttl_index=false   skip native TTL indexes (for servers without them)
//
// Collections: links, reaction_notes, webex_reactions, events, kv.
//
// Expiry uses both mechanisms. Each record collection gets a native TTL
// index on created_at with expireAfterSeconds set to the retention; Open
// keeps it in step when the retention changes, and drops it when retention
// is 0. MongoDB's TTL monitor deletes expired documents about once a minute,
// and Purge deletes them outright, so reads never depend on its timing.
package mongostore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sthorne/slack-webex-sync/internal/model"
	"github.com/sthorne/slack-webex-sync/internal/store"
)

func init() { store.Register("mongodb", Open) }

const (
	defaultDatabase = "slack_webex_sync"
	ttlIndexName    = "created_at_ttl"
	sep             = "\x1f"
)

// Store implements store.Store on MongoDB.
type Store struct {
	client    *mongo.Client
	links     *mongo.Collection
	notes     *mongo.Collection
	reactions *mongo.Collection
	kv        *mongo.Collection
	events    *mongo.Collection
	now       func() time.Time
}

type linkDoc struct {
	ID            string    `bson:"_id"`
	Pairing       string    `bson:"pairing"`
	SlackTS       string    `bson:"slack_ts"`
	WebexID       string    `bson:"webex_id"`
	Origin        string    `bson:"origin"`
	SlackThreadTS string    `bson:"slack_thread_ts"`
	WebexParentID string    `bson:"webex_parent_id"`
	CreatedAt     time.Time `bson:"created_at"`
}

type noteDoc struct {
	ID        string    `bson:"_id"`
	WebexID   string    `bson:"webex_id"`
	CreatedAt time.Time `bson:"created_at"`
}

type reactionDoc struct {
	ID        string    `bson:"_id"` // activity id
	WebexID   string    `bson:"webex_id"`
	PersonID  string    `bson:"person_id"`
	Reaction  string    `bson:"reaction"`
	CreatedAt time.Time `bson:"created_at"`
}

// Open connects and prepares collections and indexes.
func Open(ctx context.Context, opts store.Options) (store.Store, error) {
	u, err := url.Parse(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse mongodb dsn: %w", err)
	}
	q := u.Query()
	useTTL := true
	if v := q.Get("ttl_index"); v != "" {
		if useTTL, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("mongodb dsn: ttl_index must be true or false")
		}
		q.Del("ttl_index")
		u.RawQuery = q.Encode()
	}
	database := strings.Trim(u.Path, "/")
	if database == "" {
		database = defaultDatabase
	}

	client, err := mongo.Connect(options.Client().ApplyURI(u.String()))
	if err != nil {
		return nil, fmt.Errorf("connect to mongodb: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("connect to mongodb: %w", err)
	}
	s, err := New(ctx, client, database, useTTL, opts)
	if err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}
	return s, nil
}

// New prepares a store on an existing client.
func New(ctx context.Context, client *mongo.Client, database string, useTTL bool, opts store.Options) (*Store, error) {
	db := client.Database(database)
	s := &Store{
		client:    client,
		links:     db.Collection("links"),
		notes:     db.Collection("reaction_notes"),
		reactions: db.Collection("webex_reactions"),
		kv:        db.Collection("kv"),
		events:    db.Collection("events"),
		now:       opts.Clock(),
	}
	if _, err := s.links.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "webex_id", Value: 1}}, Options: options.Index().SetUnique(true),
	}); err != nil {
		return nil, fmt.Errorf("create links index: %w", err)
	}
	if _, err := s.reactions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "webex_id", Value: 1}, {Key: "reaction", Value: 1}},
	}); err != nil {
		return nil, fmt.Errorf("create reactions index: %w", err)
	}
	if _, err := s.events.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "status", Value: 1}, {Key: "enqueued", Value: 1}, {Key: "_id", Value: 1}},
	}); err != nil {
		return nil, fmt.Errorf("create events index: %w", err)
	}
	for _, c := range []*mongo.Collection{s.links, s.notes, s.reactions} {
		if !useTTL {
			if _, err := c.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "created_at", Value: 1}}}); err != nil {
				return nil, fmt.Errorf("create %s index: %w", c.Name(), err)
			}
			continue
		}
		if err := syncTTLIndex(ctx, db, c, opts.Retention); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// syncTTLIndex makes the collection's TTL index match the retention:
// created, changed in place with collMod, or dropped when retention is 0.
func syncTTLIndex(ctx context.Context, db *mongo.Database, c *mongo.Collection, retention time.Duration) error {
	if retention/time.Second > math.MaxInt32 {
		return fmt.Errorf("retention %v is too long for a MongoDB TTL index; use 0 to keep links forever", retention)
	}
	seconds := int32(retention / time.Second)
	cursor, err := c.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("list %s indexes: %w", c.Name(), err)
	}
	var existing []bson.M
	if err := cursor.All(ctx, &existing); err != nil {
		return err
	}
	var current bson.M
	for _, ix := range existing {
		if ix["name"] == ttlIndexName {
			current = ix
		}
	}

	switch {
	case current == nil && seconds > 0:
		_, err = c.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetName(ttlIndexName).SetExpireAfterSeconds(seconds),
		})
	case current != nil && seconds == 0:
		err = c.Indexes().DropOne(ctx, ttlIndexName)
	case current != nil && toInt64(current["expireAfterSeconds"]) != int64(seconds):
		err = db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: c.Name()},
			{Key: "index", Value: bson.D{{Key: "name", Value: ttlIndexName}, {Key: "expireAfterSeconds", Value: seconds}}},
		}).Err()
	}
	if err != nil {
		return fmt.Errorf("ttl index on %s: %w", c.Name(), err)
	}
	return nil
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}

func (s *Store) Close() error { return s.client.Disconnect(context.Background()) }

func id(parts ...string) string { return strings.Join(parts, sep) }

// stamp is the creation time. BSON dates have millisecond precision.
func (s *Store) stamp() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

var upsert = options.Replace().SetUpsert(true)

// ---------------------------------------------------------------------------
// Links

func (s *Store) PutLink(ctx context.Context, l store.Link) error {
	doc := linkDoc{
		ID: id(l.Pairing, l.SlackTS), Pairing: l.Pairing, SlackTS: l.SlackTS, WebexID: l.WebexID,
		Origin: string(l.Origin), SlackThreadTS: l.SlackThreadTS, WebexParentID: l.WebexParentID, CreatedAt: s.stamp(),
	}
	_, err := s.links.ReplaceOne(ctx, bson.D{{Key: "_id", Value: doc.ID}}, doc, upsert)
	return err
}

func (s *Store) findLink(ctx context.Context, filter bson.D) (*store.Link, error) {
	var doc linkDoc
	err := s.links.FindOne(ctx, filter).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &store.Link{
		Pairing: doc.Pairing, SlackTS: doc.SlackTS, WebexID: doc.WebexID, Origin: model.Platform(doc.Origin),
		SlackThreadTS: doc.SlackThreadTS, WebexParentID: doc.WebexParentID,
	}, nil
}

func (s *Store) LinkBySlack(ctx context.Context, pairing, ts string) (*store.Link, error) {
	return s.findLink(ctx, bson.D{{Key: "_id", Value: id(pairing, ts)}})
}

func (s *Store) LinkByWebex(ctx context.Context, webexID string) (*store.Link, error) {
	return s.findLink(ctx, bson.D{{Key: "webex_id", Value: webexID}})
}

func (s *Store) DeleteLink(ctx context.Context, l store.Link) error {
	_, err := s.links.DeleteOne(ctx, bson.D{{Key: "_id", Value: id(l.Pairing, l.SlackTS)}})
	return err
}

// takeOne atomically removes and decodes the document with the given _id.
// It reads the document, then deletes it only if it is unchanged (match
// returns the fields to compare); whoever's delete succeeds owns it. This
// relies only on single-document deletes being atomic, not on
// findOneAndDelete, which some MongoDB-compatible servers don't implement
// atomically.
func takeOne(ctx context.Context, c *mongo.Collection, docID string, into any, match func() bson.D) (bool, error) {
	for {
		err := c.FindOne(ctx, bson.D{{Key: "_id", Value: docID}}).Decode(into)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		filter := append(bson.D{{Key: "_id", Value: docID}}, match()...)
		res, err := c.DeleteOne(ctx, filter)
		if err != nil {
			return false, err
		}
		if res.DeletedCount == 1 {
			return true, nil
		}
		// Someone else took or replaced it first; look again.
	}
}

// ---------------------------------------------------------------------------
// Reaction notes

func (s *Store) PutReactionNote(ctx context.Context, n store.ReactionNote) error {
	doc := noteDoc{ID: id(n.Pairing, n.SlackTS, n.User, n.Reaction), WebexID: n.WebexID, CreatedAt: s.stamp()}
	_, err := s.notes.ReplaceOne(ctx, bson.D{{Key: "_id", Value: doc.ID}}, doc, upsert)
	return err
}

func (s *Store) TakeReactionNote(ctx context.Context, pairing, slackTS, user, reaction string) (*store.ReactionNote, error) {
	var doc noteDoc
	found, err := takeOne(ctx, s.notes, id(pairing, slackTS, user, reaction), &doc, func() bson.D {
		return bson.D{{Key: "webex_id", Value: doc.WebexID}, {Key: "created_at", Value: doc.CreatedAt}}
	})
	if err != nil || !found {
		return nil, err
	}
	return &store.ReactionNote{Pairing: pairing, SlackTS: slackTS, User: user, Reaction: reaction, WebexID: doc.WebexID}, nil
}

// ---------------------------------------------------------------------------
// Webex reactions

func (s *Store) countReactions(ctx context.Context, webexID, reaction string) (int, error) {
	n, err := s.reactions.CountDocuments(ctx, bson.D{{Key: "webex_id", Value: webexID}, {Key: "reaction", Value: reaction}})
	return int(n), err
}

func (s *Store) AddWebexReaction(ctx context.Context, r store.WebexReaction) (int, error) {
	_, err := s.reactions.InsertOne(ctx, reactionDoc{
		ID: r.ActivityID, WebexID: r.WebexID, PersonID: r.PersonID, Reaction: r.Reaction, CreatedAt: s.stamp(),
	})
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return 0, err
	}
	return s.countReactions(ctx, r.WebexID, r.Reaction)
}

func (s *Store) RemoveWebexReaction(ctx context.Context, activityID string) (*store.WebexReaction, int, error) {
	var doc reactionDoc
	found, err := takeOne(ctx, s.reactions, activityID, &doc, func() bson.D {
		return bson.D{{Key: "created_at", Value: doc.CreatedAt}}
	})
	if err != nil || !found {
		return nil, 0, err
	}
	remaining, err := s.countReactions(ctx, doc.WebexID, doc.Reaction)
	return &store.WebexReaction{ActivityID: activityID, WebexID: doc.WebexID, PersonID: doc.PersonID, Reaction: doc.Reaction}, remaining, err
}

// ---------------------------------------------------------------------------
// Values

func (s *Store) GetValue(ctx context.Context, key string) (string, error) {
	var doc struct {
		V string `bson:"v"`
	}
	err := s.kv.FindOne(ctx, bson.D{{Key: "_id", Value: key}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	return doc.V, err
}

func (s *Store) SetValue(ctx context.Context, key, value string) error {
	_, err := s.kv.ReplaceOne(ctx, bson.D{{Key: "_id", Value: key}}, bson.D{{Key: "_id", Value: key}, {Key: "v", Value: value}}, upsert)
	return err
}

// ---------------------------------------------------------------------------
// Purge

func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	filter := bson.D{{Key: "created_at", Value: bson.D{{Key: "$lt", Value: before.UTC()}}}}
	removed := 0
	for _, c := range []*mongo.Collection{s.links, s.notes, s.reactions} {
		res, err := c.DeleteMany(ctx, filter)
		if err != nil {
			return removed, fmt.Errorf("purge %s: %w", c.Name(), err)
		}
		removed += int(res.DeletedCount)
	}
	return removed, nil
}

// ---------------------------------------------------------------------------
// Event queue (never expires; times are stored as unix microseconds)

type eventDoc struct {
	ID          string `bson:"_id"`
	Payload     []byte `bson:"payload"`
	Status      string `bson:"status"`
	Enqueued    int64  `bson:"enqueued"`
	Attempts    int    `bson:"attempts"`
	NextAttempt int64  `bson:"next_attempt"`
	LastError   string `bson:"last_error"`
}

func (d eventDoc) event() store.QueuedEvent {
	return store.QueuedEvent{
		ID: d.ID, Payload: d.Payload, Status: store.EventStatus(d.Status), Enqueued: time.UnixMicro(d.Enqueued),
		Attempts: d.Attempts, NextAttempt: time.UnixMicro(d.NextAttempt), LastError: d.LastError,
	}
}

func (s *Store) EnqueueEvent(ctx context.Context, e store.QueuedEvent) error {
	_, err := s.events.InsertOne(ctx, eventDoc{
		ID: e.ID, Payload: e.Payload, Status: string(e.Status), Enqueued: e.Enqueued.UnixMicro(),
		Attempts: e.Attempts, NextAttempt: e.NextAttempt.UnixMicro(), LastError: e.LastError,
	})
	if mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}

func (s *Store) findEvents(ctx context.Context, filter bson.D, limit int) ([]store.QueuedEvent, error) {
	cursor, err := s.events.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "enqueued", Value: 1}, {Key: "_id", Value: 1}}).
		SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	var docs []eventDoc
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]store.QueuedEvent, len(docs))
	for i, d := range docs {
		out[i] = d.event()
	}
	return out, nil
}

func (s *Store) DueEvents(ctx context.Context, now time.Time, limit int) ([]store.QueuedEvent, error) {
	return s.findEvents(ctx, bson.D{
		{Key: "status", Value: string(store.EventPending)},
		{Key: "next_attempt", Value: bson.D{{Key: "$lte", Value: now.UnixMicro()}}},
	}, limit)
}

func (s *Store) ParkedEvents(ctx context.Context, limit int) ([]store.QueuedEvent, error) {
	return s.findEvents(ctx, bson.D{{Key: "status", Value: string(store.EventParked)}}, limit)
}

func (s *Store) UpdateEvent(ctx context.Context, e store.QueuedEvent) error {
	_, err := s.events.UpdateOne(ctx, bson.D{{Key: "_id", Value: e.ID}}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(e.Status)},
		{Key: "attempts", Value: e.Attempts},
		{Key: "next_attempt", Value: e.NextAttempt.UnixMicro()},
		{Key: "last_error", Value: e.LastError},
	}}})
	return err
}

func (s *Store) DeleteEvent(ctx context.Context, id string) error {
	_, err := s.events.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
	return err
}

func (s *Store) GetEvent(ctx context.Context, id string) (*store.QueuedEvent, error) {
	var d eventDoc
	err := s.events.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e := d.event()
	return &e, nil
}

func (s *Store) CountEvents(ctx context.Context) (pending, parked int, err error) {
	p, err := s.events.CountDocuments(ctx, bson.D{{Key: "status", Value: string(store.EventPending)}})
	if err != nil {
		return 0, 0, err
	}
	k, err := s.events.CountDocuments(ctx, bson.D{{Key: "status", Value: string(store.EventParked)}})
	return int(p), int(k), err
}
