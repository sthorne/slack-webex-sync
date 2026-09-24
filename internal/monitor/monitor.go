// Package monitor serves /healthz and a Prometheus-format /metrics.
//
// The metrics are written in the Prometheus text exposition format by hand,
// which avoids pulling the Prometheus client library and its dependency tree
// into the binary.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds counters and gauges.
type Registry struct {
	mu       sync.Mutex
	families []family
}

type family interface {
	write(ctx context.Context, w io.Writer)
}

// CounterVec is a counter with one label.
type CounterVec struct {
	name, help, label string
	mu                sync.Mutex
	values            map[string]uint64
}

// Counter registers a counter with one label.
func (r *Registry) Counter(name, help, label string) *CounterVec {
	c := &CounterVec{name: name, help: help, label: label, values: map[string]uint64{}}
	r.mu.Lock()
	r.families = append(r.families, c)
	r.mu.Unlock()
	return c
}

// Inc adds one for the label value.
func (c *CounterVec) Inc(labelValue string) {
	c.mu.Lock()
	c.values[labelValue]++
	c.mu.Unlock()
}

// Value returns the current count for a label value.
func (c *CounterVec) Value(labelValue string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[labelValue]
}

func (c *CounterVec) write(_ context.Context, w io.Writer) {
	c.mu.Lock()
	samples := make(map[string]float64, len(c.values))
	for k, v := range c.values {
		samples[k] = float64(v)
	}
	c.mu.Unlock()
	writeFamily(w, c.name, c.help, "counter", c.label, samples)
}

type gaugeFunc struct {
	name, help, label string
	fn                func(ctx context.Context) (map[string]float64, error)
}

// GaugeFunc registers a gauge computed at scrape time. With an empty label,
// fn should return a single sample under the key "".
func (r *Registry) GaugeFunc(name, help, label string, fn func(ctx context.Context) (map[string]float64, error)) {
	r.mu.Lock()
	r.families = append(r.families, &gaugeFunc{name: name, help: help, label: label, fn: fn})
	r.mu.Unlock()
}

func (g *gaugeFunc) write(ctx context.Context, w io.Writer) {
	samples, err := g.fn(ctx)
	if err != nil {
		slog.Warn("metric unavailable", "metric", g.name, "err", err)
		return
	}
	writeFamily(w, g.name, g.help, "gauge", g.label, samples)
}

func writeFamily(w io.Writer, name, help, typ, label string, samples map[string]float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	keys := make([]string, 0, len(samples))
	for k := range samples {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		value := strconv.FormatFloat(samples[k], 'g', -1, 64)
		if label == "" {
			fmt.Fprintf(w, "%s %s\n", name, value)
			continue
		}
		fmt.Fprintf(w, "%s{%s=%q} %s\n", name, label, escapeLabel(k), value)
	}
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(v)
}

// WriteTo writes every metric in exposition format.
func (r *Registry) WriteTo(ctx context.Context, w io.Writer) {
	r.mu.Lock()
	families := append([]family(nil), r.families...)
	r.mu.Unlock()
	for _, f := range families {
		f.write(ctx, w)
	}
}

// Check is one health probe. It returns a short detail, or an error when
// unhealthy.
type Check struct {
	Name string
	Fn   func(ctx context.Context) (string, error)
}

type checkResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Handler serves /healthz and /metrics.
func Handler(checks []Check, metrics *Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		healthy := true
		results := map[string]checkResult{}
		for _, c := range checks {
			detail, err := c.Fn(ctx)
			if err != nil {
				healthy = false
				detail = err.Error()
			}
			results[c.Name] = checkResult{OK: err == nil, Detail: detail}
		}
		status, code := "ok", http.StatusOK
		if !healthy {
			status, code = "unhealthy", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "checks": results})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		metrics.WriteTo(r.Context(), w)
	})
	return mux
}

// Serve listens on addr until ctx is cancelled.
func Serve(ctx context.Context, addr string, handler http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("health endpoint: %w", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("health and metrics listening", "addr", ln.Addr().String())
	if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
