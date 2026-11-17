package main

import (
	"net/http"
	"sync/atomic"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/manade/internal/pool"
)

// cpParts is what the control plane's checks read once its store has
// opened. Each signal holds its own lease.
type cpParts struct {
	store, hosts, recorder, housekeeper health.Signal
	pool                                func() pool.Health
}

// cpHealth is the control plane's /healthz (§12.3): one check per loop,
// each with its fixed reason. It reads starting until the store opens and
// the first generation boots, and draining from BeginDrain on; until the
// parts are set, every other check passes, so the starting reason stands
// alone.
type cpHealth struct {
	parts    atomic.Pointer[cpParts]
	draining atomic.Bool
}

// ready hands the checks their parts.
func (h *cpHealth) ready(p *cpParts) { h.parts.Store(p) }

// BeginDrain makes /healthz answer 503 for good; call it when the
// shutdown begins.
func (h *cpHealth) BeginDrain() { h.draining.Store(true) }

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
	return []health.Check{
		{Signal: health.SignalFunc(func() bool {
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
	}
}
