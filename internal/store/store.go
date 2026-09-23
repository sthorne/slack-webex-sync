// Package store persists the links between mirrored messages.
//
// A Link records that a Slack message and a Webex message are copies of each
// other; links are what let edits, deletions, thread replies and reactions
// follow a message across platforms. The same SQL runs on SQLite and
// PostgreSQL.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // registers "sqlite"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

// Link ties a Slack message to its Webex counterpart.
type Link struct {
	Pairing string
	SlackTS string
	WebexID string
	Origin  model.Platform
	// Thread roots on each side when the message is a reply.
	SlackThreadTS string
	WebexParentID string
}

// SlackRoot is the ts to use as thread_ts when replying to this message.
func (l Link) SlackRoot() string {
	if l.SlackThreadTS != "" {
		return l.SlackThreadTS
	}
	return l.SlackTS
}

// WebexRoot is the id to use as parentId when replying to this message.
func (l Link) WebexRoot() string {
	if l.WebexParentID != "" {
		return l.WebexParentID
	}
	return l.WebexID
}

// ReactionNote is the Webex text note standing in for a Slack reaction.
type ReactionNote struct {
	Pairing  string
	SlackTS  string
	User     string
	Reaction string
	WebexID  string
}

// WebexReaction is a Webex reaction mirrored as a Slack reaction.
type WebexReaction struct {
	ActivityID string
	WebexID    string // the message that was reacted to
	PersonID   string
	Reaction   string
}

// Store is the persistence interface used by the bridge.
type Store interface {
	PutLink(ctx context.Context, link Link) error
	// LinkBySlack and LinkByWebex return (nil, nil) when there is no link.
	LinkBySlack(ctx context.Context, pairing, ts string) (*Link, error)
	LinkByWebex(ctx context.Context, webexID string) (*Link, error)
	DeleteLink(ctx context.Context, link Link) error

	PutReactionNote(ctx context.Context, note ReactionNote) error
	// TakeReactionNote removes and returns a note, or (nil, nil).
	TakeReactionNote(ctx context.Context, pairing, slackTS, user, reaction string) (*ReactionNote, error)

	// AddWebexReaction records a reaction and returns how many people now
	// have that reaction on that message. Re-adding the same activity is a
	// no-op.
	AddWebexReaction(ctx context.Context, r WebexReaction) (int, error)
	// RemoveWebexReaction deletes a reaction by activity id and returns it
	// with the number of people who still have that reaction, or (nil, 0).
	RemoveWebexReaction(ctx context.Context, activityID string) (*WebexReaction, int, error)

	GetValue(ctx context.Context, key string) (string, error)
	SetValue(ctx context.Context, key, value string) error

	Close() error
}

// Open connects to the configured backend and creates tables if needed.
func Open(ctx context.Context, driver, dsn string) (Store, error) {
	var s *sqlStore
	switch driver {
	case "sqlite":
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		// SQLite allows one writer; a single connection avoids SQLITE_BUSY.
		db.SetMaxOpenConns(1)
		s = &sqlStore{db: db}
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	case "postgres":
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		s = &sqlStore{db: db, numbered: true}
	default:
		return nil, fmt.Errorf("unknown storage driver %q", driver)
	}
	if err := s.migrate(ctx); err != nil {
		s.db.Close()
		return nil, err
	}
	return s, nil
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS message_links (
		pairing         TEXT NOT NULL,
		slack_ts        TEXT NOT NULL,
		webex_id        TEXT NOT NULL UNIQUE,
		origin          TEXT NOT NULL,
		slack_thread_ts TEXT NOT NULL DEFAULT '',
		webex_parent_id TEXT NOT NULL DEFAULT '',
		created_at      BIGINT NOT NULL,
		PRIMARY KEY (pairing, slack_ts)
	)`,
	`CREATE TABLE IF NOT EXISTS reaction_notes (
		pairing   TEXT NOT NULL,
		slack_ts  TEXT NOT NULL,
		slack_user TEXT NOT NULL,
		reaction  TEXT NOT NULL,
		webex_id  TEXT NOT NULL,
		PRIMARY KEY (pairing, slack_ts, slack_user, reaction)
	)`,
	`CREATE TABLE IF NOT EXISTS webex_reactions (
		activity_id TEXT PRIMARY KEY,
		webex_id    TEXT NOT NULL,
		person_id   TEXT NOT NULL,
		reaction    TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS webex_reactions_message ON webex_reactions (webex_id, reaction)`,
	`CREATE TABLE IF NOT EXISTS kv (
		k TEXT PRIMARY KEY,
		v TEXT NOT NULL
	)`,
}

type sqlStore struct {
	db *sql.DB
	// numbered is true for drivers that use $1-style placeholders.
	numbered bool
}

func (s *sqlStore) migrate(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// q rewrites ? placeholders for drivers that need $1, $2, ...
func (s *sqlStore) q(query string) string {
	if !s.numbered {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *sqlStore) Close() error { return s.db.Close() }

func (s *sqlStore) PutLink(ctx context.Context, l Link) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO message_links
			(pairing, slack_ts, webex_id, origin, slack_thread_ts, webex_parent_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (pairing, slack_ts) DO UPDATE SET
			webex_id = excluded.webex_id,
			origin = excluded.origin,
			slack_thread_ts = excluded.slack_thread_ts,
			webex_parent_id = excluded.webex_parent_id`),
		l.Pairing, l.SlackTS, l.WebexID, string(l.Origin), l.SlackThreadTS, l.WebexParentID, time.Now().Unix())
	return err
}

