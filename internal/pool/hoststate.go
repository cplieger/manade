package pool

import (
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

// HostHistory takes what the history records of each host: its
// state as the pool serves it, each status a pass read and each proof. Its
// methods return at once and are safe for concurrent use.
type HostHistory interface {
	// State takes a change of host's state, with its detail; an empty
	// state is a gap, a time the pool does not observe the host.
	State(host string, at time.Time, state HostState, detail string)
	Status(host string, at time.Time, st *hostapi.Status)
	// Proved takes a proof that completed: rep is nil when the probe
	// request failed, and failure is the verdict's reason, empty when it
	// passed.
	Proved(host string, at time.Time, rep *hostapi.ProbeReport, failure string)
}

// HostState is a host's state as the pool serves it: ready, or the first
// HostReason that stands for the whole host.
type HostState string

// hostReady is a host that serves.
const hostReady HostState = "ready"

// DetailHostRemoved is the gap of a host the settings no longer hold.
const DetailHostRemoved = "removed"

// stateOf is h's state and its detail at now.
func stateOf(h *host, now time.Time) (state HostState, detail string) {
	switch {
	case h.serving(now):
		return hostReady, ""
	case !h.reachable || !h.cfg.Accepting || !h.current || h.proof(now) != proofHeld:
		r, d := unserved(h, now)
		if r == hostUnproved {
			d = h.failure
		}
		return HostState(r), d
	case !h.settled:
		return HostState(hostNotReady), detailAdopting
	}
	return HostState(hostNotReady), detailCapacityOverLimits
}

// noteState hands the history host i's state when it changed since the
// last it was handed; g.mu is held.
func (g *generation) noteState(i int) {
	h := g.hosts[i]
	now := g.now()
	s, d := stateOf(h, now)
	if s == h.noted && d == h.notedDetail {
		return
	}
	h.noted, h.notedDetail = s, d
	g.d.Hosts.State(h.cfg.Name, now, s, d)
}
