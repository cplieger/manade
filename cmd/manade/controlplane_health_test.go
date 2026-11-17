package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/manade/internal/pool"
)

// The checks' fixed reasons.
const (
	reasonStarting    = "the control plane is starting: its store has not opened or its first pool generation has not booted"
	reasonDraining    = "the control plane is shutting down"
	reasonStore       = "the store's writes have kept failing for 2 minutes"
	reasonHosts       = "the hosts listener has stopped accepting connections"
	reasonPool        = "the pool's pass loop has stalled"
	reasonReconcile   = "a host's reconcile loop has stalled"
	reasonApplier     = "a settings change has been applying for more than 10 minutes"
	reasonRecorder    = "the history has not written its queued messages for 2 minutes"
	reasonHousekeeper = "the housekeeper has not finished a run for 2 hours"
)

// healthz answers h's /healthz status and reasons.
func healthz(t *testing.T, h *cpHealth) (int, []string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	var body struct{ Reasons []string }
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return w.Code, body.Reasons
}

// healthyParts is every part healthy over a booted pool.
func healthyParts() *cpParts {
	ok := health.SignalFunc(func() bool { return true })
	return &cpParts{
		store: ok, hosts: ok, recorder: ok, housekeeper: ok,
		pool: func() pool.Health { return pool.Health{Booted: true, Passing: true, Reconciling: true, Applied: true} },
	}
}

func TestControlPlaneHealth(t *testing.T) {
	failing := health.SignalFunc(func() bool { return false })
	for _, tc := range []struct {
		set  func(p *cpParts)
		name string
		want string
	}{
		{name: "store", want: reasonStore, set: func(p *cpParts) { p.store = failing }},
		{name: "hostListener", want: reasonHosts, set: func(p *cpParts) { p.hosts = failing }},
		{name: "pool", want: reasonPool, set: func(p *cpParts) {
			p.pool = func() pool.Health { return pool.Health{Booted: true, Reconciling: true, Applied: true} }
		}},
		{name: "reconcile", want: reasonReconcile, set: func(p *cpParts) {
			p.pool = func() pool.Health { return pool.Health{Booted: true, Passing: true, Applied: true} }
		}},
		{name: "applier", want: reasonApplier, set: func(p *cpParts) {
			p.pool = func() pool.Health { return pool.Health{Booted: true, Passing: true, Reconciling: true} }
		}},
		{name: "recorder", want: reasonRecorder, set: func(p *cpParts) { p.recorder = failing }},
		{name: "housekeeper", want: reasonHousekeeper, set: func(p *cpParts) { p.housekeeper = failing }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyParts()
			tc.set(p)
			var h cpHealth
			h.ready(p)
			if code, reasons := healthz(t, &h); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{tc.want}) {
				t.Errorf("/healthz = %d %q, want 503 with only %q", code, reasons, tc.want)
			}
		})
	}
	t.Run("healthy", func(t *testing.T) {
		var h cpHealth
		h.ready(healthyParts())
		if code, reasons := healthz(t, &h); code != http.StatusOK || len(reasons) != 0 {
			t.Errorf("/healthz = %d %q, want 200", code, reasons)
		}
	})
}

// Starting stands alone: before the store opens every part is unknown, and
// before the boot pass the pool's loops have no lease to keep.
func TestControlPlaneHealthWhileStarting(t *testing.T) {
	var h cpHealth
	if code, reasons := healthz(t, &h); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{reasonStarting}) {
		t.Errorf("/healthz before the store opened = %d %q, want 503 starting", code, reasons)
	}
	p := healthyParts()
	p.pool = func() pool.Health { return pool.Health{Applied: true} }
	h.ready(p)
	if code, reasons := healthz(t, &h); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{reasonStarting}) {
		t.Errorf("/healthz before the boot pass = %d %q, want 503 starting", code, reasons)
	}
	p.store = health.SignalFunc(func() bool { return false })
	if _, reasons := healthz(t, &h); !slices.Equal(reasons, []string{reasonStarting, reasonStore}) {
		t.Errorf("reasons with the store failing at boot = %q, want starting and the store's", reasons)
	}
}

// A store that cannot open answers its class's reason alone, in place of
// starting, and with draining once the shutdown begins.
func TestControlPlaneHealthInTheStoresOutage(t *testing.T) {
	for _, tc := range []struct {
		class httpserver.OutageClass
		want  string
	}{
		{httpserver.OutageDamaged, "the store cannot open: its file is damaged"},
		{httpserver.OutageFormat, "the store cannot open: its file was written by another version of manade, or is not manade's file"},
		{httpserver.OutageNewer, "the store cannot open: its file was written by a newer version of manade"},
		{httpserver.OutageUnreadable, "the store cannot open: its file cannot be read"},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			var h cpHealth
			h.outage(tc.class)
			if code, reasons := healthz(t, &h); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{tc.want}) {
				t.Errorf("/healthz = %d %q, want 503 with only %q", code, reasons, tc.want)
			}
			h.BeginDrain()
			if _, reasons := healthz(t, &h); !slices.Equal(reasons, []string{reasonDraining, tc.want}) {
				t.Errorf("reasons through the drain = %q, want draining and %q", reasons, tc.want)
			}
		})
	}
}

func TestControlPlaneHealthDrains(t *testing.T) {
	var h cpHealth
	h.ready(healthyParts())
	h.BeginDrain()
	if code, reasons := healthz(t, &h); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{reasonDraining}) {
		t.Errorf("/healthz after BeginDrain = %d %q, want 503 draining", code, reasons)
	}
}
