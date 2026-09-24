package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sthorne/slack-webex-sync/internal/config"
	"github.com/sthorne/slack-webex-sync/internal/secret"
	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/webex"
)

const (
	tokensKey = "webex.tokens"
	deviceKey = "webex.device_url"
)

// tokenStore keeps the Webex OAuth tokens in the store, encrypted when a
// key is configured.
type tokenStore struct {
	st  store.Store
	box *secret.Box
}

// load returns saved tokens, or ok=false if none are saved. Tokens saved in
// plain text are re-encrypted when a key is configured.
func (t *tokenStore) load(ctx context.Context) (tok webex.Tokens, ok bool, err error) {
	raw, err := t.st.GetValue(ctx, tokensKey)
	if err != nil || raw == "" {
		return tok, false, err
	}
	plain, err := t.box.Open(raw, tokensKey)
	if err != nil {
		return tok, false, fmt.Errorf("read saved webex tokens: %w", err)
	}
	if err := json.Unmarshal([]byte(plain), &tok); err != nil {
		return tok, false, fmt.Errorf("read saved webex tokens: %w", err)
	}
	if t.box.Enabled() && !secret.IsEncrypted(raw) {
		slog.Info("encrypting webex tokens that were stored in plain text")
		if err := t.save(ctx, tok); err != nil {
			return tok, false, err
		}
	}
	return tok, tok.RefreshToken != "", nil
}

func (t *tokenStore) save(ctx context.Context, tok webex.Tokens) error {
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	sealed, err := t.box.Seal(string(data), tokensKey)
	if err != nil {
		return err
	}
	return t.st.SetValue(ctx, tokensKey, sealed)
}

// webexClient builds a Webex client, preferring tokens saved by an earlier
// refresh or webex-login over the ones in the config file.
func webexClient(ctx context.Context, cfg *config.Config, tokens *tokenStore) (*webex.Client, error) {
	initial := webex.Tokens{AccessToken: cfg.Webex.AccessToken, RefreshToken: cfg.Webex.RefreshToken}
	saved, ok, err := tokens.load(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		initial = saved
	}
	if !tokens.box.Enabled() {
		slog.Warn("webex tokens are stored unencrypted; set storage.encryption_key (see `slack-webex-sync generate-key`)")
	}
	return webex.NewClient(webex.Options{
		ClientID:     cfg.Webex.ClientID,
		ClientSecret: cfg.Webex.ClientSecret,
		Tokens:       initial,
		OnRefresh: func(t webex.Tokens) {
			if err := tokens.save(context.Background(), t); err != nil {
				slog.Error("could not save refreshed webex tokens", "err", err)
			}
		},
	}), nil
}
