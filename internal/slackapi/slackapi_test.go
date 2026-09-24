package slackapi

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

func parse(t *testing.T, raw string) any {
	t.Helper()
	var ev slackevents.MessageEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	return &ev
}

func TestTranslateMessages(t *testing.T) {
	posted, ok := Translate(parse(t, `{"type":"message","channel":"C1","user":"U1","text":"hi","ts":"2.0","thread_ts":"1.0",
		"files":[{"name":"a.png","mimetype":"image/png","size":3,"url_private_download":"https://files/a.png"}]}`))
	if !ok || posted.Kind != model.SlackMessagePosted {
		t.Fatalf("posted = %+v, %v", posted, ok)
	}
	m := posted.Message
	if m.Channel != "C1" || m.User != "U1" || m.Text != "hi" || m.TS != "2.0" || m.ThreadTS != "1.0" {
		t.Errorf("message = %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0].DownloadURL != "https://files/a.png" || m.Files[0].Size != 3 {
		t.Errorf("files = %+v", m.Files)
	}

	bot, _ := Translate(parse(t, `{"type":"message","subtype":"bot_message","channel":"C1","bot_id":"B1","username":"CI","text":"build ok","ts":"3.0"}`))
	if bot.Kind != model.SlackMessagePosted || bot.Message.BotID != "B1" || bot.Message.Username != "CI" {
		t.Errorf("bot = %+v", bot)
	}

	edited, _ := Translate(parse(t, `{"type":"message","subtype":"message_changed","channel":"C1",
		"message":{"user":"U1","text":"new","ts":"2.0"},"previous_message":{"user":"U1","text":"old","ts":"2.0"}}`))
	if edited.Kind != model.SlackMessageEdited || edited.Message.Text != "new" || edited.PreviousText != "old" || edited.Message.TS != "2.0" {
		t.Errorf("edited = %+v", edited)
	}

	deleted, _ := Translate(parse(t, `{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"2.0"}`))
	if deleted.Kind != model.SlackMessageDeleted || deleted.TS != "2.0" {
		t.Errorf("deleted = %+v", deleted)
	}

	if _, ok := Translate(parse(t, `{"type":"message","subtype":"channel_join","channel":"C1","user":"U1","ts":"4.0"}`)); ok {
		t.Error("channel_join should be ignored")
	}
}

func TestTranslateReactions(t *testing.T) {
	added, ok := Translate(&slackevents.ReactionAddedEvent{User: "U1", Reaction: "tada",
		Item: slackevents.Item{Type: "message", Channel: "C1", Timestamp: "2.0"}})
	if !ok || added.Kind != model.SlackReactionAdded || added.TS != "2.0" || added.Reaction != "tada" || added.User != "U1" {
		t.Errorf("added = %+v", added)
	}
	if _, ok := Translate(&slackevents.ReactionAddedEvent{Item: slackevents.Item{Type: "file"}}); ok {
		t.Error("file reactions should be ignored")
	}
}

func TestDeliverAcksOnlyAfterQueueing(t *testing.T) {
	msg := slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: parse(t, `{"type":"message","channel":"C1","user":"U1","text":"hi","ts":"1.0"}`),
	}}
	ok := func(model.SlackEvent) error { return nil }
	failing := func(model.SlackEvent) error { return errors.New("store down") }

	if !deliver(msg, ok) {
		t.Error("queued event should be acknowledged")
	}
	if deliver(msg, failing) {
		t.Error("event that could not be queued must not be acknowledged")
	}
	ignored := slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: parse(t, `{"type":"message","subtype":"channel_join","channel":"C1","ts":"2.0"}`),
	}}
	if !deliver(ignored, failing) {
		t.Error("events the bridge ignores should always be acknowledged")
	}
}
