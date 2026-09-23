package mongostore

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/storetest"
)

// Set SWS_TEST_MONGODB_URI (e.g. mongodb://localhost:27017) to run the suite.
// Each test gets its own database, dropped afterwards. Set
// SWS_TEST_MONGODB_TTL=false for servers without TTL index support.
var dbSeq atomic.Int64

func uri(t *testing.T) (string, bool) {
	u := os.Getenv("SWS_TEST_MONGODB_URI")
	if u == "" {
		t.Skip("SWS_TEST_MONGODB_URI not set")
	}
	ttl := true
	if v := os.Getenv("SWS_TEST_MONGODB_TTL"); v != "" {
		ttl, _ = strconv.ParseBool(v)
	}
	return u, ttl
}

func openTest(t *testing.T, opts store.Options) *Store {
	base, ttl := uri(t)
	name := fmt.Sprintf("sws_test_%d_%d", time.Now().UnixNano(), dbSeq.Add(1))
	opts.DSN = fmt.Sprintf("%s/%s?ttl_index=%t", base, name, ttl)
	s, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ms := s.(*Store)
	t.Cleanup(func() {
		_ = ms.links.Database().Drop(context.Background())
		ms.Close()
	})
	return ms
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		return openTest(t, opts)
	})
}

func ttlSeconds(t *testing.T, s *Store) any {
	t.Helper()
	cursor, err := s.links.Indexes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var indexes []bson.M
	if err := cursor.All(context.Background(), &indexes); err != nil {
		t.Fatal(err)
	}
	for _, ix := range indexes {
		if ix["name"] == ttlIndexName {
			return ix["expireAfterSeconds"]
		}
	}
	return nil
}

func TestTTLIndexFollowsRetention(t *testing.T) {
	if _, ttl := uri(t); !ttl {
		t.Skip("server without TTL index support")
	}
	s := openTest(t, store.Options{Retention: 30 * 24 * time.Hour})
	if got := toInt64(ttlSeconds(t, s)); got != 30*24*3600 {
		t.Fatalf("expireAfterSeconds = %v", got)
	}
	ctx := context.Background()
	db := s.links.Database()
	if _, err := New(ctx, s.client, db.Name(), true, store.Options{Retention: 60 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := toInt64(ttlSeconds(t, s)); got != 60*24*3600 {
		t.Fatalf("after retention change, expireAfterSeconds = %v", got)
	}
	if _, err := New(ctx, s.client, db.Name(), true, store.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := ttlSeconds(t, s); got != nil {
		t.Fatalf("retention 0 should drop the TTL index, found %v", got)
	}
}
