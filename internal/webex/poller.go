package webex

import (
	"context"
	"fmt"
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

	// lastSuccess is when every room was last polled without error (unix nanos).
	lastSuccess atomic.Int64
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
func (p *Poller) PollOnce(ctx context.Context, sink func(model.WebexEvent) error) {
	ok := true
	for _, room := range p.rooms {
		if err := p.pollRoom(ctx, room, sink); err != nil && ctx.Err() == nil {
			slog.Warn("webex poll failed", "room", room, "err", err)
			ok = false
		}
	}
	if ok {
		p.lastSuccess.Store(time.Now().UnixNano())
	}
}

// LastSuccess is when a poll of every room last succeeded (zero if never).
func (p *Poller) LastSuccess() time.Time {
	if n := p.lastSuccess.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// pollRoom queues new and edited messages. If queueing fails, the room's
// marks are left unchanged so the next poll offers the messages again.
func (p *Poller) pollRoom(ctx context.Context, room string, sink func(model.WebexEvent) error) error {
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
				if err := sink(model.WebexEvent{Kind: model.WebexMessageCreated, RoomID: room, ActorID: msg.PersonID, MessageID: msg.ID}); err != nil {
					return fmt.Errorf("queue message: %w", err)
				}
			}
			continue
		}
		if !updated.IsZero() && updated.After(p.lastUpdated[room]) {
			if updated.After(updatedMark) {
				updatedMark = updated
			}
			if msg.PersonID != p.selfID {
				if err := sink(model.WebexEvent{Kind: model.WebexMessageUpdated, RoomID: room, ActorID: msg.PersonID, MessageID: msg.ID}); err != nil {
					return fmt.Errorf("queue edit: %w", err)
				}
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

	websocketUp atomic.Bool
}

// Health reports whether Webex events are flowing, and how.
func (s *Source) Health() (ok bool, mode string) {
	if s.websocketUp.Load() {
		return true, "websocket"
	}
	if last := s.Poller.LastSuccess(); !last.IsZero() && time.Since(last) < 3*s.Interval+time.Minute {
		return true, "polling"
	}
	return false, "down"
}

// WebsocketUp reports whether the real-time connection is up.
func (s *Source) WebsocketUp() bool { return s.websocketUp.Load() }

// Run blocks until ctx is cancelled.
func (s *Source) Run(ctx context.Context, sink func(model.WebexEvent) error) {
	if s.Listener == nil {
		slog.Info("webex websocket disabled; polling", "interval", s.Interval)
		s.pollLoop(ctx, sink, nil, nil)
		return
	}
	up := &s.websocketUp
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
	go s.pollLoop(ctx, sink, up, catchUp)
	s.Listener.Run(ctx, sink)
}

func (s *Source) pollLoop(ctx context.Context, sink func(model.WebexEvent) error, up *atomic.Bool, catchUp <-chan struct{}) {
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
