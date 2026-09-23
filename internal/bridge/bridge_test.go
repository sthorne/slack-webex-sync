package bridge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sthorne/slack-webex-sync/internal/config"
	"github.com/sthorne/slack-webex-sync/internal/model"
	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/memstore"
	"github.com/sthorne/slack-webex-sync/internal/webex"
)

// ---- fakes -----------------------------------------------------------------

type slackPost struct {
	Channel, Text, ThreadTS, Username, IconURL string
}

type fakeSlack struct {
	users     map[string]model.Person
	byEmail   map[string]string
	posts     []slackPost
	updates   map[string]string // ts -> text
	deleted   []string
	reactions map[string]int // "ts:name" -> count
	uploads   []string       // "threadTS:filename"
	files     map[string]model.Attachment
}

func newFakeSlack() *fakeSlack {
	return &fakeSlack{
		users: map[string]model.Person{
			"U1": {ID: "U1", DisplayName: "Ada", Email: "ada@example.com"},
			"U2": {ID: "U2", DisplayName: "Bob", Email: "bob@example.com"},
		},
		byEmail:   map[string]string{"ada@example.com": "U1", "carol@example.com": "U3"},
		updates:   map[string]string{},
		reactions: map[string]int{},
		files:     map[string]model.Attachment{},
	}
}

func (f *fakeSlack) Identity(context.Context) (string, string, error) {
	return "BBRIDGE", "UBRIDGE", nil
}
func (f *fakeSlack) User(_ context.Context, id string) (model.Person, error) {
	if p, ok := f.users[id]; ok {
		return p, nil
	}
	return model.Person{ID: id, DisplayName: id}, fmt.Errorf("user_not_found")
}
func (f *fakeSlack) UserIDByEmail(_ context.Context, email string) (string, error) {
	return f.byEmail[strings.ToLower(email)], nil
}
func (f *fakeSlack) PostMessage(_ context.Context, channel, text, threadTS, username, iconURL string) (string, error) {
	f.posts = append(f.posts, slackPost{channel, text, threadTS, username, iconURL})
	return fmt.Sprintf("900.%d", len(f.posts)), nil
}
func (f *fakeSlack) UpdateMessage(_ context.Context, _, ts, text string) error {
	f.updates[ts] = text
	return nil
}
func (f *fakeSlack) DeleteMessage(_ context.Context, _, ts string) error {
	f.deleted = append(f.deleted, ts)
	return nil
}
func (f *fakeSlack) AddReaction(_ context.Context, _, ts, name string) error {
	f.reactions[ts+":"+name]++
	return nil
}
func (f *fakeSlack) RemoveReaction(_ context.Context, _, ts, name string) error {
	f.reactions[ts+":"+name]--
	return nil
}
func (f *fakeSlack) DownloadFile(_ context.Context, file model.SlackFile, _ int64) (model.Attachment, error) {
	a, ok := f.files[file.DownloadURL]
	if !ok {
		return a, fmt.Errorf("not found")
	}
	return a, nil
}
func (f *fakeSlack) UploadFile(_ context.Context, _, threadTS string, a model.Attachment) error {
	f.uploads = append(f.uploads, threadTS+":"+a.Filename)
	return nil
}

type webexPost struct {
	Room, Markdown, ParentID, File string
}

type fakeWebex struct {
	people   map[string]model.Person
	messages map[string]model.WebexMessage
	posts    []webexPost
	edits    map[string]string
	deleted  []string
	files    map[string]model.Attachment
	// rejectMentions makes posts containing a Webex mention fail with 400.
	rejectMentions bool
}

func newFakeWebex() *fakeWebex {
	return &fakeWebex{
		people: map[string]model.Person{
			"PME":  {ID: "PME", DisplayName: "Bridge"},
			"PCAR": {ID: "PCAR", DisplayName: "Carol", Email: "carol@example.com", AvatarURL: "https://avatars/carol"},
			"PADA": {ID: "PADA", DisplayName: "Ada W", Email: "ada@example.com"},
			"PBOT": {ID: "PBOT", DisplayName: "Jira", IsBot: true},
		},
		messages: map[string]model.WebexMessage{},
		edits:    map[string]string{},
		files:    map[string]model.Attachment{},
	}
}

