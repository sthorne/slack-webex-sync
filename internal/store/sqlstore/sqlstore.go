// Package sqlstore is the SQL store backend, registered as "sqlite" and
// "postgres". The same SQL runs on both; only placeholders differ.
//
// DSNs:
//
//	sqlite:   a file path, e.g. /var/lib/slack-webex-sync/links.db
//	postgres: postgres://user:pass@host:5432/dbname?sslmode=require
//
// Expired rows are removed by Purge, which the bridge runs on a schedule.
package sqlstore

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
	"github.com/sthorne/slack-webex-sync/internal/store"
)

func init() {
	store.Register("sqlite", OpenSQLite)
	store.Register("postgres", OpenPostgres)
}

// OpenSQLite opens (creating if needed) a SQLite database file.
func OpenSQLite(ctx context.Context, opts store.Options) (store.Store, error) {
	db, err := sql.Open("sqlite", opts.DSN)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY and
	// makes every method below atomic.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	return finish(ctx, &Store{db: db, now: opts.Clock()})
}

// OpenPostgres connects to PostgreSQL.
func OpenPostgres(ctx context.Context, opts store.Options) (store.Store, error) {
	db, err := sql.Open("pgx", opts.DSN)
	if err != nil {
		return nil, err
	}
	return finish(ctx, &Store{db: db, numbered: true, now: opts.Clock()})
}

func finish(ctx context.Context, s *Store) (store.Store, error) {
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
	`CREATE INDEX IF NOT EXISTS message_links_created ON message_links (created_at)`,
	`CREATE TABLE IF NOT EXISTS reaction_notes (
		pairing    TEXT NOT NULL,
		slack_ts   TEXT NOT NULL,
		slack_user TEXT NOT NULL,
		reaction   TEXT NOT NULL,
		webex_id   TEXT NOT NULL,
		created_at BIGINT NOT NULL,
		PRIMARY KEY (pairing, slack_ts, slack_user, reaction)
	)`,
	`CREATE INDEX IF NOT EXISTS reaction_notes_created ON reaction_notes (created_at)`,
	`CREATE TABLE IF NOT EXISTS webex_reactions (
		activity_id TEXT PRIMARY KEY,
		webex_id    TEXT NOT NULL,
		person_id   TEXT NOT NULL,
		reaction    TEXT NOT NULL,
		created_at  BIGINT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS webex_reactions_message ON webex_reactions (webex_id, reaction)`,
	`CREATE INDEX IF NOT EXISTS webex_reactions_created ON webex_reactions (created_at)`,
	`CREATE TABLE IF NOT EXISTS events (
		id           TEXT PRIMARY KEY,
		payload      TEXT NOT NULL,
		status       TEXT NOT NULL,
		enqueued     BIGINT NOT NULL,
		attempts     INTEGER NOT NULL DEFAULT 0,
		next_attempt BIGINT NOT NULL,
		last_error   TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS events_due ON events (status, enqueued, id)`,
	`CREATE TABLE IF NOT EXISTS kv (
		k TEXT PRIMARY KEY,
		v TEXT NOT NULL
	)`,
}

// Store implements store.Store on database/sql.
type Store struct {
	db  *sql.DB
	now func() time.Time
	// numbered is true for drivers that use $1-style placeholders.
	numbered bool
}

// DB exposes the underlying handle (used by tests to reset tables).
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// q rewrites ? placeholders for drivers that need $1, $2, ...
func (s *Store) q(query string) string {
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

// stamp is the stored creation time, in microseconds so a purge cutoff and a
// write in the same second still compare correctly.
func (s *Store) stamp() int64 { return s.now().UnixMicro() }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) PutLink(ctx context.Context, l store.Link) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO message_links
			(pairing, slack_ts, webex_id, origin, slack_thread_ts, webex_parent_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (pairing, slack_ts) DO UPDATE SET
			webex_id = excluded.webex_id,
			origin = excluded.origin,
			slack_thread_ts = excluded.slack_thread_ts,
			webex_parent_id = excluded.webex_parent_id`),
		l.Pairing, l.SlackTS, l.WebexID, string(l.Origin), l.SlackThreadTS, l.WebexParentID, s.stamp())
	return err
}

const linkColumns = `pairing, slack_ts, webex_id, origin, slack_thread_ts, webex_parent_id`

func scanLink(row *sql.Row) (*store.Link, error) {
	var l store.Link
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

func (s *Store) LinkBySlack(ctx context.Context, pairing, ts string) (*store.Link, error) {
	return scanLink(s.db.QueryRowContext(ctx,
		s.q(`SELECT `+linkColumns+` FROM message_links WHERE pairing = ? AND slack_ts = ?`), pairing, ts))
}

func (s *Store) LinkByWebex(ctx context.Context, webexID string) (*store.Link, error) {
	return scanLink(s.db.QueryRowContext(ctx,
		s.q(`SELECT `+linkColumns+` FROM message_links WHERE webex_id = ?`), webexID))
}

func (s *Store) DeleteLink(ctx context.Context, l store.Link) error {
	_, err := s.db.ExecContext(ctx,
		s.q(`DELETE FROM message_links WHERE pairing = ? AND slack_ts = ?`), l.Pairing, l.SlackTS)
	return err
}

func (s *Store) PutReactionNote(ctx context.Context, n store.ReactionNote) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO reaction_notes (pairing, slack_ts, slack_user, reaction, webex_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (pairing, slack_ts, slack_user, reaction) DO UPDATE SET
			webex_id = excluded.webex_id,
			created_at = excluded.created_at`),
		n.Pairing, n.SlackTS, n.User, n.Reaction, n.WebexID, s.stamp())
	return err
}

