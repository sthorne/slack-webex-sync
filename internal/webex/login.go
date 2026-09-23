package webex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// AuthorizeURL builds the URL a person opens to grant the integration access.
func AuthorizeURL(clientID, redirectURI, scopes, state string) string {
	q := url.Values{
		"client_id":     {clientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {scopes},
		"state":         {state},
	}
	return APIBase + "authorize?" + q.Encode()
}

// Login runs the OAuth authorization-code flow. It serves redirectURI
// locally, calls show with the URL to open (signed in as the service
// account), and returns the tokens once the browser is redirected back.
func Login(ctx context.Context, client *Client, clientID, redirectURI, scopes string, show func(string)) (Tokens, error) {
	redirect, err := url.Parse(redirectURI)
	if err != nil {
		return Tokens{}, fmt.Errorf("parse redirect_uri: %w", err)
	}
	if redirect.Scheme != "http" {
		return Tokens{}, errors.New("redirect_uri must be a local http:// URL, e.g. http://localhost:8765/callback")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Tokens{}, err
	}
	state := hex.EncodeToString(nonce)

	listener, err := net.Listen("tcp", redirect.Host)
	if err != nil {
		return Tokens{}, fmt.Errorf("listen on %s: %w", redirect.Host, err)
	}
	type result struct {
		tokens Tokens
		err    error
	}
	results := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(redirect.Path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			results <- result{err: fmt.Errorf("authorization failed: %s", e)}
			return
		}
		tok, err := client.ExchangeCode(r.Context(), q.Get("code"), redirectURI)
		if err != nil {
			http.Error(w, "token exchange failed; see terminal", http.StatusBadGateway)
			results <- result{err: err}
			return
		}
		fmt.Fprintln(w, "Webex authorization complete. You can close this tab.")
		results <- result{tokens: tok}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	show(AuthorizeURL(clientID, redirectURI, scopes, state))
	select {
	case <-ctx.Done():
		return Tokens{}, ctx.Err()
	case res := <-results:
		return res.tokens, res.err
	}
}
