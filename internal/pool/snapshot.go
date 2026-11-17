package pool

import (
	"slices"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// Snapshot is the pool's state at one moment, At, the source of its read
// series and of the API's live reads. The zero Snapshot is a pool with no
// generation yet.
type Snapshot struct {
	// HeadSince is when the oldest claim on the queue began waiting, zero
	// with none.
	HeadSince, At time.Time
	Sets          []SetCounts
	Hosts         []HostView
	// Profiles is the settings' profile names, in settings order.
	Profiles []string
	// Waiting is every waiting job with its reason.
	Waiting []Waiting
	// Stops is every stopping state of the settings.
	Stops []Stop
	// Orphaned is every recorded scale set the settings would not ensure,
	// sorted by scope and profile.
	Orphaned []Orphan
	// Routes is every repository and organization scope's routing
	// variable, in settings order.
	Routes []RouteStatus
	// Scopes is every scope's row, in settings order.
	Scopes []ScopeStatus
	// Revision is the settings revision the generation serves.
	Revision uint64
	// Booted reports that the first generation's boot pass has run.
	Booted bool
}

// SetCounts is one scale set's assignments with no runner started, its
// queued jobs the last read listed and when its last good
// poll ended, zero before one. A profile with no scale set in a scope has
// ID 0: one not serving the scope, or one whose ensure has not succeeded.
type SetCounts struct {
	Polled time.Time
	github.ScaleSet
	AssignedUnstarted, Queued int
}

// Orphan is a recorded scale set the settings would not ensure, with the
// queued jobs of its profile the last read of its scope listed: none once
// the settings hold no such scope, whose jobs no read sees.
type Orphan struct {
	github.ScaleSet
	Queued int
}

// ScopeStatus is one scope's row: served, or the scope_not_served detail
// that keeps it from being served, with each scale set another pool holds
// and the owner GitHub named for it. A repository scope's ReadAt is
// when the last whole read of its queued jobs ended and ReadDue when that
// read named the next, both zero before one.
type ScopeStatus struct {
	ReadAt, ReadDue time.Time
	Scope           string
	Detail          string
	Held            []HeldSet
	Kind            config.ScopeKind
	Serving         bool
}

// HeldSet is a profile's scale set whose session another pool holds.
type HeldSet struct{ Profile, Owner string }

// Resources is a reservation in the settings' units.
type Resources struct{ MilliCPUs, Memory, Disk int64 }

// HostView is one host of the settings as the pool sees it.
type HostView struct {
	// ProvedAt is when the passing proof of the status's fingerprint
	// passed, zero while none holds; ProbeUntil is the deadline of a probe
	// in flight, zero while none runs.
	ProvedAt, ProbeUntil time.Time
	// Reconciled is when the host's reconcile loop last ended a pass, and
	// ReconcileDue the deadline of its lease, which time inside a host or
	// GitHub call does not count against; both are zero before its first.
	Reconciled, ReconcileDue time.Time
	// Status is the last status read while the host answers, nil while it
	// does not; Seen is the last any pass read, nil before one.
	Status, Seen *hostapi.Status
	// Probe is the last probe's report, nil before one answered.
	Probe *hostapi.ProbeReport
	// Capacity is the budget the pool funds the host to, nil while a host
	// whose settings set none has reported none; Disk is 0 with no
	// capacity.disk.
	Capacity *Resources
	Host     string
	// Profiles is the settings' profiles that list the host.
	Profiles []string
	Runners  []RunnerCounts
	Reserved Resources
	// ImageRefresh is the host's images.refresh.
	ImageRefresh time.Duration
	// Connected reports an open connection; Current, Status running the
	// section the generation delivers the host; ProbeFailed, that the last
	// completed probe of the status's fingerprint failed and none passed
	// since.
	Connected, Serving, Accepting, EgressOpen, Current, ProbeFailed bool
}

// RunnerCounts counts one scale set's runners on a host by state.
type RunnerCounts struct {
	github.ScaleSet
	Starting, Idle, Busy int
}

// snapshot reads the generation's state now.
func (g *generation) snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	snap := Snapshot{
		At: now, Revision: g.rev, Booted: !g.booting, Stops: g.stops(), Orphaned: g.orphans(g.reg.orphaned(g.cfg)),
		Routes: g.routeStatuses(), Scopes: g.scopeStatuses(),
	}
	for i := range g.cfg.Profiles {
		snap.Profiles = append(snap.Profiles, g.cfg.Profiles[i].Name)
	}
	a := g.l.allocate(now)
	if a.headFound {
		snap.HeadSince = a.headSince
	}
	snap.Waiting = g.waiting(now, &a)
	for _, b := range g.l.books {
		st := b.asg.stats
		snap.Sets = append(snap.Sets, SetCounts{ScaleSet: b.ss, AssignedUnstarted: max(0, st.Assigned-st.Running), Queued: len(b.asg.waiting(b.jobs)), Polled: g.sets[b].polled})
	}
	snap.Sets = append(snap.Sets, g.unserved()...)
	free := g.l.free()
	for h := range g.hosts {
		held := g.l.hosts[h].budget.minus(free[h])
		v := g.hostView(h, now)
		v.Reserved, v.Runners = Resources{MilliCPUs: held.cpu, Memory: held.mem, Disk: held.disk}, g.l.runnerCounts(h)
		snap.Hosts = append(snap.Hosts, v)
	}
	return snap
}

