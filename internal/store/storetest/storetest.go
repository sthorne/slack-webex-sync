// Package storetest is the conformance suite for store backends. Every
// backend runs it from its own tests:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
//			s, err := Open(ctx, opts) // a fresh, empty store
//			...
//			return s
//		})
//	}
package storetest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/model"
	"github.com/sthorne/slack-webex-sync/internal/store"
)

// Retention is the retention the suite configures; Purge tests advance the
// clock past it.
const Retention = 24 * time.Hour

// Opener returns a fresh, empty store configured with opts. It should
// register cleanup with t.
type Opener func(t *testing.T, opts store.Options) store.Store

// Clock is a manually advanced clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts a clock at a fixed time, truncated to the second, since
// some backends store whole seconds.
func NewClock() *Clock {
	return &Clock{now: time.Now().Truncate(time.Second)}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Run checks every point of the store.Store contract.
func Run(t *testing.T, open Opener) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store, clock *Clock)
	}{
		{"Links", testLinks},
		{"LinkUpsert", testLinkUpsert},
		{"LinkDelete", testLinkDelete},
		{"ReactionNotes", testReactionNotes},
		{"ReactionNoteTakenOnce", testReactionNoteTakenOnce},
		{"WebexReactions", testWebexReactions},
		{"WebexReactionsConcurrent", testWebexReactionsConcurrent},
		{"Values", testValues},
		{"Purge", testPurge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := NewClock()
			s := open(t, store.Options{Retention: Retention, Now: clock.Now})
			tt.fn(t, s, clock)
		})
	}
}

func ctx() context.Context { return context.Background() }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equalLink(t *testing.T, what string, got *store.Link, want store.Link) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: link not found, want %+v", what, want)
	}
	if *got != want {
		t.Fatalf("%s:\n got %+v\nwant %+v", what, *got, want)
	}
}

func absent[T any](t *testing.T, what string, got *T, err error) {
	t.Helper()
	must(t, err)
	if got != nil {
		t.Fatalf("%s: expected nothing, got %+v", what, *got)
	}
}

func testLinks(t *testing.T, s store.Store, _ *Clock) {
	root := store.Link{Pairing: "eng", SlackTS: "1.1", WebexID: "W1", Origin: model.Slack}
	reply := store.Link{Pairing: "eng", SlackTS: "1.2", WebexID: "W2", Origin: model.Webex, SlackThreadTS: "1.1", WebexParentID: "W1"}
	// Same ts in another pairing (another channel) is a different message.
	other := store.Link{Pairing: "ops", SlackTS: "1.1", WebexID: "W3", Origin: model.Slack}
	for _, l := range []store.Link{root, reply, other} {
		must(t, s.PutLink(ctx(), l))
	}

	got, err := s.LinkBySlack(ctx(), "eng", "1.2")
	must(t, err)
	equalLink(t, "LinkBySlack(reply)", got, reply)
	got, err = s.LinkByWebex(ctx(), "W1")
	must(t, err)
	equalLink(t, "LinkByWebex(root)", got, root)
	got, err = s.LinkBySlack(ctx(), "ops", "1.1")
	must(t, err)
	equalLink(t, "LinkBySlack(other pairing)", got, other)

	got, err = s.LinkBySlack(ctx(), "eng", "9.9")
	absent(t, "LinkBySlack(missing)", got, err)
	got, err = s.LinkByWebex(ctx(), "missing")
	absent(t, "LinkByWebex(missing)", got, err)
}

func testLinkUpsert(t *testing.T, s store.Store, _ *Clock) {
	first := store.Link{Pairing: "eng", SlackTS: "1.1", WebexID: "W1", Origin: model.Slack}
	must(t, s.PutLink(ctx(), first))
	updated := first
	updated.WebexParentID = "W0"
	updated.SlackThreadTS = "1.0"
	must(t, s.PutLink(ctx(), updated))
	got, err := s.LinkBySlack(ctx(), "eng", "1.1")
	must(t, err)
	equalLink(t, "after upsert", got, updated)
	got, err = s.LinkByWebex(ctx(), "W1")
	must(t, err)
	equalLink(t, "LinkByWebex after upsert", got, updated)
}

