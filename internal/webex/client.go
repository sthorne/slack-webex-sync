// Package webex is a small Webex client: the REST API with OAuth token
// refresh, the device websocket for real-time events, and a REST poller
// used as a fallback.
package webex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

const (
	APIBase = "https://webexapis.com/v1/"
	// MaxMessageBytes is the largest message body Webex accepts.
	MaxMessageBytes = 7439
	truncatedNote   = "\n\n_…message truncated_"
)

// APIError is a non-2xx response from the REST API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("webex: HTTP %d: %s", e.Status, e.Body) }

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// Tokens is an OAuth token pair.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// Options configures a Client.
type Options struct {
	ClientID     string
	ClientSecret string
	Tokens       Tokens
	// OnRefresh is called with new tokens so they can be persisted; Webex
	// may rotate the refresh token.
	OnRefresh  func(Tokens)
	HTTPClient *http.Client
	BaseURL    string
	MaxRetries int
}

// Client talks to the Webex REST API as the bridge's service account.
type Client struct {
	opts Options
	http *http.Client
	base string

	mu     sync.Mutex
	tokens Tokens

	peopleMu sync.Mutex
	people   map[string]model.Person
}

// NewClient builds a client. A zero access token is fetched on first use.
func NewClient(opts Options) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.BaseURL == "" {
		opts.BaseURL = APIBase
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 3
	}
	return &Client{
		opts:   opts,
		http:   opts.HTTPClient,
		base:   opts.BaseURL,
		tokens: opts.Tokens,
		people: map[string]model.Person{},
	}
}

// AccessToken returns a valid access token, refreshing it if it is missing
// or about to expire.
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.tokens
	c.mu.Unlock()
	if tok.AccessToken != "" && (tok.Expiry.IsZero() || time.Until(tok.Expiry) > 5*time.Minute) {
		return tok.AccessToken, nil
	}
	if err := c.Refresh(ctx, tok.AccessToken); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens.AccessToken, nil
}

// Refresh exchanges the refresh token for a new access token. stale is the
// token the caller saw fail; if another goroutine already replaced it, no
// request is made.
func (c *Client) Refresh(ctx context.Context, stale string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens.AccessToken != stale && c.tokens.AccessToken != "" {
		return nil
	}
	if c.tokens.RefreshToken == "" {
		return errors.New("webex: access token expired and no refresh token is configured (run webex-login)")
	}
	tok, err := c.exchange(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.tokens.RefreshToken},
	})
	if err != nil {
		return err
	}
	c.tokens = tok
	slog.Info("refreshed Webex access token", "expires", tok.Expiry.Format(time.RFC3339))
	if c.opts.OnRefresh != nil {
		c.opts.OnRefresh(tok)
	}
	return nil
}

// ExchangeCode trades an OAuth authorization code for tokens.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (Tokens, error) {
	tok, err := c.exchange(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	})
	if err != nil {
		return Tokens{}, err
	}
	c.mu.Lock()
	c.tokens = tok
	c.mu.Unlock()
	return tok, nil
}

func (c *Client) exchange(ctx context.Context, form url.Values) (Tokens, error) {
	form.Set("client_id", c.opts.ClientID)
	form.Set("client_secret", c.opts.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return Tokens{}, &APIError{Status: resp.StatusCode, Body: string(body)}
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Tokens{}, err
	}
	tok := Tokens{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = form.Get("refresh_token")
	}
	return tok, nil
}

type requestBody struct {
	contentType string
	data        []byte
}

func jsonBody(v any) (*requestBody, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &requestBody{"application/json", data}, nil
}

// do sends a request, refreshing the token once on 401 and backing off on
// 429 and 5xx responses. target is a path relative to the API base or an
// absolute URL.
func (c *Client) do(ctx context.Context, method, target string, body *requestBody) (*http.Response, error) {
	if !strings.HasPrefix(target, "http") {
		target = c.base + target
	}
	refreshed := false
	for attempt := 0; ; attempt++ {
		token, err := c.AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body.data)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", body.contentType)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshed {
			resp.Body.Close()
			refreshed = true
			if err := c.Refresh(ctx, token); err != nil {
				return nil, err
			}
			continue
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if retryable && attempt < c.opts.MaxRetries {
			resp.Body.Close()
			delay := time.Duration(1<<attempt) * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				delay = time.Duration(s) * time.Second
			}
			slog.Warn("webex request throttled, retrying", "status", resp.StatusCode, "delay", delay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		if resp.StatusCode >= 300 {
			defer resp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, &APIError{Status: resp.StatusCode, Body: string(data)}
		}
		return resp, nil
	}
}

func (c *Client) getJSON(ctx context.Context, target string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// Me returns the service account's own identity.
func (c *Client) Me(ctx context.Context) (model.Person, error) {
	var p person
	if err := c.getJSON(ctx, "people/me", &p); err != nil {
		return model.Person{}, err
	}
	return p.model(), nil
}

type person struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	NickName    string   `json:"nickName"`
	Emails      []string `json:"emails"`
	Avatar      string   `json:"avatar"`
	Type        string   `json:"type"`
}

