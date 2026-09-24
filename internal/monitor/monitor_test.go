package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsExposition(t *testing.T) {
	var r Registry
	synced := r.Counter("sws_events_synced_total", "Events synced.", "source")
	synced.Inc("slack")
	synced.Inc("slack")
	synced.Inc("webex")
	r.GaugeFunc("sws_queue_events", "Queued events.", "status", func(context.Context) (map[string]float64, error) {
		return map[string]float64{"pending": 3, "parked": 1}, nil
	})
	r.GaugeFunc("sws_up", "Up.", "", func(context.Context) (map[string]float64, error) {
		return map[string]float64{"": 1}, nil
	})
	r.GaugeFunc("sws_broken", "Fails.", "", func(context.Context) (map[string]float64, error) {
		return nil, errors.New("db down")
	})

	rec := httptest.NewRecorder()
	Handler(nil, &r).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := `# HELP sws_events_synced_total Events synced.
# TYPE sws_events_synced_total counter
sws_events_synced_total{source="slack"} 2
sws_events_synced_total{source="webex"} 1
# HELP sws_queue_events Queued events.
# TYPE sws_queue_events gauge
sws_queue_events{status="parked"} 1
sws_queue_events{status="pending"} 3
# HELP sws_up Up.
# TYPE sws_up gauge
sws_up 1
`
	if got := rec.Body.String(); got != want {
		t.Errorf("metrics:\n%s\nwant:\n%s", got, want)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Errorf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestHealthz(t *testing.T) {
	ok := Check{Name: "store", Fn: func(context.Context) (string, error) { return "reachable", nil }}
	bad := Check{Name: "slack", Fn: func(context.Context) (string, error) { return "", errors.New("disconnected") }}

	get := func(checks ...Check) (int, map[string]any) {
		rec := httptest.NewRecorder()
		Handler(checks, &Registry{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	if code, body := get(ok); code != 200 || body["status"] != "ok" {
		t.Errorf("healthy = %d %v", code, body)
	}
	code, body := get(ok, bad)
	if code != 503 || body["status"] != "unhealthy" {
		t.Errorf("unhealthy = %d %v", code, body)
	}
	slack := body["checks"].(map[string]any)["slack"].(map[string]any)
	if slack["ok"] != false || slack["detail"] != "disconnected" {
		t.Errorf("slack check = %v", slack)
	}
}