func (f *fakeWebex) Me(context.Context) (model.Person, error) { return f.people["PME"], nil }
func (f *fakeWebex) Person(_ context.Context, id string) (model.Person, error) {
	if p, ok := f.people[id]; ok {
		return p, nil
	}
	return model.Person{ID: id, DisplayName: "Unknown user"}, fmt.Errorf("not found")
}
func (f *fakeWebex) GetMessage(_ context.Context, id string) (model.WebexMessage, error) {
	m, ok := f.messages[id]
	if !ok {
		return m, &webex.APIError{Status: http.StatusNotFound}
	}
	return m, nil
}
func (f *fakeWebex) PostMessage(_ context.Context, room, markdown, parentID string, file *model.Attachment) (string, error) {
	if f.rejectMentions && strings.Contains(markdown, "<@") {
		return "", &webex.APIError{Status: http.StatusBadRequest, Body: "bad mention"}
	}
	p := webexPost{Room: room, Markdown: markdown, ParentID: parentID}
	if file != nil {
		p.File = file.Filename
	}
	f.posts = append(f.posts, p)
	return fmt.Sprintf("WX%d", len(f.posts)), nil
}
func (f *fakeWebex) EditMessage(_ context.Context, id, _, markdown string) error {
	f.edits[id] = markdown
	return nil
}
func (f *fakeWebex) DeleteMessage(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeWebex) DownloadFile(_ context.Context, url string, _ int64) (model.Attachment, error) {
	a, ok := f.files[url]
	if !ok {
		return a, fmt.Errorf("not found")
	}
	return a, nil
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	b     *Bridge
	slack *fakeSlack
	webex *fakeWebex
	store store.Store
	ctx   context.Context
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg := &config.Config{
		Display: config.Display{SlackUsernameSuffix: " (Webex)"},
		Sync:    config.Sync{BotMessages: true, Files: true, MaxFileBytes: 1 << 20, Mentions: true, Reactions: true},
		Pairings: []config.Pairing{
			{Name: "eng", SlackChannel: "C1", WebexRoom: "R1"},
		},
	}
	for _, m := range mutate {
		m(cfg)
	}
	ctx := context.Background()
	st := memstore.New(store.Options{})
	h := &harness{slack: newFakeSlack(), webex: newFakeWebex(), store: st, ctx: ctx}
	h.b = New(cfg, st, h.slack, h.webex)
	if err := h.b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) slackPost(m model.SlackMessage) {
	m.Channel = "C1"
	h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: model.SlackMessagePosted, Channel: "C1", Message: m})
}

func (h *harness) webexPost(msg model.WebexMessage) {
	msg.RoomID = "R1"
	h.webex.messages[msg.ID] = msg
	h.b.HandleWebex(h.ctx, model.WebexEvent{Kind: model.WebexMessageCreated, RoomID: "R1", ActorID: msg.PersonID, MessageID: msg.ID})
}

// ---- Slack -> Webex ----------------------------------------------------------

func TestSlackMessageMirroredToWebex(t *testing.T) {
	h := newHarness(t)
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "ship it *today* <@U2>"})

	if len(h.webex.posts) != 1 {
		t.Fatalf("posts = %+v", h.webex.posts)
	}
	want := "**Ada**: ship it **today** <@personEmail:bob@example.com|Bob>"
	if got := h.webex.posts[0]; got.Room != "R1" || got.Markdown != want || got.ParentID != "" {
		t.Errorf("post = %+v\nwant markdown %q", got, want)
	}
	link, _ := h.store.LinkBySlack(h.ctx, "eng", "1.0")
	if link == nil || link.WebexID != "WX1" || link.Origin != model.Slack {
		t.Errorf("link = %+v", link)
	}

	// Redelivery is ignored.
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "ship it"})
	if len(h.webex.posts) != 1 {
		t.Errorf("duplicate delivery was mirrored again")
	}
}

func TestSlackMultilineMessageStartsOnNewLine(t *testing.T) {
	h := newHarness(t)
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "```\ncode\n```"})
	if got := h.webex.posts[0].Markdown; got != "**Ada**:\n```\ncode\n```" {
		t.Errorf("markdown = %q", got)
	}
}

func TestOwnSlackPostsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.slackPost(model.SlackMessage{TS: "1.0", BotID: "BBRIDGE", Username: "Carol (Webex)", Text: "echo"})
	h.slackPost(model.SlackMessage{TS: "1.1", User: "UBRIDGE", Text: "echo"})
	if len(h.webex.posts) != 0 {
		t.Errorf("echo mirrored: %+v", h.webex.posts)
	}
}