func (p person) model() model.Person {
	out := model.Person{ID: p.ID, DisplayName: p.DisplayName, AvatarURL: p.Avatar, IsBot: p.Type == "bot"}
	if out.DisplayName == "" {
		out.DisplayName = p.NickName
	}
	if len(p.Emails) > 0 {
		out.Email = p.Emails[0]
	}
	return out
}

// Person looks up (and caches) a person by id.
func (c *Client) Person(ctx context.Context, id string) (model.Person, error) {
	c.peopleMu.Lock()
	cached, ok := c.people[id]
	c.peopleMu.Unlock()
	if ok {
		return cached, nil
	}
	var p person
	if err := c.getJSON(ctx, "people/"+url.PathEscape(id), &p); err != nil {
		return model.Person{ID: id, DisplayName: "Unknown user"}, err
	}
	out := p.model()
	c.peopleMu.Lock()
	c.people[id] = out
	c.peopleMu.Unlock()
	return out, nil
}

// Room returns a space's details.
func (c *Client) Room(ctx context.Context, id string) (map[string]any, error) {
	var room map[string]any
	err := c.getJSON(ctx, "rooms/"+url.PathEscape(id), &room)
	return room, err
}

// GetMessage fetches a message with its decrypted content.
func (c *Client) GetMessage(ctx context.Context, id string) (model.WebexMessage, error) {
	var msg model.WebexMessage
	err := c.getJSON(ctx, "messages/"+url.PathEscape(id), &msg)
	// Ids built from websocket uuids use the standard base64 alphabet; if
	// that was the wrong guess, try the URL-safe spelling.
	if alt := strings.NewReplacer("+", "-", "/", "_").Replace(id); err != nil && alt != id &&
		(IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusBadRequest)) {
		msg = model.WebexMessage{}
		err = c.getJSON(ctx, "messages/"+url.PathEscape(alt), &msg)
	}
	return msg, err
}

// ListMessages returns up to max recent messages in a space, newest first.
func (c *Client) ListMessages(ctx context.Context, roomID string, max int) ([]model.WebexMessage, error) {
	var page struct {
		Items []model.WebexMessage `json:"items"`
	}
	q := url.Values{"roomId": {roomID}, "max": {strconv.Itoa(max)}}
	err := c.getJSON(ctx, "messages?"+q.Encode(), &page)
	return page.Items, err
}

// TruncateMarkdown shortens markdown to fit the Webex size limit.
func TruncateMarkdown(markdown string) string {
	if len(markdown) <= MaxMessageBytes {
		return markdown
	}
	cut := MaxMessageBytes - len(truncatedNote)
	for cut > 0 && !utf8RuneStart(markdown[cut]) {
		cut--
	}
	return markdown[:cut] + truncatedNote
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// PostMessage posts markdown to a space, optionally as a threaded reply and
// with one file attached. It returns the new message id.
func (c *Client) PostMessage(ctx context.Context, roomID, markdown, parentID string, file *model.Attachment) (string, error) {
	markdown = TruncateMarkdown(markdown)
	var body *requestBody
	if file == nil {
		fields := map[string]string{"roomId": roomID, "markdown": markdown}
		if parentID != "" {
			fields["parentId"] = parentID
		}
		var err error
		if body, err = jsonBody(fields); err != nil {
			return "", err
		}
	} else {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("roomId", roomID)
		if markdown != "" {
			_ = w.WriteField("markdown", markdown)
		}
		if parentID != "" {
			_ = w.WriteField("parentId", parentID)
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name": "files", "filename": file.Filename,
		}))
		contentType := file.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		header.Set("Content-Type", contentType)
		part, err := w.CreatePart(header)
		if err != nil {
			return "", err
		}
		if _, err := part.Write(file.Content); err != nil {
			return "", err
		}
		if err := w.Close(); err != nil {
			return "", err
		}
		body = &requestBody{w.FormDataContentType(), buf.Bytes()}
	}
	resp, err := c.do(ctx, http.MethodPost, "messages", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// EditMessage replaces a message's text.
func (c *Client) EditMessage(ctx context.Context, id, roomID, markdown string) error {
	body, err := jsonBody(map[string]string{"roomId": roomID, "markdown": TruncateMarkdown(markdown)})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, "messages/"+url.PathEscape(id), body)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// DeleteMessage deletes a message. A message that is already gone is not an
// error.
func (c *Client) DeleteMessage(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "messages/"+url.PathEscape(id), nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ErrTooLarge is returned by DownloadFile for files over the size limit.
var ErrTooLarge = errors.New("file exceeds the configured size limit")

// DownloadFile fetches a file attached to a message.
func (c *Client) DownloadFile(ctx context.Context, fileURL string, maxBytes int64) (model.Attachment, error) {
	resp, err := c.do(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return model.Attachment{}, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxBytes {
		return model.Attachment{}, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return model.Attachment{}, err
	}
	if int64(len(data)) > maxBytes {
		return model.Attachment{}, ErrTooLarge
	}
	name := path.Base(fileURL)
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return model.Attachment{Filename: name, ContentType: contentType, Content: data}, nil
}
