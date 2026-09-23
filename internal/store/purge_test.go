package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/memstore"
)

type recordingStore struct {
	*memstore.Store
	mu      sync.Mutex
	cutoffs []time.Time
}

func (r *recordingStore) Purge(ctx context.Context, before time.Time) (int, error) {
	r.mu.Lock()
	r.cutoffs = append(r.cutoffs, before)
	r.mu.Unlock()
	return r.Store.Purge(ctx, before)
}

func (r *recordingStore) calls() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.cutoffs...)
}

func TestRunPurgerPurgesAtStartAndOnSchedule(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s := &recordingStore{Store: memstore.New(store.Options{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		store.RunPurger(ctx, s, 30*24*time.Hour, 10*time.Millisecond, func() time.Time { return now })
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for len(s.calls()) < 3 {
		select {
		case <-deadline:
			t.Fatalf("purge ran %d times", len(s.calls()))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if want := now.Add(-30 * 24 * time.Hour); !s.calls()[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", s.calls()[0], want)
	}
}

func TestRunPurgerDisabledWithZeroRetention(t *testing.T) {
	s := &recordingStore{Store: memstore.New(store.Options{})}
	store.RunPurger(context.Background(), s, 0, time.Millisecond, nil) // returns immediately
	if len(s.calls()) != 0 {
		t.Errorf("purge ran with retention 0")
	}
}

func TestOpenUnknownDriver(t *testing.T) {
	if _, err := store.Open(context.Background(), "nope", store.Options{}); err == nil {
		t.Error("expected an error for an unknown driver")
	}
}
