package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/sthorne/slack-webex-sync/internal/model"
	"github.com/sthorne/slack-webex-sync/internal/store"
)

// Incoming events are saved to the store before they are acknowledged, then
// processed by one worker, oldest first. A failed event is retried after
// each delay in RetryDelays; once MaxAttempts attempts have failed it is
// parked for an operator (see the "events" CLI commands). A backed-off
// event does not hold up the ones behind it.

// MaxAttempts is how many times an event is tried before it is parked.
const MaxAttempts = 5

// RetryDelays are the waits after the 1st, 2nd, ... failed attempt:
// 5 attempts spread over about 15 minutes.
var RetryDelays = []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

const (
	sourceSlack = "slack"
	sourceWebex = "webex"
	batchSize   = 50
	idlePoll    = time.Second
)

// Observer receives processing outcomes, e.g. for metrics. Any may be nil.
type Observer struct {
	Received func(source string)
	Synced   func(source string)
	Failed   func(source string)
	Parked   func(source string)
}

type envelope struct {
	Source string            `json:"source"`
	Slack  *model.SlackEvent `json:"slack,omitempty"`
	Webex  *model.WebexEvent `json:"webex,omitempty"`
}

// SubmitSlack saves a Slack event to the queue. Only acknowledge the event
// to Slack when this returns nil. Safe for concurrent use.
func (b *Bridge) SubmitSlack(ev model.SlackEvent) error {
	return b.enqueue(envelope{Source: sourceSlack, Slack: &ev})
}

// SubmitWebex saves a Webex event to the queue. Safe for concurrent use.
func (b *Bridge) SubmitWebex(ev model.WebexEvent) error {
	return b.enqueue(envelope{Source: sourceWebex, Webex: &ev})
}

func (b *Bridge) enqueue(env envelope) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	now := b.enqueueTime()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = b.store.EnqueueEvent(ctx, store.QueuedEvent{
		// Due immediately; Enqueued may be nudged a microsecond ahead for ordering.
		ID: uuid.NewString(), Payload: payload, Status: store.EventPending, Enqueued: now, NextAttempt: b.now(),
	})
	if err != nil {
		return fmt.Errorf("queue %s event: %w", env.Source, err)
	}
	b.observe(b.Observer.Received, env.Source)
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return nil
}

// enqueueTime returns a timestamp later than any this process has handed
// out, so events keep their arrival order even within one microsecond (the
// queue orders by time, and ties would otherwise fall back to random IDs).
func (b *Bridge) enqueueTime() time.Time {
	b.enqueueMu.Lock()
	defer b.enqueueMu.Unlock()
	now := b.now().Truncate(time.Microsecond)
	if !now.After(b.lastEnqueued) {
		now = b.lastEnqueued.Add(time.Microsecond)
	}
	b.lastEnqueued = now
	return now
}

// Run processes queued events until ctx is cancelled. Events left over from
// a previous run are processed first.
func (b *Bridge) Run(ctx context.Context) {
	ticker := time.NewTicker(idlePoll)
	defer ticker.Stop()
	for {
		for b.ProcessDue(ctx) > 0 && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-b.wake:
		case <-ticker.C:
		}
	}
}

// ProcessDue handles one batch of due events and returns how many it
// handled.
func (b *Bridge) ProcessDue(ctx context.Context) int {
	due, err := b.store.DueEvents(ctx, b.now(), batchSize)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("reading event queue failed", "err", err)
		}
		return 0
	}
	for _, e := range due {
		if ctx.Err() != nil {
			return 0
		}
		b.process(ctx, e)
	}
	return len(due)
}

func (b *Bridge) process(ctx context.Context, e store.QueuedEvent) {
	var env envelope
	err := json.Unmarshal(e.Payload, &env)
	if err == nil {
		err = b.handle(ctx, env)
	} else {
		err = fmt.Errorf("unreadable event: %w", err)
		e.Attempts = MaxAttempts - 1 // retrying cannot help
	}
	if err == nil {
		if derr := b.store.DeleteEvent(ctx, e.ID); derr != nil {
			slog.Error("could not remove processed event; it will be processed again", "event", e.ID, "err", derr)
		}
		b.observe(b.Observer.Synced, env.Source)
		return
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return // shutting down; the event stays pending
	}

	e.Attempts++
	e.LastError = err.Error()
	b.observe(b.Observer.Failed, env.Source)
	if e.Attempts >= MaxAttempts {
		e.Status = store.EventParked
		slog.Error("event parked after repeated failures; see `slack-webex-sync events list`",
			"event", e.ID, "source", env.Source, "attempts", e.Attempts, "err", err)
		b.observe(b.Observer.Parked, env.Source)
	} else {
		delay := RetryDelays[min(e.Attempts-1, len(RetryDelays)-1)]
		e.NextAttempt = b.now().Add(delay)
		slog.Warn("event not synced; will retry", "event", e.ID, "source", env.Source,
			"attempt", e.Attempts, "retry_in", delay, "err", err)
	}
	if uerr := b.store.UpdateEvent(ctx, e); uerr != nil {
		slog.Error("could not record failed attempt", "event", e.ID, "err", uerr)
	}
}

func (b *Bridge) handle(ctx context.Context, env envelope) error {
	switch {
	case env.Source == sourceSlack && env.Slack != nil:
		return b.HandleSlack(ctx, *env.Slack)
	case env.Source == sourceWebex && env.Webex != nil:
		return b.HandleWebex(ctx, *env.Webex)
	}
	return fmt.Errorf("unknown event source %q", env.Source)
}

func (b *Bridge) observe(fn func(string), source string) {
	if fn != nil {
		fn(source)
	}
}

// Requeue moves a parked event back to pending with a fresh set of attempts.
func Requeue(ctx context.Context, st store.Store, id string, now time.Time) (bool, error) {
	e, err := st.GetEvent(ctx, id)
	if err != nil || e == nil {
		return false, err
	}
	e.Status, e.Attempts, e.NextAttempt = store.EventPending, 0, now
	return true, st.UpdateEvent(ctx, *e)
}

// Describe summarizes a queued event for operators.
func Describe(e store.QueuedEvent) string {
	var env envelope
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		return "unreadable event"
	}
	switch {
	case env.Slack != nil:
		s := env.Slack
		ts := s.TS
		if ts == "" {
			ts = s.Message.TS
		}
		return fmt.Sprintf("slack %s channel=%s ts=%s", slackKind(s.Kind), s.Channel, ts)
	case env.Webex != nil:
		w := env.Webex
		return fmt.Sprintf("webex %s room=%s message=%s", webexKind(w.Kind), w.RoomID, w.MessageID)
	}
	return env.Source
}

func slackKind(k model.SlackEventKind) string {
	switch k {
	case model.SlackMessagePosted:
		return "message"
	case model.SlackMessageEdited:
		return "edit"
	case model.SlackMessageDeleted:
		return "delete"
	case model.SlackReactionAdded:
		return "reaction-added"
	case model.SlackReactionRemoved:
		return "reaction-removed"
	}
	return "unknown"
}

func webexKind(k model.WebexEventKind) string {
	switch k {
	case model.WebexMessageCreated:
		return "message"
	case model.WebexMessageUpdated:
		return "edit"
	case model.WebexDeleted:
		return "delete"
	case model.WebexReactionAdded:
		return "reaction-added"
	}
	return "unknown"
}
