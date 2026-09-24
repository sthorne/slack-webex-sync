// Package bridge turns an event on one platform into an action on the other.
//
// Every mirrored message is recorded as a store.Link so later edits,
// deletions, thread replies and reactions can find their counterpart.
//
// Loops are prevented two ways. Messages authored by the bridge's own
// identities (the Slack bot and the Webex service account) are ignored. A
// message that already has a link is never mirrored again.
//
// Events from both platforms go through one durable queue (see queue.go)
// and are handled one at a time, oldest first, so a thread reply is
// normally processed after the message it replies to.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/config"
	"github.com/sthorne/slack-webex-sync/internal/format"
	"github.com/sthorne/slack-webex-sync/internal/model"
	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/webex"
)

// SlackAPI is what the bridge needs from Slack.
type SlackAPI interface {
	Identity(ctx context.Context) (botID, userID string, err error)
	User(ctx context.Context, id string) (model.Person, error)
	UserIDByEmail(ctx context.Context, email string) (string, error)
	PostMessage(ctx context.Context, channel, text, threadTS, username, iconURL string) (string, error)
	UpdateMessage(ctx context.Context, channel, ts, text string) error
	DeleteMessage(ctx context.Context, channel, ts string) error
	AddReaction(ctx context.Context, channel, ts, name string) error
	RemoveReaction(ctx context.Context, channel, ts, name string) error
	DownloadFile(ctx context.Context, f model.SlackFile, maxBytes int64) (model.Attachment, error)
	UploadFile(ctx context.Context, channel, threadTS string, a model.Attachment) error
}

// WebexAPI is what the bridge needs from Webex.
type WebexAPI interface {
	Me(ctx context.Context) (model.Person, error)
	Person(ctx context.Context, id string) (model.Person, error)
	GetMessage(ctx context.Context, id string) (model.WebexMessage, error)
	PostMessage(ctx context.Context, roomID, markdown, parentID string, file *model.Attachment) (string, error)
	EditMessage(ctx context.Context, id, roomID, markdown string) error
	DeleteMessage(ctx context.Context, id string) error
	DownloadFile(ctx context.Context, url string, maxBytes int64) (model.Attachment, error)
}

// Bridge mirrors messages between paired Slack channels and Webex spaces.
type Bridge struct {
	cfg   *config.Config
	store store.Store
	slack SlackAPI
	webex WebexAPI

	byChannel map[string]config.Pairing
	byRoom    map[string]config.Pairing

	slackBotID   string
	slackBotUser string
	webexSelf    model.Person

	// Observer, if set, is told about processing outcomes (for metrics).
	Observer Observer

	wake chan struct{}
	now  func() time.Time

	enqueueMu    sync.Mutex
	lastEnqueued time.Time
}

// New creates a bridge. Call Start before Run.
func New(cfg *config.Config, st store.Store, slack SlackAPI, wx WebexAPI) *Bridge {
	b := &Bridge{
		cfg:       cfg,
		store:     st,
		slack:     slack,
		webex:     wx,
		byChannel: map[string]config.Pairing{},
		byRoom:    map[string]config.Pairing{},
		wake:      make(chan struct{}, 1),
		now:       time.Now,
	}
	for _, p := range cfg.Pairings {
		b.byChannel[p.SlackChannel] = p
		b.byRoom[p.WebexRoom] = p
	}
	return b
}

// Start learns the bridge's own identities so it can ignore its echoes.
func (b *Bridge) Start(ctx context.Context) error {
	botID, userID, err := b.slack.Identity(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test: %w", err)
	}
	b.slackBotID, b.slackBotUser = botID, userID
	me, err := b.webex.Me(ctx)
	if err != nil {
		return fmt.Errorf("webex people/me: %w", err)
	}
	b.webexSelf = me
	slog.Info("bridge ready", "slack_bot", userID, "webex_account", me.DisplayName, "pairings", len(b.cfg.Pairings))
	return nil
}

// WebexSelfID is the service account's person id.
func (b *Bridge) WebexSelfID() string { return b.webexSelf.ID }

// ---------------------------------------------------------------------------
// Slack -> Webex
// ---------------------------------------------------------------------------

