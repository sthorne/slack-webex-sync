// Package slackapi adapts slack-go to the bridge: Web API calls with
// caching, and Socket Mode events translated to model.SlackEvent.
package slackapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

// Client is the bridge's view of the Slack Web API.
type Client struct {
	api *slack.Client

	mu      sync.Mutex
	people  map[string]model.Person
	byEmail map[string]string
}

// New creates a client. appToken is only needed for Listen.
func New(botToken, appToken string) *Client {
	opts := []slack.Option{}
	if appToken != "" {
		opts = append(opts, slack.OptionAppLevelToken(appToken))
	}
	return &Client{
		api:     slack.New(botToken, opts...),
		people:  map[string]model.Person{},
		byEmail: map[string]string{},
	}
}

// Identity returns the bot's bot id and user id, used to ignore its own posts.
func (c *Client) Identity(ctx context.Context) (botID, userID string, err error) {
	resp, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return "", "", err
	}
	return resp.BotID, resp.UserID, nil
}

// User looks up (and caches) a Slack user.
func (c *Client) User(ctx context.Context, id string) (model.Person, error) {
	c.mu.Lock()
	cached, ok := c.people[id]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}
	u, err := c.api.GetUserInfoContext(ctx, id)
	if err != nil {
		return model.Person{ID: id, DisplayName: id}, err
	}
	name := u.Profile.DisplayName
	if name == "" {
		name = u.Profile.RealName
	}
	if name == "" {
		name = u.RealName
	}
	if name == "" {
		name = u.Name
	}
	avatar := u.Profile.Image192
	if avatar == "" {
		avatar = u.Profile.Image72
	}
	p := model.Person{ID: id, DisplayName: name, Email: u.Profile.Email, AvatarURL: avatar, IsBot: u.IsBot}
	c.mu.Lock()
	c.people[id] = p
	c.mu.Unlock()
	return p, nil
}

// UserIDByEmail returns the Slack user with this email, or "" if none.
func (c *Client) UserIDByEmail(ctx context.Context, email string) (string, error) {
	key := strings.ToLower(email)
	c.mu.Lock()
	cached, ok := c.byEmail[key]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}
	u, err := c.api.GetUserByEmailContext(ctx, email)
	id := ""
	switch {
	case err == nil:
		id = u.ID
	case isSlackError(err, "users_not_found"):
	default:
		return "", err
	}
	c.mu.Lock()
	c.byEmail[key] = id
	c.mu.Unlock()
	return id, nil
}

// PostMessage posts text; username and iconURL override the bot's identity
// (requires the chat:write.customize scope).
func (c *Client) PostMessage(ctx context.Context, channel, text, threadTS, username, iconURL string) (string, error) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	if username != "" {
		opts = append(opts, slack.MsgOptionUsername(username))
	}
	if iconURL != "" {
		opts = append(opts, slack.MsgOptionIconURL(iconURL))
	}
	_, ts, err := c.api.PostMessageContext(ctx, channel, opts...)
	return ts, err
}

func (c *Client) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	_, _, _, err := c.api.UpdateMessageContext(ctx, channel, ts, slack.MsgOptionText(text, false))
	return err
}

// DeleteMessage deletes a message; one that is already gone is not an error.
func (c *Client) DeleteMessage(ctx context.Context, channel, ts string) error {
	_, _, err := c.api.DeleteMessageContext(ctx, channel, ts)
	if isSlackError(err, "message_not_found") {
		return nil
	}
	return err
}

func (c *Client) AddReaction(ctx context.Context, channel, ts, name string) error {
	err := c.api.AddReactionContext(ctx, name, slack.NewRefToMessage(channel, ts))
	if isSlackError(err, "already_reacted") {
		return nil
	}
	return err
}

func (c *Client) RemoveReaction(ctx context.Context, channel, ts, name string) error {
	err := c.api.RemoveReactionContext(ctx, name, slack.NewRefToMessage(channel, ts))
	if isSlackError(err, "no_reaction") {
		return nil
	}
	return err
}

// ErrTooLarge is returned by DownloadFile for files over the size limit.
var ErrTooLarge = errors.New("file exceeds the configured size limit")

// DownloadFile fetches a file shared in Slack (requires files:read).
func (c *Client) DownloadFile(ctx context.Context, f model.SlackFile, maxBytes int64) (model.Attachment, error) {
	if int64(f.Size) > maxBytes {
		return model.Attachment{}, ErrTooLarge
	}
	var buf bytes.Buffer
	if err := c.api.GetFileContext(ctx, f.DownloadURL, &buf); err != nil {
		return model.Attachment{}, err
	}
	if int64(buf.Len()) > maxBytes {
		return model.Attachment{}, ErrTooLarge
	}
	contentType := f.Mimetype
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return model.Attachment{Filename: f.Name, ContentType: contentType, Content: buf.Bytes()}, nil
}

