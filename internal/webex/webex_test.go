package webex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

func TestIDs(t *testing.T) {
	room := EncodeID("us", "ROOM", "bbceb1ad-43f1-3b58-9147-f14bb0c4d154")
	if room != "Y2lzY29zcGFyazovL3VzL1JPT00vYmJjZWIxYWQtNDNmMS0zYjU4LTkxNDctZjE0YmIwYzRkMTU0" {
		t.Errorf("EncodeID = %s", room)
	}
	id, ok := DecodeID(room)
	if !ok || id.Cluster != "us" || id.Type != "ROOM" || id.UUID != "bbceb1ad-43f1-3b58-9147-f14bb0c4d154" {
		t.Errorf("DecodeID = %+v, %v", id, ok)
	}
	// Some clusters are URNs rather than a short region name.
	odd := EncodeID("urn:TEAM:us-west-2_r", "MESSAGE", "abc")
	if id, ok := DecodeID(odd); !ok || id.Cluster != "urn:TEAM:us-west-2_r" || id.UUID != "abc" {
		t.Errorf("DecodeID(odd) = %+v", id)
	}
	if UUIDOf("plain-uuid") != "plain-uuid" {
		t.Error("UUIDOf should pass bare uuids through")
	}
}

func TestTruncateMarkdown(t *testing.T) {
	long := strings.Repeat("é", MaxMessageBytes)
	got := TruncateMarkdown(long)
	if len(got) > MaxMessageBytes || !strings.HasSuffix(got, truncatedNote) {
		t.Errorf("len=%d suffix ok=%v", len(got), strings.HasSuffix(got, truncatedNote))
	}
	if !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Error("truncation split a rune")
	}
}

const roomUUID = "11111111-1111-1111-1111-111111111111"
const selfUUID = "99999999-9999-9999-9999-999999999999"

func frame(verb, actor string, extra map[string]any) []byte {
	activity := map[string]any{
		"id":     "act-1",
		"verb":   verb,
		"actor":  map[string]any{"id": actor, "entryUUID": actor},
		"target": map[string]any{"id": roomUUID, "objectType": "conversation"},
	}
	for k, v := range extra {
		activity[k] = v
	}
	data, _ := json.Marshal(map[string]any{
		"id":   "mercury-1",
		"data": map[string]any{"eventType": "conversation.activity", "activity": activity},
	})
	return data
}