// HandleSlack processes one Slack event synchronously.
func (b *Bridge) HandleSlack(ctx context.Context, ev model.SlackEvent) error {
	pairing, ok := b.byChannel[ev.Channel]
	if !ok {
		return nil
	}
	var err error
	switch ev.Kind {
	case model.SlackMessagePosted:
		err = b.slackPosted(ctx, pairing, ev.Message)
	case model.SlackMessageEdited:
		err = b.slackEdited(ctx, pairing, ev.Message, ev.PreviousText)
	case model.SlackMessageDeleted:
		err = b.slackDeleted(ctx, pairing, ev.TS)
	case model.SlackReactionAdded:
		err = b.slackReactionAdded(ctx, pairing, ev)
	case model.SlackReactionRemoved:
		err = b.slackReactionRemoved(ctx, pairing, ev)
	}
	if err != nil {
		return fmt.Errorf("pairing %s: %w", pairing.Name, err)
	}
	return nil
}

func (b *Bridge) isOwnSlack(m model.SlackMessage) bool {
	return (m.BotID != "" && m.BotID == b.slackBotID) || (m.User != "" && m.User == b.slackBotUser)
}

func (b *Bridge) slackAuthor(ctx context.Context, m model.SlackMessage) model.Person {
	if m.User != "" {
		p, err := b.slack.User(ctx, m.User)
		if err != nil {
			slog.Warn("slack user lookup failed", "user", m.User, "err", err)
		}
		return p
	}
	name := m.Username
	if name == "" {
		name = "Slack bot"
	}
	return model.Person{ID: m.BotID, DisplayName: name, IsBot: true}
}

func (b *Bridge) renderForWebex(ctx context.Context, author model.Person, text string, hasFiles, mentions bool) string {
	users := map[string]model.Person{}
	for _, id := range format.SlackMentionIDs(text) {
		if p, err := b.slack.User(ctx, id); err == nil {
			users[id] = p
		}
	}
	body := strings.TrimSpace(format.SlackToWebex(text, users, mentions))
	name := "**" + author.DisplayName + b.cfg.Display.WebexNameSuffix + "**"
	switch {
	case body == "" && hasFiles:
		return name + " shared a file"
	case body == "":
		return name
	case strings.Contains(body, "\n") || hasBlockPrefix(body):
		return name + ":\n" + body
	default:
		return name + ": " + body
	}
}

