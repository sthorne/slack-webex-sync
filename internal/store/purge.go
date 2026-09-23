package store

import (
	"context"
	"log/slog"
	"time"
)

// RunPurger deletes records older than retention: once at startup, then
// every interval, until ctx is cancelled. A zero retention disables it.
func RunPurger(ctx context.Context, s Store, retention, interval time.Duration, now func() time.Time) {
	if retention <= 0 {
		slog.Info("link retention disabled; links are kept forever")
		return
	}
	if now == nil {
		now = time.Now
	}
	purge := func() {
		cutoff := now().Add(-retention)
		n, err := s.Purge(ctx, cutoff)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("purge failed", "err", err)
		case n > 0:
			slog.Info("purged expired links", "removed", n, "older_than", cutoff.Format(time.RFC3339))
		}
	}
	purge()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}
