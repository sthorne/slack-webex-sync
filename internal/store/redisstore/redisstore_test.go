package redisstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/storetest"
)

// Runs against an in-process Redis (miniredis). Set SWS_TEST_REDIS_URL to
// run against a real server too; its database is flushed.
func TestMiniredis(t *testing.T) {
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		srv := miniredis.RunT(t)
		opts.DSN = "redis://" + srv.Addr() + "/0?prefix=test:"
		s, err := Open(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestRedis(t *testing.T) {
	url := os.Getenv("SWS_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SWS_TEST_REDIS_URL not set")
	}
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		opts.DSN = url
		s, err := Open(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.(*Store).rdb.FlushDB(context.Background()).Err(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestNativeTTLIsSet(t *testing.T) {
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	s := New(rdb, "p:", store.Options{Retention: time.Hour})
	ctx := context.Background()
	if err := s.PutLink(ctx, store.Link{Pairing: "eng", SlackTS: "1.1", WebexID: "W1"}); err != nil {
		t.Fatal(err)
	}
	if ttl := srv.TTL("p:link:eng\x1f1.1"); ttl != time.Hour {
		t.Errorf("link ttl = %v", ttl)
	}
	if err := s.SetValue(ctx, "token", "x"); err != nil {
		t.Fatal(err)
	}
	if ttl := srv.TTL("p:kv:token"); ttl != 0 {
		t.Errorf("values must not expire, ttl = %v", ttl)
	}

	// After native expiry, Purge still cleans the indexes left behind.
	srv.FastForward(2 * time.Hour)
	if l, _ := s.LinkByWebex(ctx, "W1"); l != nil {
		t.Error("expired link still readable")
	}
	if _, err := s.Purge(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if members, _ := srv.ZMembers("p:idx:links"); len(members) != 0 {
		t.Errorf("index not cleaned: %v", members)
	}
}

func TestExpiredReactionsAreNotCounted(t *testing.T) {
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	s := New(rdb, "p:", store.Options{Retention: time.Hour})
	ctx := context.Background()
	add := func(activity string) int {
		n, err := s.AddWebexReaction(ctx, store.WebexReaction{ActivityID: activity, WebexID: "W1", PersonID: activity, Reaction: "heart"})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	add("A1")
	srv.FastForward(2 * time.Hour) // A1's hash expires natively
	if n := add("A2"); n != 1 {
		t.Errorf("count = %d, want 1 (expired reaction counted)", n)
	}
}
