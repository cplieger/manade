package main

import (
	"net/http"
	"sync/atomic"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/manade/internal/pool"
)

// cpParts is what the control plane's checks read once its store has
// opened. Each signal holds its own lease.
type cpParts struct {
	store, hosts, recorder, housekeeper health.Signal
	pool                                func() pool.Health
}

// cpHealth is the control plane's /healthz: one check per loop,
// each with its fixed reason. It reads starting until the store opens and
// the first generation boots, and draining from beginDrain on; until the
// parts are set, every other check passes, so the starting reason stands
// alone.
type cpHealth struct {
	parts atomic.Pointer[cpParts]
	// failed is why the store cannot open, nil while it has not failed.
	failed   atomic.Pointer[httpserver.OutageClass]
	draining atomic.Bool
}

// ready hands the checks their parts.
func (h *cpHealth) ready(p *cpParts) { h.parts.Store(p) }

// outage holds the control plane in its store's outage, class the reason
// the file cannot open; the starting reason gives way to it.
func (h *cpHealth) outage(class httpserver.OutageClass) { h.failed.Store(&class) }

// outageReasons is the fixed reason of each class an open of the store
// fails with.
var outageReasons = []struct {
	class  httpserver.OutageClass
	reason string
}{
	{httpserver.OutageDamaged, "the store cannot open: its file is damaged"},
	{httpserver.OutageFormat, "the store cannot open: its file was written by another version of manade, or is not manade's file"},
	{httpserver.OutageNewer, "the store cannot open: its file was written by a newer version of manade"},
	{httpserver.OutageUnreadable, "the store cannot open: its file cannot be read"},
}

// beginDrain makes /healthz answer 503 for good; call it when the
// shutdown begins.
func (h *cpHealth) beginDrain() { h.draining.Store(true) }

// handler answers /healthz.
func (h *cpHealth) handler() http.Handler { return health.CheckHandler(h.checks()...) }

func (h *cpHealth) checks() []health.Check {
	// set passes until the parts are set; booted, until the pool has
	// booted too, since a pass loop or a reconcile lease means nothing
	// before the boot pass.
	set := func(read func(p *cpParts) bool) health.Signal {
		return health.SignalFunc(func() bool {
			p := h.parts.Load()
			return p == nil || read(p)
		})
	}
	booted := func(read func(ph pool.Health) bool) health.Signal {
		return set(func(p *cpParts) bool {
			ph := p.pool()
			return !ph.Booted || read(ph)
		})
	}
	outage := make([]health.Check, 0, len(outageReasons))
	for _, o := range outageReasons {
		outage = append(outage, health.Check{Signal: health.SignalFunc(func() bool {
			c := h.failed.Load()
			return c == nil || *c != o.class
		}), Reason: o.reason})
	}
	return append([]health.Check{
		{Signal: health.SignalFunc(func() bool {
			if h.failed.Load() != nil {
				return true
			}
			p := h.parts.Load()
			return p != nil && p.pool().Booted
		}), Reason: "the control plane is starting: its store has not opened or its first pool generation has not booted"},
		{Signal: health.SignalFunc(func() bool { return !h.draining.Load() }), Reason: "the control plane is shutting down"},
		{Signal: set(func(p *cpParts) bool { return p.store.Healthy() }), Reason: "the store's writes have kept failing for 2 minutes"},
		{Signal: set(func(p *cpParts) bool { return p.hosts.Healthy() }), Reason: "the hosts listener has stopped accepting connections"},
		{Signal: booted(func(ph pool.Health) bool { return ph.Passing }), Reason: "the pool's pass loop has stalled"},
		{Signal: booted(func(ph pool.Health) bool { return ph.Reconciling }), Reason: "a host's reconcile loop has stalled"},
		{Signal: set(func(p *cpParts) bool { return p.pool().Applied }), Reason: "a settings change has been applying for more than 10 minutes"},
		{Signal: set(func(p *cpParts) bool { return p.recorder.Healthy() }), Reason: "the history has not written its queued messages for 2 minutes"},
		{Signal: set(func(p *cpParts) bool { return p.housekeeper.Healthy() }), Reason: "the housekeeper has not finished a run for 2 hours"},
	}, outage...)
}