// hostView is host i's view at now, its reservation and runners aside;
// g.mu is held.
func (g *generation) hostView(i int, now time.Time) HostView {
	h, hb := g.hosts[i], &g.l.hosts[i]
	_, connected := g.d.Links.Host(h.cfg.Name)
	v := HostView{
		Host: h.cfg.Name, Seen: h.st, Probe: h.report, Reconciled: h.passed, ReconcileDue: h.beat.Add(ReconcileLease),
		ImageRefresh: h.cfg.ImageRefresh, Connected: connected, Serving: h.serving(now), Accepting: h.cfg.Accepting,
		EgressOpen: h.cfg.Egress.Open, ProbeFailed: h.fprint != "" && h.failed == h.fprint,
	}
	switch {
	case h.waiting:
		v.ReconcileDue = now.Add(ReconcileLease)
	case h.beat.IsZero():
		v.ReconcileDue = time.Time{}
	}
	if h.reachable {
		v.Status, v.Current = h.st, h.current
	}
	if h.proved != "" && h.proved == h.fprint {
		v.ProvedAt = h.provedAt
	}
	if h.probing != "" {
		v.ProbeUntil = h.probeUntil
	}
	if !hb.reported || h.st != nil {
		c := Resources{MilliCPUs: hb.budget.cpu, Memory: hb.budget.mem}
		if hb.disk {
			c.Disk = hb.budget.disk
		}
		v.Capacity = &c
	}
	for j := range g.cfg.Profiles {
		if slices.Contains(g.cfg.Profiles[j].Hosts, h.cfg.Name) {
			v.Profiles = append(v.Profiles, g.cfg.Profiles[j].Name)
		}
	}
	return v
}

// scopeStatuses answers every scope's row; g.mu is held.
func (g *generation) scopeStatuses() []ScopeStatus {
	out := make([]ScopeStatus, 0, len(g.cfg.Scopes))
	for i := range g.cfg.Scopes {
		sc := g.scopes[g.cfg.Scopes[i].Key]
		st := ScopeStatus{Scope: sc.cfg.Key, Kind: sc.cfg.Kind, Detail: g.scopeDetail(sc), ReadAt: sc.readAt, ReadDue: sc.readDue}
		st.Serving = st.Detail == ""
		for _, b := range g.l.books {
			if s := g.sets[b]; s.scope == sc && s.held {
				st.Held = append(st.Held, HeldSet{Profile: b.ss.Profile, Owner: s.holder})
			}
		}
		out = append(out, st)
	}
	return out
}

// runnerCounts counts, by state, the runners of each scale set whose
// profile lists host h, or that runs there.
func (l *ledger) runnerCounts(h int) []RunnerCounts {
	var out []RunnerCounts
	for _, b := range l.books {
		c := RunnerCounts{ScaleSet: b.ss}
		listed := slices.Contains(l.profiles[b.profile].hosts, h)
		for _, r := range b.runners {
			if r.host != h {
				continue
			}
			listed = true
			switch r.state {
			case starting:
				c.Starting++
			case idle:
				c.Idle++
			case busy:
				c.Busy++
			}
		}
		if listed {
			out = append(out, c)
		}
	}
	return out
}

// orphans counts the queued jobs of each orphaned scale set the scope's
// last read lists; g.mu is held.
func (g *generation) orphans(sets []github.ScaleSet) []Orphan {
	out := make([]Orphan, 0, len(sets))
	for _, ss := range sets {
		o := Orphan{ScaleSet: ss}
		if sc := g.scopeOf(ss.Scope); sc != nil {
			o.Queued = len(sc.jobsOf(ss.Profile))
		}
		out = append(out, o)
	}
	return out
}

// unserved counts the queued jobs of profiles with no scale set in their
// scope; g.mu is held.
func (g *generation) unserved() []SetCounts {
	var out []SetCounts
	for i := range g.cfg.Scopes {
		sc := g.scopes[g.cfg.Scopes[i].Key]
		for j := range g.cfg.Profiles {
			pr := &g.cfg.Profiles[j]
			if n := len(sc.jobsOf(pr.Name)); n > 0 && g.bookOf(sc.cfg.Key, pr.Name) == nil {
				out = append(out, SetCounts{Scope: sc.cfg.Key, Profile: pr.Name, Queued: n})
			}
		}
	}
	return out
}