const linkColumns = `pairing, slack_ts, webex_id, origin, slack_thread_ts, webex_parent_id`

func (s *sqlStore) scanLink(row *sql.Row) (*Link, error) {
	var l Link
	var origin string
	err := row.Scan(&l.Pairing, &l.SlackTS, &l.WebexID, &origin, &l.SlackThreadTS, &l.WebexParentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.Origin = model.Platform(origin)
	return &l, nil
}

func (s *sqlStore) LinkBySlack(ctx context.Context, pairing, ts string) (*Link, error) {
	return s.scanLink(s.db.QueryRowContext(ctx,
		s.q(`SELECT `+linkColumns+` FROM message_links WHERE pairing = ? AND slack_ts = ?`), pairing, ts))
}

func (s *sqlStore) LinkByWebex(ctx context.Context, webexID string) (*Link, error) {
	return s.scanLink(s.db.QueryRowContext(ctx,
		s.q(`SELECT `+linkColumns+` FROM message_links WHERE webex_id = ?`), webexID))
}

func (s *sqlStore) DeleteLink(ctx context.Context, l Link) error {
	_, err := s.db.ExecContext(ctx,
		s.q(`DELETE FROM message_links WHERE pairing = ? AND slack_ts = ?`), l.Pairing, l.SlackTS)
	return err
}

func (s *sqlStore) PutReactionNote(ctx context.Context, n ReactionNote) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO reaction_notes (pairing, slack_ts, slack_user, reaction, webex_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (pairing, slack_ts, slack_user, reaction) DO UPDATE SET webex_id = excluded.webex_id`),
		n.Pairing, n.SlackTS, n.User, n.Reaction, n.WebexID)
	return err
}

func (s *sqlStore) TakeReactionNote(ctx context.Context, pairing, slackTS, user, reaction string) (*ReactionNote, error) {
	n := ReactionNote{Pairing: pairing, SlackTS: slackTS, User: user, Reaction: reaction}
	err := s.db.QueryRowContext(ctx, s.q(`
		DELETE FROM reaction_notes
		WHERE pairing = ? AND slack_ts = ? AND slack_user = ? AND reaction = ?
		RETURNING webex_id`), pairing, slackTS, user, reaction).Scan(&n.WebexID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *sqlStore) countReaction(ctx context.Context, webexID, reaction string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		s.q(`SELECT COUNT(*) FROM webex_reactions WHERE webex_id = ? AND reaction = ?`),
		webexID, reaction).Scan(&count)
	return count, err
}

func (s *sqlStore) AddWebexReaction(ctx context.Context, r WebexReaction) (int, error) {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO webex_reactions (activity_id, webex_id, person_id, reaction)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (activity_id) DO NOTHING`),
		r.ActivityID, r.WebexID, r.PersonID, r.Reaction)
	if err != nil {
		return 0, err
	}
	return s.countReaction(ctx, r.WebexID, r.Reaction)
}

func (s *sqlStore) RemoveWebexReaction(ctx context.Context, activityID string) (*WebexReaction, int, error) {
	r := WebexReaction{ActivityID: activityID}
	err := s.db.QueryRowContext(ctx, s.q(`
		DELETE FROM webex_reactions WHERE activity_id = ?
		RETURNING webex_id, person_id, reaction`), activityID).Scan(&r.WebexID, &r.PersonID, &r.Reaction)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	remaining, err := s.countReaction(ctx, r.WebexID, r.Reaction)
	return &r, remaining, err
}

func (s *sqlStore) GetValue(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT v FROM kv WHERE k = ?`), key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *sqlStore) SetValue(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO kv (k, v) VALUES (?, ?)
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`), key, value)
	return err
}