func hasBlockPrefix(s string) bool {
	for _, p := range []string{"```", ">", "- ", "* ", "#", "1. "} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// postToWebex posts a rendered Slack message, retrying without real
// mentions if Webex rejects them (e.g. the person isn't in the space).
func (b *Bridge) postToWebex(ctx context.Context, p config.Pairing, author model.Person, text, parentID string, file *model.Attachment, hasFiles bool) (string, error) {
	mentions := b.cfg.Sync.Mentions
	markdown := b.renderForWebex(ctx, author, text, hasFiles, mentions)
	id, err := b.webex.PostMessage(ctx, p.WebexRoom, markdown, parentID, file)
	if err == nil || !mentions || !webex.IsStatus(err, http.StatusBadRequest) {
		return id, err
	}
	plain := b.renderForWebex(ctx, author, text, hasFiles, false)
	if plain == markdown {
		return "", err
	}
	slog.Info("webex rejected mentions; retrying with plain names", "err", err)
	return b.webex.PostMessage(ctx, p.WebexRoom, plain, parentID, file)
}

func (b *Bridge) slackPosted(ctx context.Context, p config.Pairing, m model.SlackMessage) error {
	if b.isOwnSlack(m) || (m.BotID != "" && !b.cfg.Sync.BotMessages) {
		return nil
	}
	if link, err := b.store.LinkBySlack(ctx, p.Name, m.TS); err != nil || link != nil {
		return err // a non-nil link means it was already mirrored
	}

	isReply := m.ThreadTS != "" && m.ThreadTS != m.TS
	parentID := ""
	if isReply {
		root, err := b.store.LinkBySlack(ctx, p.Name, m.ThreadTS)
		if err != nil {
			return err
		}
		if root != nil {
			parentID = root.WebexRoot()
		}
	}

	attachments, failed := b.collectSlackFiles(ctx, m.Files)
	text := m.Text
	for _, name := range failed {
		text += fmt.Sprintf("\n_(attachment “%s” could not be copied)_", name)
	}
	var first *model.Attachment
	if len(attachments) > 0 {
		first = &attachments[0]
	}

	author := b.slackAuthor(ctx, m)
	webexID, err := b.postToWebex(ctx, p, author, text, parentID, first, len(m.Files) > 0)
	if err != nil {
		return fmt.Errorf("post to webex: %w", err)
	}
	link := store.Link{Pairing: p.Name, SlackTS: m.TS, WebexID: webexID, Origin: model.Slack, WebexParentID: parentID}
	if isReply {
		link.SlackThreadTS = m.ThreadTS
	}
	if err := b.store.PutLink(ctx, link); err != nil {
		return err
	}

	// Webex allows one file per message; send the rest in the same thread.
	for i := 1; i < len(attachments); i++ {
		thread := parentID
		if thread == "" {
			thread = webexID
		}
		if _, err := b.webex.PostMessage(ctx, p.WebexRoom, "", thread, &attachments[i]); err != nil {
			slog.Error("webex file upload failed", "file", attachments[i].Filename, "err", err)
		}
	}
	return nil
}

func (b *Bridge) collectSlackFiles(ctx context.Context, files []model.SlackFile) (ok []model.Attachment, failed []string) {
	if !b.cfg.Sync.Files {
		return nil, nil
	}
	for _, f := range files {
		a, err := b.slack.DownloadFile(ctx, f, b.cfg.Sync.MaxFileBytes)
		if err != nil {
			slog.Warn("slack file download failed", "file", f.Name, "err", err)
			failed = append(failed, f.Name)
			continue
		}
		ok = append(ok, a)
	}
	return ok, failed
}

func (b *Bridge) slackEdited(ctx context.Context, p config.Pairing, m model.SlackMessage, previous string) error {
	if b.isOwnSlack(m) || m.Text == previous {
		return nil // our own echo, or a non-text change such as an unfurl
	}
	link, err := b.store.LinkBySlack(ctx, p.Name, m.TS)
	if err != nil || link == nil || link.Origin != model.Slack {
		return err
	}
	author := b.slackAuthor(ctx, m)
	markdown := b.renderForWebex(ctx, author, m.Text, len(m.Files) > 0, b.cfg.Sync.Mentions)
	return b.webex.EditMessage(ctx, link.WebexID, p.WebexRoom, markdown)
}

func (b *Bridge) slackDeleted(ctx context.Context, p config.Pairing, ts string) error {
	link, err := b.store.LinkBySlack(ctx, p.Name, ts)
	if err != nil || link == nil {
		return err
	}
	if err := b.store.DeleteLink(ctx, *link); err != nil {
		return err
	}
	// Only a deleted original removes its copy. A deleted copy (say, by a
	// Slack admin) just drops the link; the author's message stays.
	if link.Origin == model.Slack {
		return b.webex.DeleteMessage(ctx, link.WebexID)
	}
	return nil
}

func (b *Bridge) slackReactionAdded(ctx context.Context, p config.Pairing, ev model.SlackEvent) error {
	if !b.cfg.Sync.Reactions || ev.User == b.slackBotUser {
		return nil
	}
	link, err := b.store.LinkBySlack(ctx, p.Name, ev.TS)
	if err != nil || link == nil {
		return err
	}
	author := b.slackAuthor(ctx, model.SlackMessage{User: ev.User})
	note := fmt.Sprintf("_%s%s reacted %s_", author.DisplayName, b.cfg.Display.WebexNameSuffix, format.SlackReactionText(ev.Reaction))
	id, err := b.webex.PostMessage(ctx, p.WebexRoom, note, link.WebexRoot(), nil)
	if err != nil {
		return fmt.Errorf("post reaction note: %w", err)
	}
	return b.store.PutReactionNote(ctx, store.ReactionNote{
		Pairing: p.Name, SlackTS: ev.TS, User: ev.User, Reaction: ev.Reaction, WebexID: id,
	})
}

func (b *Bridge) slackReactionRemoved(ctx context.Context, p config.Pairing, ev model.SlackEvent) error {
	if !b.cfg.Sync.Reactions || ev.User == b.slackBotUser {
		return nil
	}
	note, err := b.store.TakeReactionNote(ctx, p.Name, ev.TS, ev.User, ev.Reaction)
	if err != nil || note == nil {
		return err
	}
	return b.webex.DeleteMessage(ctx, note.WebexID)
}

// ---------------------------------------------------------------------------
// Webex -> Slack
// ---------------------------------------------------------------------------

// HandleWebex processes one Webex event synchronously.
func (b *Bridge) HandleWebex(ctx context.Context, ev model.WebexEvent) error {
	pairing, ok := b.byRoom[ev.RoomID]
	if !ok {
		return nil
	}
	if ev.ActorID != "" && webex.UUIDOf(ev.ActorID) == webex.UUIDOf(b.webexSelf.ID) {
		return nil
	}
	var err error
	switch ev.Kind {
	case model.WebexMessageCreated:
		err = b.webexPosted(ctx, pairing, ev.MessageID)
	case model.WebexMessageUpdated:
		err = b.webexEdited(ctx, pairing, ev.MessageID)
	case model.WebexDeleted:
		err = b.webexDeleted(ctx, pairing, ev)
	case model.WebexReactionAdded:
		err = b.webexReactionAdded(ctx, pairing, ev)
	}
	if err != nil {
		return fmt.Errorf("pairing %s: %w", pairing.Name, err)
	}
	return nil
}

func (b *Bridge) renderForSlack(ctx context.Context, msg model.WebexMessage) string {
	markdown := format.WebexMessageMarkdown(msg)
	mentions := b.cfg.Sync.Mentions
	emailsByPerson := map[string]string{}
	slackIDs := map[string]string{}
	if mentions {
		emails, personIDs := format.WebexMentionTargets(markdown)
		for _, id := range personIDs {
			if p, err := b.webex.Person(ctx, id); err == nil && p.Email != "" {
				emailsByPerson[id] = p.Email
				emails = append(emails, p.Email)
			}
		}
		for _, email := range emails {
			if id, err := b.slack.UserIDByEmail(ctx, email); err == nil && id != "" {
				slackIDs[email] = id
			}
		}
	}
	return strings.TrimSpace(format.WebexToSlack(markdown, slackIDs, emailsByPerson, mentions))
}

func (b *Bridge) webexPosted(ctx context.Context, p config.Pairing, messageID string) error {
	if link, err := b.store.LinkByWebex(ctx, messageID); err != nil || link != nil {
		return err
	}
	msg, err := b.webex.GetMessage(ctx, messageID)
	if err != nil {
		return fmt.Errorf("fetch webex message: %w", err)
	}
	if msg.PersonID == b.webexSelf.ID {
		return nil
	}
	// The websocket and the poller can build the id differently; key the
	// link on the id the REST API returned.
	if msg.ID != "" && msg.ID != messageID {
		if link, err := b.store.LinkByWebex(ctx, msg.ID); err != nil || link != nil {
			return err
		}
		messageID = msg.ID
	}
	author, err := b.webex.Person(ctx, msg.PersonID)
	if err != nil {
		slog.Warn("webex person lookup failed", "person", msg.PersonID, "err", err)
		if msg.PersonEmail != "" {
			author.DisplayName = msg.PersonEmail
		}
	}
	if author.IsBot && !b.cfg.Sync.BotMessages {
		return nil
	}

	threadTS := ""
	if msg.ParentID != "" {
		root, err := b.store.LinkByWebex(ctx, msg.ParentID)
		if err != nil {
			return err
		}
		if root != nil {
			threadTS = root.SlackRoot()
		}
	}

	text := b.renderForSlack(ctx, msg)
	attachments, failed := b.collectWebexFiles(ctx, msg.Files)
	for _, name := range failed {
		text += fmt.Sprintf("\n_(attachment “%s” could not be copied)_", name)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "_shared a file_"
	}

	ts, err := b.slack.PostMessage(ctx, p.SlackChannel, text, threadTS,
		author.DisplayName+b.cfg.Display.SlackUsernameSuffix, author.AvatarURL)
	if err != nil {
		return fmt.Errorf("post to slack: %w", err)
	}
	if err := b.store.PutLink(ctx, store.Link{
		Pairing: p.Name, SlackTS: ts, WebexID: messageID, Origin: model.Webex,
		SlackThreadTS: threadTS, WebexParentID: msg.ParentID,
	}); err != nil {
		return err
	}
	// Files go in the same thread as the message they came with.
	fileThread := threadTS
	if fileThread == "" {
		fileThread = ts
	}
	for _, a := range attachments {
		if err := b.slack.UploadFile(ctx, p.SlackChannel, fileThread, a); err != nil {
			slog.Error("slack file upload failed", "file", a.Filename, "err", err)
		}
	}
	return nil
}

func (b *Bridge) collectWebexFiles(ctx context.Context, urls []string) (ok []model.Attachment, failed []string) {
	if !b.cfg.Sync.Files {
		return nil, nil
	}
	for i, u := range urls {
		a, err := b.webex.DownloadFile(ctx, u, b.cfg.Sync.MaxFileBytes)
		if err != nil {
			slog.Warn("webex file download failed", "url", u, "err", err)
			failed = append(failed, fmt.Sprintf("file %d", i+1))
			continue
		}
		ok = append(ok, a)
	}
	return ok, failed
}

func (b *Bridge) webexEdited(ctx context.Context, p config.Pairing, messageID string) error {
	link, err := b.store.LinkByWebex(ctx, messageID)
	if err != nil || link == nil || link.Origin != model.Webex {
		return err
	}
	msg, err := b.webex.GetMessage(ctx, messageID)
	if err != nil {
		return fmt.Errorf("fetch webex message: %w", err)
	}
	text := b.renderForSlack(ctx, msg)
	if text == "" {
		text = "_shared a file_"
	}
	return b.slack.UpdateMessage(ctx, p.SlackChannel, link.SlackTS, text)
}

// webexDeleted handles a deleted activity, which is either a message or a
// reaction.
func (b *Bridge) webexDeleted(ctx context.Context, p config.Pairing, ev model.WebexEvent) error {
	if ev.ActivityID != "" {
		reaction, remaining, err := b.store.RemoveWebexReaction(ctx, ev.ActivityID)
		if err != nil {
			return err
		}
		if reaction != nil {
			if remaining > 0 {
				return nil // someone else still has this reaction
			}
			link, err := b.store.LinkByWebex(ctx, reaction.WebexID)
			if err != nil || link == nil {
				return err
			}
			return b.slack.RemoveReaction(ctx, p.SlackChannel, link.SlackTS, format.SlackReactionForWebex(reaction.Reaction))
		}
	}
	link, err := b.store.LinkByWebex(ctx, ev.MessageID)
	if err != nil || link == nil {
		return err
	}
	if err := b.store.DeleteLink(ctx, *link); err != nil {
		return err
	}
	if link.Origin == model.Webex {
		return b.slack.DeleteMessage(ctx, p.SlackChannel, link.SlackTS)
	}
	return nil
}

func (b *Bridge) webexReactionAdded(ctx context.Context, p config.Pairing, ev model.WebexEvent) error {
	if !b.cfg.Sync.Reactions || ev.Reaction == "" {
		return nil
	}
	link, err := b.store.LinkByWebex(ctx, ev.MessageID)
	if err != nil || link == nil {
		return err
	}
	count, err := b.store.AddWebexReaction(ctx, store.WebexReaction{
		ActivityID: ev.ActivityID, WebexID: link.WebexID, PersonID: ev.ActorID, Reaction: ev.Reaction,
	})
	if err != nil || count != 1 {
		return err // someone already added this reaction on Slack's side
	}
	return b.slack.AddReaction(ctx, p.SlackChannel, link.SlackTS, format.SlackReactionForWebex(ev.Reaction))
}
