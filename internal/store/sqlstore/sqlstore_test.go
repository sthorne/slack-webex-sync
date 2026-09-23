package sqlstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/storetest"
)

func TestSQLite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		opts.DSN = filepath.Join(t.TempDir(), "test.db")
		s, err := OpenSQLite(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// Set SWS_TEST_POSTGRES_DSN to run the suite against PostgreSQL.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("SWS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SWS_TEST_POSTGRES_DSN not set")
	}
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		opts.DSN = dsn
		s, err := OpenPostgres(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"message_links", "reaction_notes", "webex_reactions", "kv"} {
			if _, err := s.(*Store).DB().Exec("DELETE FROM " + table); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestPlaceholderRewrite(t *testing.T) {
	s := &Store{numbered: true}
	if got := s.q("a = ? AND b = ?"); got != "a = $1 AND b = $2" {
		t.Errorf("q = %q", got)
	}
}