func TestOtherBotsFollowSetting(t *testing.T) {
	h := newHarness(t)
	h.slackPost(model.SlackMessage{TS: "1.0", BotID: "BCI", Username: "CI", Text: "build passed"})
	if len(h.webex.posts) != 1 || h.webex.posts[0].Markdown != "**CI**: build passed" {
		t.Errorf("bot post = %+v", h.webex.posts)
	}

	off := newHarness(t, func(c *config.Config) { c.Sync.BotMessages = false })
	off.slackPost(model.SlackMessage{TS: "1.0", BotID: "BCI", Username: "CI", Text: "build passed"})
	if len(off.webex.posts) != 0 {
		t.Errorf("bot post mirrored with bot_messages off")
	}
}

func TestUnpairedChannelIgnored(t *testing.T) {
	h := newHarness(t)
	h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: model.SlackMessagePosted, Channel: "COTHER",
		Message: model.SlackMessage{Channel: "COTHER", TS: "1.0", User: "U1", Text: "hi"}})
	if len(h.webex.posts) != 0 {
		t.Error("unpaired channel mirrored")
	}
}

func TestSlackThreadReplyBecomesWebexReply(t *testing.T) {
	h := newHarness(t)
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "root"})
	h.slackPost(model.SlackMessage{TS: "1.5", ThreadTS: "1.0", User: "U2", Text: "reply"})
	if got := h.webex.posts[1]; got.ParentID != "WX1" {
		t.Errorf("reply parent = %q", got.ParentID)
	}
	link, _ := h.store.LinkBySlack(h.ctx, "eng", "1.5")
	if link.SlackThreadTS != "1.0" || link.WebexParentID != "WX1" {
		t.Errorf("reply link = %+v", link)
	}
}

func TestMentionFallbackWhenWebexRejects(t *testing.T) {
	h := newHarness(t)
	h.webex.rejectMentions = true
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "cc <@U2>"})
	if len(h.webex.posts) != 1 || h.webex.posts[0].Markdown != "**Ada**: cc @Bob" {
		t.Errorf("posts = %+v", h.webex.posts)
	}
}

func TestSlackEditAndDelete(t *testing.T) {
	h := newHarness(t)
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "helo"})

	edit := func(text, prev string) {
		h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: model.SlackMessageEdited, Channel: "C1",
			Message: model.SlackMessage{TS: "1.0", User: "U1", Text: text}, PreviousText: prev})
	}
	edit("helo", "helo") // unfurl or similar: no text change
	if len(h.webex.edits) != 0 {
		t.Error("no-op edit propagated")
	}
	edit("hello", "helo")
	if h.webex.edits["WX1"] != "**Ada**: hello" {
		t.Errorf("edits = %v", h.webex.edits)
	}

	h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: model.SlackMessageDeleted, Channel: "C1", TS: "1.0"})
	if len(h.webex.deleted) != 1 || h.webex.deleted[0] != "WX1" {
		t.Errorf("deleted = %v", h.webex.deleted)
	}
	if link, _ := h.store.LinkBySlack(h.ctx, "eng", "1.0"); link != nil {
		t.Error("link not removed")
	}
}

func TestDeletingAMirrorDoesNotDeleteTheOriginal(t *testing.T) {
	h := newHarness(t)
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Text: "hi"})
	h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: model.SlackMessageDeleted, Channel: "C1", TS: "900.1"})
	if len(h.webex.deleted) != 0 {
		t.Errorf("original Webex message deleted: %v", h.webex.deleted)
	}
}

func TestSlackFiles(t *testing.T) {
	h := newHarness(t)
	h.slack.files["u/a"] = model.Attachment{Filename: "a.png", Content: []byte("a")}
	h.slack.files["u/b"] = model.Attachment{Filename: "b.pdf", Content: []byte("b")}
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "", Files: []model.SlackFile{
		{Name: "a.png", DownloadURL: "u/a"},
		{Name: "b.pdf", DownloadURL: "u/b"},
		{Name: "missing.txt", DownloadURL: "u/missing"},
	}})
	if len(h.webex.posts) != 2 {
		t.Fatalf("posts = %+v", h.webex.posts)
	}
	first, second := h.webex.posts[0], h.webex.posts[1]
	if first.File != "a.png" || !strings.Contains(first.Markdown, "missing.txt") {
		t.Errorf("first = %+v", first)
	}
	if second.File != "b.pdf" || second.ParentID != "WX1" {
		t.Errorf("second = %+v", second)
	}
}

