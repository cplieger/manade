package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/manade/internal/r5/pool"
)

func TestHealthChecksNameEachLease(t *testing.T) {
	all := pool.Health{Started: true, Listening: true, Reconciling: true}
	cases := []struct {
		name    string
		want    string
		h       pool.Health
		running bool
	}{
		{name: "healthy", h: all, running: true},
		{name: "stopping", h: all, want: "shutting down"},
		{name: "starting", h: pool.Health{Listening: true, Reconciling: true}, running: true, want: "is starting"},
		{name: "listenerLate", h: pool.Health{Started: true, Reconciling: true}, running: true, want: "listener has not polled"},
		{name: "reconcilerLate", h: pool.Health{Started: true, Listening: true}, running: true, want: "reconciler has not finished"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := health.CheckHandler(healthChecks(func() pool.Health { return tc.h }, func() bool { return tc.running })...)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
			var body struct{ Reasons []string }
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if tc.want == "" {
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want 200", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusServiceUnavailable || len(body.Reasons) != 1 || !strings.Contains(body.Reasons[0], tc.want) {
				t.Errorf("status = %d, reasons = %q, want 503 with one reason naming %q", rec.Code, body.Reasons, tc.want)
			}
		})
	}
}

func TestKeepMarkerFollowsThePoolAndDrains(t *testing.T) {
	every := markerEvery
	markerEvery = 10 * time.Millisecond
	t.Cleanup(func() { markerEvery = every })
	path := filepath.Join(t.TempDir(), ".healthy")
	m := health.NewMarker(path, health.WithLogger(slog.New(slog.DiscardHandler)))
	ctx, cancel := context.WithCancel(t.Context())
	var healthy atomic.Bool
	healthy.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		keepMarker(ctx, health.NewLatch(m), signalFunc(healthy.Load))
	}()
	probe := health.WithMaxAge(markerLease.Duration())
	waitFor := func(fresh bool, what string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); (health.Inspect(path, probe).State == health.MarkerFresh) != fresh; {
			if time.Now().After(deadline) {
				cancel()
				<-done
				t.Fatal(what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(true, "the marker never read fresh for a healthy pool")
	healthy.Store(false)
	waitFor(false, "the marker still reads fresh after the pool turned unhealthy")
	healthy.Store(true)
	waitFor(true, "the marker never read fresh again once the pool recovered")
	cancel()
	<-done
	if got := health.Inspect(path, probe).State; got != health.MarkerAbsent {
		t.Errorf("marker after the drain = %v, want absent", got)
	}
}

func TestServeHTTPServesMetricsAndHealth(t *testing.T) {
	m := testMetrics(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- serveHTTP(ctx, ln, m, healthChecks(func() pool.Health { return pool.Health{} }, func() bool { return true }))
	}()
	base := "http://" + ln.Addr().String()
	for path, want := range map[string]int{"/metrics": http.StatusOK, "/healthz": http.StatusServiceUnavailable, "/other": http.StatusNotFound} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
		if path == "/metrics" && !strings.Contains(string(body), "manade_scope_serving") {
			t.Errorf("GET /metrics lacks manade_scope_serving:\n%s", body)
		}
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("serveHTTP after cancel = %v, want nil", err)
	}
}
