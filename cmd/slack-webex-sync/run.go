package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/bridge"
	"github.com/sthorne/slack-webex-sync/internal/config"
	"github.com/sthorne/slack-webex-sync/internal/monitor"
	"github.com/sthorne/slack-webex-sync/internal/slackapi"
	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/webex"
)

func run(ctx context.Context, cfg *config.Config, st store.Store, tokens *tokenStore) error {
	wx, err := webexClient(ctx, cfg, tokens)
	if err != nil {
		return err
	}
	sl := slackapi.New(cfg.Slack.BotToken, cfg.Slack.AppToken)
	b := bridge.New(cfg, st, sl, wx)
	if err := b.Start(ctx); err != nil {
		return err
	}
	source := webexSource(cfg, st, wx, b.WebexSelfID())

	var slackUp atomic.Bool
	metrics := newMetrics(st, &slackUp, source)
	b.Observer = metrics.observer()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var monitorErr error
	start := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	start(func() { b.Run(ctx) })
	start(func() { store.RunPurger(ctx, st, cfg.Storage.Retention(), cfg.Storage.PurgeInterval, time.Now) })
	start(func() { source.Run(ctx, b.SubmitWebex) })
	if cfg.Health.Listen != "" {
		start(func() {
			handler := monitor.Handler(healthChecks(st, &slackUp, source), metrics.registry)
			if monitorErr = monitor.Serve(ctx, cfg.Health.Listen, handler); monitorErr != nil {
				cancel()
			}
		})
	}

	err = sl.Listen(ctx, b.SubmitSlack, slackUp.Store)
	cancel() // if Slack stops, stop everything else too
	wg.Wait()
	return errors.Join(err, monitorErr)
}

func webexSource(cfg *config.Config, st store.Store, wx *webex.Client, selfID string) *webex.Source {
	rooms := make([]string, 0, len(cfg.Pairings))
	for _, p := range cfg.Pairings {
		rooms = append(rooms, p.WebexRoom)
	}
	source := &webex.Source{
		Poller:   webex.NewPoller(wx, rooms, selfID),
		Interval: cfg.Webex.PollInterval,
	}
	if cfg.Webex.WebsocketEnabled() {
		listener := webex.NewListener(wx, cfg.Webex.DeviceURL, rooms, selfID)
		listener.LoadDevice = func() string {
			v, _ := st.GetValue(context.Background(), deviceKey)
			return v
		}
		listener.SaveDevice = func(url string) {
			if err := st.SetValue(context.Background(), deviceKey, url); err != nil {
				slog.Warn("could not save webex device", "err", err)
			}
		}
		source.Listener = listener
	}
	return source
}

func healthChecks(st store.Store, slackUp *atomic.Bool, source *webex.Source) []monitor.Check {
	return []monitor.Check{
		{Name: "slack", Fn: func(context.Context) (string, error) {
			if !slackUp.Load() {
				return "", errors.New("socket mode not connected")
			}
			return "socket mode connected", nil
		}},
		{Name: "webex", Fn: func(context.Context) (string, error) {
			ok, mode := source.Health()
			if !ok {
				return "", errors.New("websocket down and polling not succeeding")
			}
			return "receiving via " + mode, nil
		}},
		{Name: "store", Fn: func(ctx context.Context) (string, error) {
			pending, parked, err := st.CountEvents(ctx)
			if err != nil {
				return "", err
			}
			// Parked events need attention but don't make the bridge unhealthy.
			return fmt.Sprintf("%d pending, %d parked events", pending, parked), nil
		}},
	}
}

type metrics struct {
	registry                         *monitor.Registry
	received, synced, failed, parked *monitor.CounterVec
}

func newMetrics(st store.Store, slackUp *atomic.Bool, source *webex.Source) *metrics {
	r := &monitor.Registry{}
	m := &metrics{
		registry: r,
		received: r.Counter("sws_events_received_total", "Events queued, by source platform.", "source"),
		synced:   r.Counter("sws_events_synced_total", "Events processed successfully, by source platform.", "source"),
		failed:   r.Counter("sws_event_failures_total", "Failed processing attempts (each is retried or parked), by source platform.", "source"),
		parked:   r.Counter("sws_events_parked_total", "Events parked after repeated failures, by source platform.", "source"),
	}
	r.GaugeFunc("sws_queue_events", "Events currently in the queue, by status.", "status",
		func(ctx context.Context) (map[string]float64, error) {
			pending, parked, err := st.CountEvents(ctx)
			return map[string]float64{"pending": float64(pending), "parked": float64(parked)}, err
		})
	r.GaugeFunc("sws_connected", "1 when events are arriving from the platform.", "platform",
		func(context.Context) (map[string]float64, error) {
			webexOK, _ := source.Health()
			return map[string]float64{"slack": boolFloat(slackUp.Load()), "webex": boolFloat(webexOK)}, nil
		})
	r.GaugeFunc("sws_webex_websocket_up", "1 when the Webex real-time websocket is connected (0 means polling).", "",
		func(context.Context) (map[string]float64, error) {
			return map[string]float64{"": boolFloat(source.WebsocketUp())}, nil
		})
	return m
}

func (m *metrics) observer() bridge.Observer {
	return bridge.Observer{Received: m.received.Inc, Synced: m.synced.Inc, Failed: m.failed.Inc, Parked: m.parked.Inc}
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