func testLinkDelete(t *testing.T, s store.Store, _ *Clock) {
	l := store.Link{Pairing: "eng", SlackTS: "1.1", WebexID: "W1", Origin: model.Slack}
	must(t, s.PutLink(ctx(), l))
	must(t, s.DeleteLink(ctx(), l))
	got, err := s.LinkBySlack(ctx(), "eng", "1.1")
	absent(t, "LinkBySlack after delete", got, err)
	got, err = s.LinkByWebex(ctx(), "W1")
	absent(t, "LinkByWebex after delete", got, err)
	must(t, s.DeleteLink(ctx(), l)) // deleting again is fine
}

func testReactionNotes(t *testing.T, s store.Store, _ *Clock) {
	note := store.ReactionNote{Pairing: "eng", SlackTS: "1.1", User: "U1", Reaction: "+1", WebexID: "N1"}
	must(t, s.PutReactionNote(ctx(), note))
	replaced := note
	replaced.WebexID = "N2"
	must(t, s.PutReactionNote(ctx(), replaced))
	// A different reaction by the same user is a separate note.
	must(t, s.PutReactionNote(ctx(), store.ReactionNote{Pairing: "eng", SlackTS: "1.1", User: "U1", Reaction: "tada", WebexID: "N3"}))

	got, err := s.TakeReactionNote(ctx(), "eng", "1.1", "U1", "+1")
	must(t, err)
	if got == nil || *got != replaced {
		t.Fatalf("TakeReactionNote = %+v, want %+v", got, replaced)
	}
	got, err = s.TakeReactionNote(ctx(), "eng", "1.1", "U1", "+1")
	absent(t, "second take", got, err)
	got, err = s.TakeReactionNote(ctx(), "eng", "1.1", "U1", "tada")
	must(t, err)
	if got == nil || got.WebexID != "N3" {
		t.Fatalf("other note = %+v", got)
	}
}

func testReactionNoteTakenOnce(t *testing.T, s store.Store, _ *Clock) {
	must(t, s.PutReactionNote(ctx(), store.ReactionNote{Pairing: "eng", SlackTS: "1.1", User: "U1", Reaction: "+1", WebexID: "N1"}))
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := s.TakeReactionNote(ctx(), "eng", "1.1", "U1", "+1"); err == nil && n != nil {
				wins.Add(1)
			} else if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("note taken %d times, want exactly 1", wins.Load())
	}
}

func testWebexReactions(t *testing.T, s store.Store, _ *Clock) {
	add := func(activity, person, reaction string) int {
		t.Helper()
		n, err := s.AddWebexReaction(ctx(), store.WebexReaction{ActivityID: activity, WebexID: "W1", PersonID: person, Reaction: reaction})
		must(t, err)
		return n
	}
	if n := add("A1", "P1", "heart"); n != 1 {
		t.Errorf("first add = %d, want 1", n)
	}
	if n := add("A1", "P1", "heart"); n != 1 {
		t.Errorf("duplicate add = %d, want 1", n)
	}
	if n := add("A2", "P2", "heart"); n != 2 {
		t.Errorf("second person = %d, want 2", n)
	}
	if n := add("A3", "P1", "thumbsup"); n != 1 {
		t.Errorf("other reaction = %d, want 1", n)
	}

	r, remaining, err := s.RemoveWebexReaction(ctx(), "A1")
	must(t, err)
	want := store.WebexReaction{ActivityID: "A1", WebexID: "W1", PersonID: "P1", Reaction: "heart"}
	if r == nil || *r != want || remaining != 1 {
		t.Fatalf("remove A1 = %+v, %d; want %+v, 1", r, remaining, want)
	}
	if _, remaining, _ = s.RemoveWebexReaction(ctx(), "A2"); remaining != 0 {
		t.Errorf("remaining after last removal = %d", remaining)
	}
	r, remaining, err = s.RemoveWebexReaction(ctx(), "A2")
	must(t, err)
	if r != nil || remaining != 0 {
		t.Errorf("removing twice = %+v, %d", r, remaining)
	}
	if n := add("A1", "P1", "heart"); n != 1 {
		t.Errorf("re-adding a removed activity = %d, want 1", n)
	}
}

