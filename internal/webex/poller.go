package webex

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

// Poller finds new and edited messages by listing each paired space. It
// cannot see deletions or reactions; those need the websocket.
type Poller struct {
	client *Client
	rooms  []string
	selfID string
	// PageSize is how many recent messages are fetched per space per poll.
	PageSize int

	// Per-room high-water marks, compared against server timestamps.
	lastCreated map[string]time.Time
	lastUpdated map[string]time.Time
}

// NewPoller creates a poller. Messages older than now are ignored so that
// starting the bridge does not replay history.
func NewPoller(client *Client, roomIDs []string, selfID string) *Poller {
	now := time.Now()
	p := &Poller{
		client:      client,
		rooms:       roomIDs,
		selfID:      selfID,
		PageSize:    100,
		lastCreated: map[string]time.Time{},
		lastUpdated: map[string]time.Time{},
	}
	for _, room := range roomIDs {
		p.lastCreated[room] = now
		p.lastUpdated[room] = now
	}
	return p
}

// PollOnce checks every room once. It must not be called concurrently.
func (p *Poller) PollOnce(ctx context.Context, sink func(model.WebexEvent)) {
	for _, room := range p.rooms {
		if err := p.pollRoom(ctx, room, sink); err != nil && ctx.Err() == nil {
			slog.Warn("webex poll failed", "room", room, "err", err)
		}
	}
}

func (p *Poller) pollRoom(ctx context.Context, room string, sink func(model.WebexEvent)) error {
	messages, err := p.client.ListMessages(ctx, room, p.PageSize)
	if err != nil {
		return err
	}
	// The API returns newest first; replay oldest first to keep order.
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].Created < messages[j].Created })

	createdMark, updatedMark := p.lastCreated[room], p.lastUpdated[room]
	for _, msg := range messages {
		created, _ := time.Parse(time.RFC3339Nano, msg.Created)
		updated, _ := time.Parse(time.RFC3339Nano, msg.Updated)
		if created.After(p.lastCreated[room]) {
			if created.After(createdMark) {
				createdMark = created
			}
			if msg.PersonID != p.selfID {
				sink(model.WebexEvent{Kind: model.WebexMessageCreated, RoomID: room, ActorID: msg.PersonID, MessageID: msg.ID})
			}
			continue
		}
		if !updated.IsZero() && updated.After(p.lastUpdated[room]) {
			if updated.After(updatedMark) {
				updatedMark = updated
			}
			if msg.PersonID != p.selfID {
				sink(model.WebexEvent{Kind: model.WebexMessageUpdated, RoomID: room, ActorID: msg.PersonID, MessageID: msg.ID})
			}
		}
	}
	p.lastCreated[room], p.lastUpdated[room] = createdMark, updatedMark
	return nil
}

// Source delivers Webex events from the websocket, falling back to
// polling while the websocket is down. Each time the websocket (re)connects,
// one poll runs to catch anything missed in the gap. Duplicates are
// harmless: the bridge ignores messages it has already linked.
type Source struct {
	Listener *Listener // nil disables the websocket
	Poller   *Poller
	Interval time.Duration
}

// Run blocks until ctx is cancelled.
func (s *Source) Run(ctx context.Context, sink func(model.WebexEvent)) {
	if s.Listener == nil {
		slog.Info("webex websocket disabled; polling", "interval", s.Interval)
		s.pollLoop(ctx, sink, nil, nil)
		return
	}
	var up atomic.Bool
	catchUp := make(chan struct{}, 1)
	s.Listener.OnConnected = func() {
		up.Store(true)
		select {
		case catchUp <- struct{}{}:
		default:
		}
	}
	s.Listener.OnDisconnected = func() {
		up.Store(false)
		slog.Warn("webex websocket down; polling until it reconnects", "interval", s.Interval)
	}
	go s.pollLoop(ctx, sink, &up, catchUp)
	s.Listener.Run(ctx, sink)
}

func (s *Source) pollLoop(ctx context.Context, sink func(model.WebexEvent), up *atomic.Bool, catchUp <-chan struct{}) {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-catchUp:
			s.Poller.PollOnce(ctx, sink)
		case <-ticker.C:
			if up == nil || !up.Load() {
				s.Poller.PollOnce(ctx, sink)
			}
		}
	}
}
