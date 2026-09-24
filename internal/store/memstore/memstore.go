// Package memstore is an in-memory store backend, registered as "memory".
//
// Nothing survives a restart, so it suits tests and trial runs. After a
// restart, edits, deletes and replies to earlier messages stop syncing.
package memstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/store"
)

func init() {
	store.Register("memory", func(_ context.Context, opts store.Options) (store.Store, error) {
		return New(opts), nil
	})
}

type linkKey struct{ pairing, ts string }

type noteKey struct{ pairing, ts, user, reaction string }

type stamped[T any] struct {
	value   T
	created time.Time
}

// Store keeps everything in maps guarded by one mutex.
type Store struct {
	now func() time.Time

	mu        sync.Mutex
	links     map[linkKey]stamped[store.Link]
	byWebex   map[string]linkKey
	notes     map[noteKey]stamped[store.ReactionNote]
	reactions map[string]stamped[store.WebexReaction]
	values    map[string]string
	events    map[string]store.QueuedEvent
}

// New creates an empty store.
func New(opts store.Options) *Store {
	return &Store{
		now:       opts.Clock(),
		links:     map[linkKey]stamped[store.Link]{},
		byWebex:   map[string]linkKey{},
		notes:     map[noteKey]stamped[store.ReactionNote]{},
		reactions: map[string]stamped[store.WebexReaction]{},
		values:    map[string]string{},
		events:    map[string]store.QueuedEvent{},
	}
}

func (s *Store) PutLink(_ context.Context, l store.Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := linkKey{l.Pairing, l.SlackTS}
	if old, ok := s.links[key]; ok && old.value.WebexID != l.WebexID {
		delete(s.byWebex, old.value.WebexID)
	}
	s.links[key] = stamped[store.Link]{l, s.now()}
	s.byWebex[l.WebexID] = key
	return nil
}

func (s *Store) LinkBySlack(_ context.Context, pairing, ts string) (*store.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[linkKey{pairing, ts}]; ok {
		return &l.value, nil
	}
	return nil, nil
}

func (s *Store) LinkByWebex(_ context.Context, webexID string) (*store.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key, ok := s.byWebex[webexID]; ok {
		if l, ok := s.links[key]; ok {
			return &l.value, nil
		}
	}
	return nil, nil
}

func (s *Store) DeleteLink(_ context.Context, l store.Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := linkKey{l.Pairing, l.SlackTS}
	if old, ok := s.links[key]; ok {
		delete(s.byWebex, old.value.WebexID)
		delete(s.links, key)
	}
	return nil
}

func (s *Store) PutReactionNote(_ context.Context, n store.ReactionNote) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes[noteKey{n.Pairing, n.SlackTS, n.User, n.Reaction}] = stamped[store.ReactionNote]{n, s.now()}
	return nil
}

func (s *Store) TakeReactionNote(_ context.Context, pairing, slackTS, user, reaction string) (*store.ReactionNote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := noteKey{pairing, slackTS, user, reaction}
	n, ok := s.notes[key]
	if !ok {
		return nil, nil
	}
	delete(s.notes, key)
	return &n.value, nil
}

func (s *Store) count(webexID, reaction string) int {
	n := 0
	for _, r := range s.reactions {
		if r.value.WebexID == webexID && r.value.Reaction == reaction {
			n++
		}
	}
	return n
}

func (s *Store) AddWebexReaction(_ context.Context, r store.WebexReaction) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reactions[r.ActivityID]; !ok {
		s.reactions[r.ActivityID] = stamped[store.WebexReaction]{r, s.now()}
	}
	return s.count(r.WebexID, r.Reaction), nil
}

func (s *Store) RemoveWebexReaction(_ context.Context, activityID string) (*store.WebexReaction, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reactions[activityID]
	if !ok {
		return nil, 0, nil
	}
	delete(s.reactions, activityID)
	return &r.value, s.count(r.value.WebexID, r.value.Reaction), nil
}

func (s *Store) GetValue(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key], nil
}

func (s *Store) SetValue(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

func (s *Store) Purge(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for key, l := range s.links {
		if l.created.Before(before) {
			delete(s.byWebex, l.value.WebexID)
			delete(s.links, key)
			removed++
		}
	}
	for key, n := range s.notes {
		if n.created.Before(before) {
			delete(s.notes, key)
			removed++
		}
	}
	for key, r := range s.reactions {
		if r.created.Before(before) {
			delete(s.reactions, key)
			removed++
		}
	}
	return removed, nil
}

func (s *Store) Close() error { return nil }

// ---------------------------------------------------------------------------
// Event queue

func (s *Store) EnqueueEvent(_ context.Context, e store.QueuedEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.events[e.ID]; !ok {
		e.Payload = append([]byte(nil), e.Payload...)
		s.events[e.ID] = e
	}
	return nil
}

// sorted returns matching events, oldest first.
func (s *Store) sorted(match func(store.QueuedEvent) bool, limit int) []store.QueuedEvent {
	var out []store.QueuedEvent
	for _, e := range s.events {
		if match(e) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Enqueued.Equal(out[j].Enqueued) {
			return out[i].Enqueued.Before(out[j].Enqueued)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Store) DueEvents(_ context.Context, now time.Time, limit int) ([]store.QueuedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sorted(func(e store.QueuedEvent) bool {
		return e.Status == store.EventPending && !e.NextAttempt.After(now)
	}, limit), nil
}

func (s *Store) UpdateEvent(_ context.Context, e store.QueuedEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.events[e.ID]; ok {
		old.Status, old.Attempts, old.NextAttempt, old.LastError = e.Status, e.Attempts, e.NextAttempt, e.LastError
		s.events[e.ID] = old
	}
	return nil
}

func (s *Store) DeleteEvent(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.events, id)
	return nil
}

func (s *Store) GetEvent(_ context.Context, id string) (*store.QueuedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.events[id]; ok {
		return &e, nil
	}
	return nil, nil
}

func (s *Store) ParkedEvents(_ context.Context, limit int) ([]store.QueuedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sorted(func(e store.QueuedEvent) bool { return e.Status == store.EventParked }, limit), nil
}

func (s *Store) CountEvents(_ context.Context) (pending, parked int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Status == store.EventParked {
			parked++
		} else {
			pending++
		}
	}
	return pending, parked, nil
}
