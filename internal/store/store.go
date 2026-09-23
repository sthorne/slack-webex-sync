// Package store defines how the bridge persists the links between mirrored
// messages, independent of the database underneath.
//
// A Link records that a Slack message and a Webex message are copies of each
// other. Links are what let edits, deletions, thread replies and reactions
// follow a message across platforms.
//
// Backends live in subpackages and register themselves by name (see
// Register), the same way database/sql drivers do. Adding a backend means
// writing one package that implements Store, registering it, and passing
// storetest.Run. Nothing else in the application changes.
package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

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

// Store is the persistence contract. Every backend must honor all of it;
// storetest.Run checks each point.
//
// General rules:
//   - All methods are safe for concurrent use.
//   - Reads see every write that has returned (read-your-writes). Eventually
//     consistent reads are not enough.
//   - "Not found" is not an error: lookups return (nil, nil) or ("", nil),
//     and deleting something that does not exist succeeds.
//   - Each record (link, reaction note, Webex reaction) remembers when it was
//     stored, using Options.Now, so Purge can expire it. Values set with
//     SetValue never expire.
//   - Only one bridge process uses a store at a time. Backends need not
//     guard against a second writer, but each method below that says
//     "atomically" must be atomic even so.
type Store interface {
	// PutLink stores a link, replacing any link with the same Pairing and
	// SlackTS. Once it returns, the link must be findable by both
	// LinkBySlack and LinkByWebex. Callers never store two links with the
	// same WebexID.
	PutLink(ctx context.Context, link Link) error
	// LinkBySlack finds a link by its pairing and Slack ts.
	LinkBySlack(ctx context.Context, pairing, ts string) (*Link, error)
	// LinkByWebex finds a link by its Webex message id. Backends without
	// secondary indexes must maintain this lookup themselves.
	LinkByWebex(ctx context.Context, webexID string) (*Link, error)
	// DeleteLink removes a link so that neither lookup finds it.
	DeleteLink(ctx context.Context, link Link) error

	// PutReactionNote stores a note, replacing any note with the same
	// Pairing, SlackTS, User and Reaction.
	PutReactionNote(ctx context.Context, note ReactionNote) error
	// TakeReactionNote atomically removes and returns a note. If two calls
	// race, at most one of them gets the note.
	TakeReactionNote(ctx context.Context, pairing, slackTS, user, reaction string) (*ReactionNote, error)

	// AddWebexReaction records a reaction and returns how many distinct
	// reactions (by ActivityID) now exist with the same WebexID and
	// Reaction, including this one. Adding an ActivityID that is already
	// recorded changes nothing and returns the current count.
	AddWebexReaction(ctx context.Context, r WebexReaction) (int, error)
	// RemoveWebexReaction atomically deletes a reaction by activity id and
	// returns it, together with how many reactions remain with the same
	// WebexID and Reaction. If the activity is not recorded, it returns
	// (nil, 0, nil).
	RemoveWebexReaction(ctx context.Context, activityID string) (*WebexReaction, int, error)

	// GetValue and SetValue hold small settings such as OAuth tokens.
	// Values are never purged.
	GetValue(ctx context.Context, key string) (string, error)
	SetValue(ctx context.Context, key, value string) error

	// Purge deletes links, reaction notes and Webex reactions stored before
	// the cutoff and returns how many it removed. Once Purge returns, those
	// records must no longer be returned by any read. A backend with native
	// expiry may delete lazily and return 0, but it must still hide the
	// records from reads.
	Purge(ctx context.Context, before time.Time) (int, error)

	Close() error
}

// Options configures a backend.
type Options struct {
	// DSN is the backend-specific connection string.
	DSN string
	// Retention is how long records are kept; 0 keeps them forever.
	// Backends with native expiry (TTL attributes or indexes) use it to
	// expire records themselves. Everyone else relies on Purge.
	Retention time.Duration
	// Now returns the current time. Defaults to time.Now; tests override it.
	Now func() time.Time
}

// Clock returns Now, or time.Now if it is unset.
func (o Options) Clock() func() time.Time {
	if o.Now != nil {
		return o.Now
	}
	return time.Now
}

// Factory opens a backend.
type Factory func(ctx context.Context, opts Options) (Store, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a backend available under name. It panics if the name is
// already taken, like database/sql.Register.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("store: Register called twice for backend " + name)
	}
	registry[name] = factory
}

// Drivers lists the registered backend names.
func Drivers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Open connects to a registered backend.
func Open(ctx context.Context, driver string, opts Options) (Store, error) {
	registryMu.RLock()
	factory, ok := registry[driver]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown storage driver %q (available: %v)", driver, Drivers())
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return factory(ctx, opts)
}
