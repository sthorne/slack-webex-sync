package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

// Set SWS_TEST_POSTGRES_DSN to also run these tests against PostgreSQL.
func backends(t *testing.T) map[string]Store {
	t.Helper()
	ctx := context.Background()
	out := map[string]Store{}
	sqlite, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	out["sqlite"] = sqlite
	if dsn := os.Getenv("SWS_TEST_POSTGRES_DSN"); dsn != "" {
		pg, err := Open(ctx, "postgres", dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"message_links", "reaction_notes", "webex_reactions", "kv"} {
			if _, err := pg.(*sqlStore).db.Exec("DELETE FROM " + table); err != nil {
				t.Fatal(err)
			}
		}
		out["postgres"] = pg
	}
	for _, s := range out {
		t.Cleanup(func() { s.Close() })
	}
	return out
}

func TestLinks(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			link := Link{Pairing: "eng", SlackTS: "1.1", WebexID: "W1", Origin: model.Slack}
			reply := Link{Pairing: "eng", SlackTS: "1.2", WebexID: "W2", Origin: model.Webex, SlackThreadTS: "1.1", WebexParentID: "W1"}
			for _, l := range []Link{link, reply} {
				if err := s.PutLink(ctx, l); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.LinkBySlack(ctx, "eng", "1.2")
			if err != nil || got == nil || *got != reply {
				t.Fatalf("LinkBySlack = %+v, %v", got, err)
			}
			if got.SlackRoot() != "1.1" || got.WebexRoot() != "W1" {
				t.Errorf("roots = %s, %s", got.SlackRoot(), got.WebexRoot())
			}
			got, err = s.LinkByWebex(ctx, "W1")
			if err != nil || got == nil || *got != link {
				t.Fatalf("LinkByWebex = %+v, %v", got, err)
			}
			if got.SlackRoot() != "1.1" || got.WebexRoot() != "W1" {
				t.Errorf("root of a root = %s, %s", got.SlackRoot(), got.WebexRoot())
			}
			if got, _ := s.LinkBySlack(ctx, "other", "1.1"); got != nil {
				t.Errorf("link leaked across pairings: %+v", got)
			}
			if err := s.DeleteLink(ctx, link); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.LinkByWebex(ctx, "W1"); got != nil {
				t.Errorf("deleted link still present")
			}
		})
	}
}

func TestReactionNotes(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			note := ReactionNote{Pairing: "eng", SlackTS: "1.1", User: "U1", Reaction: "+1", WebexID: "N1"}
			if err := s.PutReactionNote(ctx, note); err != nil {
				t.Fatal(err)
			}
			got, err := s.TakeReactionNote(ctx, "eng", "1.1", "U1", "+1")
			if err != nil || got == nil || *got != note {
				t.Fatalf("TakeReactionNote = %+v, %v", got, err)
			}
			if got, _ := s.TakeReactionNote(ctx, "eng", "1.1", "U1", "+1"); got != nil {
				t.Errorf("note taken twice")
			}
		})
	}
}

func TestWebexReactionsAreCounted(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			add := func(activity, person string) int {
				n, err := s.AddWebexReaction(ctx, WebexReaction{ActivityID: activity, WebexID: "W1", PersonID: person, Reaction: "heart"})
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			if n := add("A1", "P1"); n != 1 {
				t.Errorf("first add = %d", n)
			}
			if n := add("A1", "P1"); n != 1 {
				t.Errorf("duplicate add = %d", n)
			}
			if n := add("A2", "P2"); n != 2 {
				t.Errorf("second person = %d", n)
			}
			r, remaining, err := s.RemoveWebexReaction(ctx, "A1")
			if err != nil || r == nil || r.Reaction != "heart" || remaining != 1 {
				t.Fatalf("remove = %+v, %d, %v", r, remaining, err)
			}
			_, remaining, _ = s.RemoveWebexReaction(ctx, "A2")
			if remaining != 0 {
				t.Errorf("remaining = %d", remaining)
			}
			if r, _, _ := s.RemoveWebexReaction(ctx, "missing"); r != nil {
				t.Errorf("removed a missing reaction")
			}
		})
	}
}

func TestValues(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			if v, err := s.GetValue(ctx, "k"); err != nil || v != "" {
				t.Fatalf("empty get = %q, %v", v, err)
			}
			_ = s.SetValue(ctx, "k", "1")
			_ = s.SetValue(ctx, "k", "2")
			if v, _ := s.GetValue(ctx, "k"); v != "2" {
				t.Errorf("get = %q", v)
			}
		})
	}
}

func TestPlaceholderRewrite(t *testing.T) {
	s := &sqlStore{numbered: true}
	if got := s.q("a = ? AND b = ?"); got != "a = $1 AND b = $2" {
		t.Errorf("q = %q", got)
	}
}