func TestSlackReactionsBecomeNotes(t *testing.T) {
	h := newHarness(t)
	h.slackPost(model.SlackMessage{TS: "1.0", User: "U1", Text: "done"})
	react := func(kind model.SlackEventKind) {
		h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: kind, Channel: "C1", TS: "1.0", User: "U2", Reaction: "tada"})
	}
	react(model.SlackReactionAdded)
	if note := h.webex.posts[1]; note.Markdown != "_Bob reacted 🎉_" || note.ParentID != "WX1" {
		t.Errorf("note = %+v", note)
	}
	react(model.SlackReactionRemoved)
	if len(h.webex.deleted) != 1 || h.webex.deleted[0] != "WX2" {
		t.Errorf("deleted = %v", h.webex.deleted)
	}

	// The bridge's own reactions (mirrored from Webex) are not echoed back.
	h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: model.SlackReactionAdded, Channel: "C1", TS: "1.0", User: "UBRIDGE", Reaction: "+1"})
	if len(h.webex.posts) != 2 {
		t.Error("own reaction echoed")
	}
}

// ---- Webex -> Slack ----------------------------------------------------------

func TestWebexMessageMirroredToSlack(t *testing.T) {
	h := newHarness(t)
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Markdown: "**hey** <@personEmail:ada@example.com|Ada>", Text: "hey Ada"})
	if len(h.slack.posts) != 1 {
		t.Fatalf("posts = %+v", h.slack.posts)
	}
	want := slackPost{Channel: "C1", Text: "*hey* <@U1>", Username: "Carol (Webex)", IconURL: "https://avatars/carol"}
	if got := h.slack.posts[0]; got != want {
		t.Errorf("post = %+v\nwant %+v", got, want)
	}
	link, _ := h.store.LinkByWebex(h.ctx, "W1")
	if link == nil || link.SlackTS != "900.1" || link.Origin != model.Webex {
		t.Errorf("link = %+v", link)
	}

	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Text: "again"})
	if len(h.slack.posts) != 1 {
		t.Error("duplicate event mirrored again")
	}
}

func TestWebexSparkMentionsResolved(t *testing.T) {
	h := newHarness(t)
	h.webexPost(model.WebexMessage{
		ID: "W1", PersonID: "PCAR", Text: "Ada W please review",
		HTML: `<p><spark-mention data-object-type="person" data-object-id="PADA">Ada W</spark-mention> please review</p>`,
	})
	if got := h.slack.posts[0].Text; got != "<@U1> please review" {
		t.Errorf("text = %q", got)
	}
}

func TestOwnWebexPostsIgnored(t *testing.T) {
	h := newHarness(t)
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PME", Text: "**Ada**: hi"})
	if len(h.slack.posts) != 0 {
		t.Errorf("echo mirrored: %+v", h.slack.posts)
	}
}

func TestWebexBotsFollowSetting(t *testing.T) {
	off := newHarness(t, func(c *config.Config) { c.Sync.BotMessages = false })
	off.webexPost(model.WebexMessage{ID: "W1", PersonID: "PBOT", Text: "ticket created"})
	if len(off.slack.posts) != 0 {
		t.Error("webex bot mirrored with bot_messages off")
	}
	on := newHarness(t)
	on.webexPost(model.WebexMessage{ID: "W1", PersonID: "PBOT", Text: "ticket created"})
	if len(on.slack.posts) != 1 {
		t.Error("webex bot not mirrored with bot_messages on")
	}
}

func TestWebexReplyAndSlackReplyShareAThread(t *testing.T) {
	h := newHarness(t)
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Text: "root"})
	h.webexPost(model.WebexMessage{ID: "W2", PersonID: "PADA", ParentID: "W1", Text: "reply"})
	if got := h.slack.posts[1].ThreadTS; got != "900.1" {
		t.Errorf("thread_ts = %q", got)
	}
	// A Slack reply in the same thread goes to the Webex root.
	h.slackPost(model.SlackMessage{TS: "5.0", ThreadTS: "900.1", User: "U2", Text: "me too"})
	if got := h.webex.posts[0].ParentID; got != "W1" {
		t.Errorf("parentId = %q", got)
	}
}