func testWebexReactionsConcurrent(t *testing.T, s store.Store, _ *Clock) {
	const n = 10
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AddWebexReaction(ctx(), store.WebexReaction{
				ActivityID: fmt.Sprintf("A%d", i), WebexID: "W1", PersonID: fmt.Sprintf("P%d", i), Reaction: "heart",
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	count, err := s.AddWebexReaction(ctx(), store.WebexReaction{ActivityID: "A0", WebexID: "W1", PersonID: "P0", Reaction: "heart"})
	must(t, err)
	if count != n {
		t.Fatalf("count after %d concurrent adds = %d", n, count)
	}
}

func testValues(t *testing.T, s store.Store, _ *Clock) {
	v, err := s.GetValue(ctx(), "k")
	must(t, err)
	if v != "" {
		t.Fatalf("unset value = %q", v)
	}
	must(t, s.SetValue(ctx(), "k", "1"))
	must(t, s.SetValue(ctx(), "k", "2"))
	if v, _ := s.GetValue(ctx(), "k"); v != "2" {
		t.Fatalf("value = %q, want 2", v)
	}
}

func testPurge(t *testing.T, s store.Store, clock *Clock) {
	old := store.Link{Pairing: "eng", SlackTS: "1.1", WebexID: "W-old", Origin: model.Slack}
	must(t, s.PutLink(ctx(), old))
	must(t, s.PutReactionNote(ctx(), store.ReactionNote{Pairing: "eng", SlackTS: "1.1", User: "U1", Reaction: "+1", WebexID: "N-old"}))
	_, err := s.AddWebexReaction(ctx(), store.WebexReaction{ActivityID: "A-old", WebexID: "W-old", PersonID: "P1", Reaction: "heart"})
	must(t, err)
	must(t, s.SetValue(ctx(), "token", "keep"))

	clock.Advance(Retention + time.Hour)

	fresh := store.Link{Pairing: "eng", SlackTS: "2.2", WebexID: "W-new", Origin: model.Webex}
	must(t, s.PutLink(ctx(), fresh))
	must(t, s.PutReactionNote(ctx(), store.ReactionNote{Pairing: "eng", SlackTS: "2.2", User: "U1", Reaction: "+1", WebexID: "N-new"}))
	_, err = s.AddWebexReaction(ctx(), store.WebexReaction{ActivityID: "A-new", WebexID: "W-new", PersonID: "P1", Reaction: "heart"})
	must(t, err)

	if _, err := s.Purge(ctx(), clock.Now().Add(-Retention)); err != nil {
		t.Fatal(err)
	}

	link, err := s.LinkBySlack(ctx(), "eng", "1.1")
	absent(t, "purged LinkBySlack", link, err)
	link, err = s.LinkByWebex(ctx(), "W-old")
	absent(t, "purged LinkByWebex", link, err)
	note, err := s.TakeReactionNote(ctx(), "eng", "1.1", "U1", "+1")
	absent(t, "purged note", note, err)
	reaction, _, err := s.RemoveWebexReaction(ctx(), "A-old")
	absent(t, "purged reaction", reaction, err)
	if n, err := s.AddWebexReaction(ctx(), store.WebexReaction{ActivityID: "A-old2", WebexID: "W-old", PersonID: "P2", Reaction: "heart"}); err != nil || n != 1 {
		t.Errorf("count after purge = %d, %v; purged reactions must not be counted", n, err)
	}

	link, err = s.LinkBySlack(ctx(), "eng", "2.2")
	must(t, err)
	equalLink(t, "fresh link survives", link, fresh)
	link, err = s.LinkByWebex(ctx(), "W-new")
	must(t, err)
	equalLink(t, "fresh link by webex survives", link, fresh)
	if note, _ := s.TakeReactionNote(ctx(), "eng", "2.2", "U1", "+1"); note == nil {
		t.Error("fresh note was purged")
	}
	if r, _, _ := s.RemoveWebexReaction(ctx(), "A-new"); r == nil {
		t.Error("fresh reaction was purged")
	}
	if v, _ := s.GetValue(ctx(), "token"); v != "keep" {
		t.Errorf("values must never be purged, got %q", v)
	}
}