func TestTranslate(t *testing.T) {
	room := EncodeID("us", "ROOM", roomUUID)
	l := NewListener(nil, "", []string{room}, EncodeID("us", "PEOPLE", selfUUID))
	msgID := func(u string) string { return EncodeID("us", "MESSAGE", u) }

	tests := []struct {
		name  string
		frame []byte
		want  []model.WebexEvent
	}{
		{"post", frame("post", "p1", nil),
			[]model.WebexEvent{{Kind: model.WebexMessageCreated, RoomID: room, ActorID: "p1", MessageID: msgID("act-1")}}},
		{"share", frame("share", "p1", nil),
			[]model.WebexEvent{{Kind: model.WebexMessageCreated, RoomID: room, ActorID: "p1", MessageID: msgID("act-1")}}},
		{"edit", frame("post", "p1", map[string]any{"parent": map[string]any{"id": "orig", "type": "edit"}}),
			[]model.WebexEvent{{Kind: model.WebexMessageUpdated, RoomID: room, ActorID: "p1", MessageID: msgID("orig")}}},
		{"reply is a post", frame("post", "p1", map[string]any{"parent": map[string]any{"id": "root", "type": "reply"}}),
			[]model.WebexEvent{{Kind: model.WebexMessageCreated, RoomID: room, ActorID: "p1", MessageID: msgID("act-1")}}},
		{"delete", frame("delete", "p1", map[string]any{"object": map[string]any{"id": "gone", "objectType": "activity"}}),
			[]model.WebexEvent{{Kind: model.WebexDeleted, RoomID: room, ActorID: "p1", MessageID: msgID("gone"), ActivityID: "gone"}}},
		{"reaction", frame("add", "p1", map[string]any{
			"object": map[string]any{"objectType": "reaction2", "displayName": "thumbsup"},
			"parent": map[string]any{"id": "target", "type": "reaction"},
		}), []model.WebexEvent{{Kind: model.WebexReactionAdded, RoomID: room, ActorID: "p1", MessageID: msgID("target"), ActivityID: "act-1", Reaction: "thumbsup"}}},
		{"own activity ignored", frame("post", selfUUID, nil), nil},
		{"other room ignored", []byte(strings.Replace(string(frame("post", "p1", nil)), roomUUID, "other", 1)), nil},
		{"non-activity ignored", []byte(`{"id":"m","data":{"eventType":"status.start_typing"}}`), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ack, got := l.translate(tt.frame)
			if ack == "" {
				t.Error("frame was not acknowledged")
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// fakeAPI is a tiny Webex API for client tests.
type fakeAPI struct {
	mu        sync.Mutex
	validTok  string
	refreshes int
	posts     []map[string]string
	messages  []model.WebexMessage
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh-1" || r.Form.Get("client_secret") != "secret" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.refreshes++
		f.validTok = fmt.Sprintf("access-%d", f.refreshes)
		tok := f.validTok
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": 1209600, "refresh_token": "refresh-1"})
	})
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			ok := r.Header.Get("Authorization") == "Bearer "+f.validTok
			f.mu.Unlock()
			if !ok {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /people/me", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ME","displayName":"Bridge","emails":["bridge@example.com"],"type":"person"}`))
	}))
	mux.HandleFunc("POST /messages", auth(func(w http.ResponseWriter, r *http.Request) {
		fields := map[string]string{}
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType == "multipart/form-data" {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				if part.FileName() != "" {
					fields["file:"+part.FileName()] = string(data)
					continue
				}
				fields[part.FormName()] = string(data)
			}
		} else {
			_ = json.NewDecoder(r.Body).Decode(&fields)
		}
		f.mu.Lock()
		f.posts = append(f.posts, fields)
		n := len(f.posts)
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":"M%d"}`, n)
	}))
	mux.HandleFunc("GET /messages", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"items": f.messages})
	}))
	mux.HandleFunc("GET /contents/1", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="report.pdf"`)
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF"))
	}))
	return mux
}

func newTestClient(t *testing.T) (*Client, *fakeAPI, *[]Tokens) {
	api := &fakeAPI{}
	server := httptest.NewServer(api.handler(t))
	t.Cleanup(server.Close)
	var saved []Tokens
	client := NewClient(Options{
		ClientID:     "id",
		ClientSecret: "secret",
		Tokens:       Tokens{AccessToken: "stale", RefreshToken: "refresh-1"},
		OnRefresh:    func(tok Tokens) { saved = append(saved, tok) },
		BaseURL:      server.URL + "/",
	})
	return client, api, &saved
}

func TestClientRefreshesExpiredToken(t *testing.T) {
	client, api, saved := newTestClient(t)
	me, err := client.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if me.ID != "ME" || me.Email != "bridge@example.com" || me.IsBot {
		t.Errorf("me = %+v", me)
	}
	if api.refreshes != 1 || len(*saved) != 1 || (*saved)[0].AccessToken != "access-1" {
		t.Errorf("refreshes=%d saved=%+v", api.refreshes, *saved)
	}
	// The new token is reused.
	if _, err := client.Me(context.Background()); err != nil || api.refreshes != 1 {
		t.Errorf("second call: err=%v refreshes=%d", err, api.refreshes)
	}
}

func TestPostMessage(t *testing.T) {
	client, api, _ := newTestClient(t)
	ctx := context.Background()
	id, err := client.PostMessage(ctx, "R1", "**hi**", "P1", nil)
	if err != nil || id != "M1" {
		t.Fatalf("post = %q, %v", id, err)
	}
	want := map[string]string{"roomId": "R1", "markdown": "**hi**", "parentId": "P1"}
	if fmt.Sprint(api.posts[0]) != fmt.Sprint(want) {
		t.Errorf("json post = %v", api.posts[0])
	}

	file := &model.Attachment{Filename: "a.txt", ContentType: "text/plain", Content: []byte("hello")}
	if _, err := client.PostMessage(ctx, "R1", "", "", file); err != nil {
		t.Fatal(err)
	}
	want = map[string]string{"roomId": "R1", "file:a.txt": "hello"}
	if fmt.Sprint(api.posts[1]) != fmt.Sprint(want) {
		t.Errorf("multipart post = %v", api.posts[1])
	}
}

func TestDownloadFile(t *testing.T) {
	client, _, _ := newTestClient(t)
	a, err := client.DownloadFile(context.Background(), client.base+"contents/1", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if a.Filename != "report.pdf" || a.ContentType != "application/pdf" || string(a.Content) != "%PDF" {
		t.Errorf("attachment = %+v", a)
	}
	if _, err := client.DownloadFile(context.Background(), client.base+"contents/1", 2); err != ErrTooLarge {
		t.Errorf("size limit: err = %v", err)
	}
}

func TestPoller(t *testing.T) {
	client, api, _ := newTestClient(t)
	p := NewPoller(client, []string{"R1"}, "ME")
	start := p.lastCreated["R1"]
	at := func(d time.Duration) string { return start.Add(d).UTC().Format(time.RFC3339Nano) }

	api.messages = []model.WebexMessage{ // newest first, as the API returns them
		{ID: "new2", PersonID: "P1", Created: at(2 * time.Second)},
		{ID: "mine", PersonID: "ME", Created: at(1500 * time.Millisecond)},
		{ID: "new1", PersonID: "P1", Created: at(time.Second)},
		{ID: "edited", PersonID: "P1", Created: at(-time.Hour), Updated: at(time.Second)},
		{ID: "old", PersonID: "P1", Created: at(-time.Hour)},
	}
	var got []string
	sink := func(ev model.WebexEvent) error {
		got = append(got, fmt.Sprintf("%d:%s", ev.Kind, ev.MessageID))
		return nil
	}
	p.PollOnce(context.Background(), sink)
	want := []string{
		fmt.Sprintf("%d:edited", model.WebexMessageUpdated),
		fmt.Sprintf("%d:new1", model.WebexMessageCreated),
		fmt.Sprintf("%d:new2", model.WebexMessageCreated),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("first poll = %v, want %v", got, want)
	}

	got = nil
	p.PollOnce(context.Background(), sink)
	if len(got) != 0 {
		t.Errorf("second poll replayed %v", got)
	}
	if p.LastSuccess().IsZero() {
		t.Error("LastSuccess not recorded")
	}
}

func TestPollerRetriesWhenQueueingFails(t *testing.T) {
	client, api, _ := newTestClient(t)
	p := NewPoller(client, []string{"R1"}, "ME")
	api.messages = []model.WebexMessage{
		{ID: "new1", PersonID: "P1", Created: p.lastCreated["R1"].Add(time.Second).UTC().Format(time.RFC3339Nano)},
	}
	failing := func(model.WebexEvent) error { return fmt.Errorf("store down") }
	p.PollOnce(context.Background(), failing)
	if !p.LastSuccess().IsZero() {
		t.Error("a failed poll was recorded as a success")
	}
	var got []string
	p.PollOnce(context.Background(), func(ev model.WebexEvent) error {
		got = append(got, ev.MessageID)
		return nil
	})
	if fmt.Sprint(got) != "[new1]" {
		t.Errorf("message lost after a failed queue attempt: %v", got)
	}
}