func (s *Store) TakeReactionNote(ctx context.Context, pairing, slackTS, user, reaction string) (*store.ReactionNote, error) {
	n := store.ReactionNote{Pairing: pairing, SlackTS: slackTS, User: user, Reaction: reaction}
	// DELETE ... RETURNING is a single atomic statement on both databases.
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

func (s *Store) countReaction(ctx context.Context, tx *sql.Tx, webexID, reaction string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx,
		s.q(`SELECT COUNT(*) FROM webex_reactions WHERE webex_id = ? AND reaction = ?`),
		webexID, reaction).Scan(&count)
	return count, err
}

func (s *Store) AddWebexReaction(ctx context.Context, r store.WebexReaction) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, s.q(`
		INSERT INTO webex_reactions (activity_id, webex_id, person_id, reaction, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (activity_id) DO NOTHING`),
		r.ActivityID, r.WebexID, r.PersonID, r.Reaction, s.stamp()); err != nil {
		return 0, err
	}
	count, err := s.countReaction(ctx, tx, r.WebexID, r.Reaction)
	if err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

func (s *Store) RemoveWebexReaction(ctx context.Context, activityID string) (*store.WebexReaction, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	r := store.WebexReaction{ActivityID: activityID}
	err = tx.QueryRowContext(ctx, s.q(`
		DELETE FROM webex_reactions WHERE activity_id = ?
		RETURNING webex_id, person_id, reaction`), activityID).Scan(&r.WebexID, &r.PersonID, &r.Reaction)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	remaining, err := s.countReaction(ctx, tx, r.WebexID, r.Reaction)
	if err != nil {
		return nil, 0, err
	}
	return &r, remaining, tx.Commit()
}

func (s *Store) GetValue(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT v FROM kv WHERE k = ?`), key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetValue(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO kv (k, v) VALUES (?, ?)
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`), key, value)
	return err
}

func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	removed := 0
	for _, table := range []string{"message_links", "reaction_notes", "webex_reactions"} {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM `+table+` WHERE created_at < ?`), before.UnixMicro())
		if err != nil {
			return 0, fmt.Errorf("purge %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	return removed, tx.Commit()
}

// ---------------------------------------------------------------------------
// Event queue

const eventColumns = `id, payload, status, enqueued, attempts, next_attempt, last_error`

func (s *Store) EnqueueEvent(ctx context.Context, e store.QueuedEvent) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO events (`+eventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`),
		e.ID, string(e.Payload), string(e.Status), e.Enqueued.UnixMicro(), e.Attempts, e.NextAttempt.UnixMicro(), e.LastError)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanEvent(row scanner) (store.QueuedEvent, error) {
	var e store.QueuedEvent
	var payload, status string
	var enqueued, next int64
	err := row.Scan(&e.ID, &payload, &status, &enqueued, &e.Attempts, &next, &e.LastError)
	e.Payload, e.Status = []byte(payload), store.EventStatus(status)
	e.Enqueued, e.NextAttempt = time.UnixMicro(enqueued), time.UnixMicro(next)
	return e, err
}

func (s *Store) listEvents(ctx context.Context, query string, args ...any) ([]store.QueuedEvent, error) {
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.QueuedEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DueEvents(ctx context.Context, now time.Time, limit int) ([]store.QueuedEvent, error) {
	return s.listEvents(ctx, `SELECT `+eventColumns+` FROM events
		WHERE status = ? AND next_attempt <= ? ORDER BY enqueued, id LIMIT ?`,
		string(store.EventPending), now.UnixMicro(), limit)
}

func (s *Store) ParkedEvents(ctx context.Context, limit int) ([]store.QueuedEvent, error) {
	return s.listEvents(ctx, `SELECT `+eventColumns+` FROM events
		WHERE status = ? ORDER BY enqueued, id LIMIT ?`, string(store.EventParked), limit)
}

func (s *Store) UpdateEvent(ctx context.Context, e store.QueuedEvent) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE events SET status = ?, attempts = ?, next_attempt = ?, last_error = ? WHERE id = ?`),
		string(e.Status), e.Attempts, e.NextAttempt.UnixMicro(), e.LastError, e.ID)
	return err
}

func (s *Store) DeleteEvent(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM events WHERE id = ?`), id)
	return err
}

func (s *Store) GetEvent(ctx context.Context, id string) (*store.QueuedEvent, error) {
	e, err := scanEvent(s.db.QueryRowContext(ctx, s.q(`SELECT `+eventColumns+` FROM events WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) CountEvents(ctx context.Context) (pending, parked int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM events GROUP BY status`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return 0, 0, err
		}
		if store.EventStatus(status) == store.EventParked {
			parked += n
		} else {
			pending += n
		}
	}
	return pending, parked, rows.Err()
}
