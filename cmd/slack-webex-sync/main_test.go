package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/secret"
	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/memstore"
	"github.com/sthorne/slack-webex-sync/internal/webex"
)

func TestParseArgsAllowsFlagsAnywhere(t *testing.T) {
	opts, pos := parseArgs([]string{"events", "-config", "x.yaml", "retry", "-all"})
	if opts.configPath != "x.yaml" || !opts.all || fmt.Sprint(pos) != "[events retry]" {
		t.Errorf("opts=%+v pos=%v", opts, pos)
	}
	opts, pos = parseArgs(nil)
	if opts.configPath != "config.yaml" || len(pos) != 0 {
		t.Errorf("defaults: opts=%+v pos=%v", opts, pos)
	}
}

func TestTokensEncryptedAndPlainTextMigrated(t *testing.T) {
	ctx := context.Background()
	st := memstore.New(store.Options{})
	tok := webex.Tokens{AccessToken: "a1", RefreshToken: "r1"}

	// Saved without a key: plain text.
	plain := &tokenStore{st: st}
	if err := plain.save(ctx, tok); err != nil {
		t.Fatal(err)
	}
	raw, _ := st.GetValue(ctx, tokensKey)
	if !strings.Contains(raw, "r1") {
		t.Fatalf("expected plain text, got %q", raw)
	}

	// A key is configured later: loading works and re-encrypts.
	key, _ := secret.GenerateKey()
	box, _ := secret.New(key)
	enc := &tokenStore{st: st, box: box}
	got, ok, err := enc.load(ctx)
	if err != nil || !ok || got.RefreshToken != "r1" {
		t.Fatalf("load = %+v, %v, %v", got, ok, err)
	}
	raw, _ = st.GetValue(ctx, tokensKey)
	if !secret.IsEncrypted(raw) || strings.Contains(raw, "r1") {
		t.Fatalf("not re-encrypted: %q", raw)
	}

	// Removing the key afterwards is a clear error, not silent garbage.
	if _, _, err := plain.load(ctx); err == nil || !strings.Contains(err.Error(), "encryption_key") {
		t.Errorf("load without key = %v", err)
	}
}

func TestEventsCommands(t *testing.T) {
	ctx := context.Background()
	st := memstore.New(store.Options{})
	now := time.Now()
	for i, status := range []store.EventStatus{store.EventParked, store.EventParked, store.EventPending} {
		e := store.QueuedEvent{ID: fmt.Sprintf("e%d", i), Payload: []byte(`{"source":"slack","slack":{"Kind":1,"Channel":"C1","Message":{"TS":"1.0"}}}`),
			Status: store.EventPending, Enqueued: now.Add(time.Duration(i) * time.Second), NextAttempt: now}
		_ = st.EnqueueEvent(ctx, e)
		e.Status, e.Attempts = status, 5
		_ = st.UpdateEvent(ctx, e)
	}

	if err := events(ctx, st, []string{"list"}, false); err != nil {
		t.Fatal(err)
	}
	if err := events(ctx, st, []string{"drop", "e2"}, false); err == nil {
		t.Error("dropped a pending event")
	}
	if err := events(ctx, st, []string{"drop", "e0"}, false); err != nil {
		t.Fatal(err)
	}
	if err := events(ctx, st, []string{"retry", "missing"}, false); err == nil {
		t.Error("retrying a missing event should fail")
	}
	if err := events(ctx, st, []string{"retry"}, true); err != nil {
		t.Fatal(err)
	}
	pending, parked, _ := st.CountEvents(ctx)
	if pending != 2 || parked != 0 {
		t.Errorf("after retry -all: %d pending, %d parked", pending, parked)
	}
	if e, _ := st.GetEvent(ctx, "e1"); e == nil || e.Attempts != 0 {
		t.Errorf("requeued event = %+v", e)
	}
}