func TestWebexEditAndDelete(t *testing.T) {
	h := newHarness(t)
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Text: "helo"})
	h.webex.messages["W1"] = model.WebexMessage{ID: "W1", RoomID: "R1", PersonID: "PCAR", Text: "hello"}
	h.b.HandleWebex(h.ctx, model.WebexEvent{Kind: model.WebexMessageUpdated, RoomID: "R1", ActorID: "PCAR", MessageID: "W1"})
	if h.slack.updates["900.1"] != "hello" {
		t.Errorf("updates = %v", h.slack.updates)
	}
	h.b.HandleWebex(h.ctx, model.WebexEvent{Kind: model.WebexDeleted, RoomID: "R1", ActorID: "PCAR", MessageID: "W1", ActivityID: "w1-uuid"})
	if len(h.slack.deleted) != 1 || h.slack.deleted[0] != "900.1" {
		t.Errorf("deleted = %v", h.slack.deleted)
	}
}

func TestWebexFiles(t *testing.T) {
	h := newHarness(t)
	h.webex.files["https://files/1"] = model.Attachment{Filename: "plan.pdf", Content: []byte("x")}
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Files: []string{"https://files/1", "https://files/2"}})
	if got := h.slack.posts[0].Text; got != "_(attachment “file 2” could not be copied)_" {
		t.Errorf("text = %q", got)
	}
	if len(h.slack.uploads) != 1 || h.slack.uploads[0] != "900.1:plan.pdf" {
		t.Errorf("uploads = %v", h.slack.uploads)
	}
}

func TestWebexReactionsAreCountedBeforeMirroring(t *testing.T) {
	h := newHarness(t)
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Text: "done"})
	react := func(activity, person string) {
		h.b.HandleWebex(h.ctx, model.WebexEvent{Kind: model.WebexReactionAdded, RoomID: "R1", ActorID: person,
			MessageID: "W1", ActivityID: activity, Reaction: "thumbsup"})
	}
	unreact := func(activity, person string) {
		h.b.HandleWebex(h.ctx, model.WebexEvent{Kind: model.WebexDeleted, RoomID: "R1", ActorID: person,
			MessageID: "unrelated", ActivityID: activity})
	}
	react("A1", "PADA")
	react("A2", "PCAR")
	if n := h.slack.reactions["900.1:+1"]; n != 1 {
		t.Errorf("slack reactions added = %d, want 1", n)
	}
	unreact("A1", "PADA")
	if n := h.slack.reactions["900.1:+1"]; n != 1 {
		t.Errorf("removed while another person still reacted")
	}
	unreact("A2", "PCAR")
	if n := h.slack.reactions["900.1:+1"]; n != 0 {
		t.Errorf("reaction not removed, count = %d", n)
	}
	if len(h.slack.deleted) != 0 {
		t.Error("reaction removal deleted a message")
	}
}

func TestReactionsCanBeDisabled(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Sync.Reactions = false })
	h.webexPost(model.WebexMessage{ID: "W1", PersonID: "PCAR", Text: "done"})
	h.b.HandleWebex(h.ctx, model.WebexEvent{Kind: model.WebexReactionAdded, RoomID: "R1", ActorID: "PADA", MessageID: "W1", ActivityID: "A1", Reaction: "heart"})
	h.b.HandleSlack(h.ctx, model.SlackEvent{Kind: model.SlackReactionAdded, Channel: "C1", TS: "900.1", User: "U2", Reaction: "tada"})
	if len(h.slack.reactions) != 0 || len(h.webex.posts) != 0 {
		t.Errorf("reactions mirrored while disabled: %v %v", h.slack.reactions, h.webex.posts)
	}
}

func TestQueueProcessesInOrder(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { h.b.Run(ctx); close(done) }()
	h.b.SubmitSlack(model.SlackEvent{Kind: model.SlackMessagePosted, Channel: "C1", Message: model.SlackMessage{Channel: "C1", TS: "1.0", User: "U1", Text: "root"}})
	h.b.SubmitSlack(model.SlackEvent{Kind: model.SlackMessagePosted, Channel: "C1", Message: model.SlackMessage{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: "U1", Text: "reply"}})
	flushed := make(chan struct{})
	h.b.queue <- func(context.Context) { close(flushed) }
	<-flushed
	cancel()
	<-done
	if len(h.webex.posts) != 2 || h.webex.posts[1].ParentID != "WX1" {
		t.Errorf("posts = %+v", h.webex.posts)
	}
}