// UploadFile shares a file in a channel (or thread).
func (c *Client) UploadFile(ctx context.Context, channel, threadTS string, a model.Attachment) error {
	_, err := c.api.UploadFileV2Context(ctx, slack.UploadFileV2Parameters{
		Reader:          bytes.NewReader(a.Content),
		FileSize:        len(a.Content),
		Filename:        a.Filename,
		Title:           a.Filename,
		Channel:         channel,
		ThreadTimestamp: threadTS,
	})
	return err
}

// ChannelStatus reports a channel's name and whether the bot is a member.
func (c *Client) ChannelStatus(ctx context.Context, channel string) (name string, member bool, err error) {
	ch, err := c.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		return "", false, err
	}
	return ch.Name, ch.IsMember, nil
}

func isSlackError(err error, code string) bool {
	if err == nil {
		return false
	}
	var resp slack.SlackErrorResponse
	if errors.As(err, &resp) {
		return resp.Err == code
	}
	return err.Error() == code
}

// Listen runs Socket Mode until ctx is cancelled, passing events to sink.
func (c *Client) Listen(ctx context.Context, sink func(model.SlackEvent)) error {
	sm := socketmode.New(c.api)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-sm.Events:
				if !ok {
					return
				}
				switch evt.Type {
				case socketmode.EventTypeConnecting:
					slog.Info("slack socket mode connecting")
				case socketmode.EventTypeConnected:
					slog.Info("slack socket mode connected")
				case socketmode.EventTypeConnectionError:
					slog.Warn("slack socket mode connection error", "data", fmt.Sprint(evt.Data))
				case socketmode.EventTypeEventsAPI:
					if evt.Request != nil {
						sm.Ack(*evt.Request)
					}
					if api, ok := evt.Data.(slackevents.EventsAPIEvent); ok {
						if ev, ok := Translate(api.InnerEvent.Data); ok {
							sink(ev)
						}
					}
				}
			}
		}
	}()
	return sm.RunContext(ctx)
}

// Translate converts a slack-go inner event to a bridge event.
func Translate(data any) (model.SlackEvent, bool) {
	switch e := data.(type) {
	case *slackevents.MessageEvent:
		return translateMessage(e)
	case *slackevents.ReactionAddedEvent:
		if e.Item.Type != "message" {
			return model.SlackEvent{}, false
		}
		return model.SlackEvent{Kind: model.SlackReactionAdded, Channel: e.Item.Channel, TS: e.Item.Timestamp, User: e.User, Reaction: e.Reaction}, true
	case *slackevents.ReactionRemovedEvent:
		if e.Item.Type != "message" {
			return model.SlackEvent{}, false
		}
		return model.SlackEvent{Kind: model.SlackReactionRemoved, Channel: e.Item.Channel, TS: e.Item.Timestamp, User: e.User, Reaction: e.Reaction}, true
	}
	return model.SlackEvent{}, false
}

// Subtypes that carry something a person (or bot) said.
var contentSubtypes = map[string]bool{
	"": true, "thread_broadcast": true, "file_share": true, "me_message": true, "bot_message": true,
}

func translateMessage(e *slackevents.MessageEvent) (model.SlackEvent, bool) {
	switch {
	case e.SubType == "message_changed":
		if e.Message == nil {
			return model.SlackEvent{}, false
		}
		ev := model.SlackEvent{Kind: model.SlackMessageEdited, Channel: e.Channel, Message: convertMsg(e.Channel, e.Message)}
		if e.PreviousMessage != nil {
			ev.PreviousText = e.PreviousMessage.Text
		}
		return ev, true
	case e.SubType == "message_deleted":
		ts := e.DeletedTimeStamp
		if ts == "" && e.PreviousMessage != nil {
			ts = e.PreviousMessage.Timestamp
		}
		return model.SlackEvent{Kind: model.SlackMessageDeleted, Channel: e.Channel, TS: ts}, true
	case contentSubtypes[e.SubType]:
		msg := convertMsg(e.Channel, e.Message)
		// Top-level fields are authoritative for new messages.
		msg.TS, msg.ThreadTS, msg.Text = e.TimeStamp, e.ThreadTimeStamp, e.Text
		if e.User != "" {
			msg.User = e.User
		}
		if e.BotID != "" {
			msg.BotID = e.BotID
		}
		if e.Username != "" {
			msg.Username = e.Username
		}
		return model.SlackEvent{Kind: model.SlackMessagePosted, Channel: e.Channel, Message: msg}, true
	}
	return model.SlackEvent{}, false
}

func convertMsg(channel string, m *slack.Msg) model.SlackMessage {
	if m == nil {
		return model.SlackMessage{Channel: channel}
	}
	out := model.SlackMessage{
		Channel:  channel,
		TS:       m.Timestamp,
		ThreadTS: m.ThreadTimestamp,
		User:     m.User,
		BotID:    m.BotID,
		Username: m.Username,
		Text:     m.Text,
	}
	if out.Username == "" && m.BotProfile != nil {
		out.Username = m.BotProfile.Name
	}
	for _, f := range m.Files {
		url := f.URLPrivateDownload
		if url == "" {
			url = f.URLPrivate
		}
		out.Files = append(out.Files, model.SlackFile{Name: f.Name, Mimetype: f.Mimetype, Size: f.Size, DownloadURL: url})
	}
	return out
}
