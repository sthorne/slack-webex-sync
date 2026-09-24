package webex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

// The device websocket is how Webex's own clients and SDKs receive events.
// It isn't documented for third-party use, so the translation below covers
// only the activity shapes the bridge needs. Activity content is encrypted
// on the websocket, so events carry only ids and the bridge fetches the
// decrypted message over REST.

// Listener receives real-time events over the Webex device websocket.
type Listener struct {
	client *Client
	// DevicesURL is the WDM endpoint used to register a device.
	DevicesURL string
	// LoadDevice and SaveDevice persist the registered device URL so
	// restarts reuse one device instead of piling up new ones.
	LoadDevice func() string
	SaveDevice func(string)
	// OnConnected and OnDisconnected report websocket state changes.
	OnConnected    func()
	OnDisconnected func()

	rooms    map[string]pairedRoom // keyed by conversation uuid
	selfUUID string
}

type pairedRoom struct {
	restID  string
	cluster string
}

// NewListener creates a listener for the given paired rooms (REST ids).
// selfID is the service account's person id; its own activity is ignored.
func NewListener(client *Client, devicesURL string, roomIDs []string, selfID string) *Listener {
	rooms := map[string]pairedRoom{}
	for _, id := range roomIDs {
		if decoded, ok := DecodeID(id); ok {
			rooms[decoded.UUID] = pairedRoom{restID: id, cluster: decoded.Cluster}
		} else {
			slog.Warn("webex room id is not in the expected format; websocket events for it will be missed", "room", id)
		}
	}
	return &Listener{client: client, DevicesURL: devicesURL, rooms: rooms, selfUUID: UUIDOf(selfID)}
}

type device struct {
	URL          string `json:"url"`
	WebSocketURL string `json:"webSocketUrl"`
}

func (l *Listener) device(ctx context.Context) (device, error) {
	if l.LoadDevice != nil {
		if saved := l.LoadDevice(); saved != "" {
			var d device
			resp, err := l.client.do(ctx, http.MethodGet, saved, nil)
			if err == nil {
				err = json.NewDecoder(resp.Body).Decode(&d)
				resp.Body.Close()
			}
			if err == nil && d.WebSocketURL != "" {
				return d, nil
			}
			slog.Info("saved webex device is no longer valid; registering a new one", "err", err)
		}
	}
	body, err := jsonBody(map[string]string{
		"deviceName":     "slack-webex-sync",
		"deviceType":     "DESKTOP",
		"localizedModel": "go",
		"model":          "go",
		"name":           "slack-webex-sync",
		"systemName":     "slack-webex-sync",
		"systemVersion":  "1.0",
	})
	if err != nil {
		return device{}, err
	}
	resp, err := l.client.do(ctx, http.MethodPost, l.DevicesURL, body)
	if err != nil {
		return device{}, fmt.Errorf("register webex device: %w", err)
	}
	defer resp.Body.Close()
	var d device
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return device{}, err
	}
	if d.WebSocketURL == "" {
		return device{}, errors.New("webex device registration returned no websocket URL")
	}
	if l.SaveDevice != nil {
		l.SaveDevice(d.URL)
	}
	return d, nil
}

