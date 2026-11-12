package main

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/manade/internal/obs"
	"github.com/cplieger/manade/internal/pool"
)

// markerPath is a var so tests write the marker under their own directory.
var markerPath = health.DefaultPath

// markerEvery is how often keepMarker writes the pool's verdict, a test
// seam, and markerLease the probe's deadline for it: three missed writes
// read as a wedged health loop.
var markerEvery = 10 * time.Second

var markerLease = health.Lease{Interval: markerEvery, Cycles: 3}

// keepMarker writes s's verdict to the marker until ctx ends, then drains
// it, so the probe reads unhealthy through the shutdown.
func keepMarker(ctx context.Context, l *health.Latch, s health.Signal) {
	t := time.NewTicker(markerEvery)
	defer t.Stop()
	for {
		l.Set(s.Healthy())
		select {
		case <-ctx.Done():
			l.BeginDrain()
			return
		case <-t.C:
		}
	}
}

// signalFunc adapts a func to health.Signal.
type signalFunc func() bool

func (f signalFunc) Healthy() bool { return f() }

// healthChecks is /healthz's verdict, the one the marker holds: one fixed
// reason per field of pool.Health, and one while running reports false.
func healthChecks(read func() pool.Health, running func() bool) []health.Check {
	return []health.Check{
		{Signal: signalFunc(running), Reason: "the pool is shutting down"},
		{Signal: signalFunc(func() bool { return read().Started }), Reason: "the pool is starting: its boot or a scale set's first session has not finished"},
		{Signal: signalFunc(func() bool { return read().Listening }), Reason: "a scale set's listener has not polled GitHub within its lease"},
		{Signal: signalFunc(func() bool { return read().Reconciling }), Reason: "the reconciler has not finished a good pass within its lease"},
	}
}

// serveHTTP serves /metrics and /healthz on ln until ctx ends.
func serveHTTP(ctx context.Context, ln net.Listener, m *obs.Metrics, checks []health.Check) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.Handle("GET /healthz", health.CheckHandler(checks...))
	srv := httpserver.New(mux)
	// Neither route streams, so a whole request and its answer are bounded.
	srv.ReadTimeout = 10 * time.Second
	srv.WriteTimeout = 30 * time.Second
	return httpserver.Run(ctx, srv, ln)
}