// Run connects and reconnects until ctx is cancelled, passing events to
// sink. A frame is acknowledged only after sink has accepted all its events.
func (l *Listener) Run(ctx context.Context, sink func(model.WebexEvent) error) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := l.connect(ctx, sink)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		slog.Warn("webex websocket disconnected", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (l *Listener) connect(ctx context.Context, sink func(model.WebexEvent) error) error {
	d, err := l.device(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, d.WebSocketURL, nil)
	if err != nil {
		if l.SaveDevice != nil {
			l.SaveDevice("") // force re-registration next time
		}
		return fmt.Errorf("dial webex websocket: %w", err)
	}
	defer conn.Close()

	token, err := l.client.AccessToken(ctx)
	if err != nil {
		return err
	}
	auth := map[string]any{
		"id":   uuid.NewString(),
		"type": "authorization",
		"data": map[string]string{"token": "Bearer " + token},
	}
	if err := conn.WriteJSON(auth); err != nil {
		return fmt.Errorf("authorize webex websocket: %w", err)
	}

	const readTimeout = 90 * time.Second
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(readTimeout)) })

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				conn.Close()
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	// Mercury closes the socket promptly if authorization fails, which
	// reports the disconnect right away.
	slog.Info("webex websocket connected")
	if l.OnConnected != nil {
		l.OnConnected()
	}
	defer func() {
		if l.OnDisconnected != nil {
			l.OnDisconnected()
		}
	}()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		ackID, events := l.translate(raw)
		accepted := true
		for _, ev := range events {
			if err := sink(ev); err != nil {
				// Leave the frame unacknowledged. The catch-up poll after
				// the next reconnect finds the message if it is not redelivered.
				slog.Error("could not queue webex event", "err", err)
				accepted = false
				break
			}
		}
		if ackID != "" && accepted {
			_ = conn.WriteJSON(map[string]string{"type": "ack", "messageId": ackID})
		}
	}
}

type mercuryMessage struct {
	ID   string `json:"id"`
	Data struct {
		EventType string   `json:"eventType"`
		Activity  activity `json:"activity"`
	} `json:"data"`
}

type activity struct {
	ID    string `json:"id"`
	Verb  string `json:"verb"`
	Actor struct {
		ID        string `json:"id"`
		EntryUUID string `json:"entryUUID"`
	} `json:"actor"`
	Object struct {
		ID          string `json:"id"`
		ObjectType  string `json:"objectType"`
		DisplayName string `json:"displayName"`
	} `json:"object"`
	Target struct {
		ID         string `json:"id"`
		ObjectType string `json:"objectType"`
	} `json:"target"`
	Parent struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"parent"`
}

// translate turns one websocket frame into bridge events. It returns the id
// to acknowledge (if any).
func (l *Listener) translate(raw []byte) (string, []model.WebexEvent) {
	var msg mercuryMessage
	if err := json.Unmarshal(raw, &msg); err != nil || !bytes.Contains(raw, []byte(`"eventType"`)) {
		return msg.ID, nil
	}
	if msg.Data.EventType != "conversation.activity" {
		return msg.ID, nil
	}
	a := msg.Data.Activity
	room, ok := l.rooms[a.Target.ID]
	if !ok {
		return msg.ID, nil
	}
	actor := a.Actor.EntryUUID
	if actor == "" {
		actor = a.Actor.ID
	}
	if actor != "" && actor == l.selfUUID {
		return msg.ID, nil
	}

	messageID := func(activityID string) string { return EncodeID(room.cluster, "MESSAGE", activityID) }
	event := model.WebexEvent{RoomID: room.restID, ActorID: actor}

	switch a.Verb {
	case "post", "share":
		if a.Parent.Type == "edit" {
			event.Kind = model.WebexMessageUpdated
			event.MessageID = messageID(a.Parent.ID)
		} else {
			event.Kind = model.WebexMessageCreated
			event.MessageID = messageID(a.ID)
		}
	case "update":
		if a.Object.ObjectType != "comment" && a.Object.ObjectType != "content" {
			return msg.ID, nil
		}
		event.Kind = model.WebexMessageUpdated
		event.MessageID = messageID(a.Object.ID)
	case "delete":
		event.Kind = model.WebexDeleted
		event.ActivityID = a.Object.ID
		event.MessageID = messageID(a.Object.ID)
	case "add":
		if a.Object.ObjectType != "reaction2" || a.Parent.ID == "" {
			return msg.ID, nil
		}
		event.Kind = model.WebexReactionAdded
		event.ActivityID = a.ID
		event.MessageID = messageID(a.Parent.ID)
		event.Reaction = a.Object.DisplayName
	default:
		return msg.ID, nil
	}
	return msg.ID, []model.WebexEvent{event}
}
