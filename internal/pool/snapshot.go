package pool

import (
	"slices"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// Snapshot is the pool's state at one moment, the source of its read
// series and of the API's live reads.
type Snapshot struct {
	// HeadSince is when the oldest claim on the queue began waiting, zero
	// with none.
	HeadSince time.Time
	Sets      []SetCounts
	Hosts     []HostCounts
	// Waiting is every waiting job with its reason (§10.5).
	Waiting []Waiting
	// Stops is every stopping state of the settings (§9.4).
	Stops []Stop
	// Orphaned is every recorded scale set the settings would not ensure
	// (§9.3), sorted by scope and profile.
	Orphaned []Orphan
	// Revision is the settings revision the generation serves.
	Revision uint64
}

// SetCounts is one scale set's assignments with no runner started (D9's
// backstop) and its queued jobs the last read listed. A profile with no
// scale set in a scope has ID 0: one not serving the scope, or one whose
// ensure has not succeeded.
type SetCounts struct {
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

// Resources is a reservation in the settings' units.
type Resources struct{ MilliCPUs, Memory, Disk int64 }

// HostCounts is one host's reservation and its runners by scale set.
type HostCounts struct {
	Host     string
	Runners  []RunnerCounts
	Reserved Resources
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
	snap := Snapshot{Revision: g.rev, Stops: g.stops(), Orphaned: g.orphans(g.reg.orphaned(g.cfg))}
	a := g.l.allocate(now)
	if a.headFound {
		snap.HeadSince = a.headSince
	}
	snap.Waiting = g.waiting(now, &a)
	for _, b := range g.l.books {
		st := b.asg.stats
		snap.Sets = append(snap.Sets, SetCounts{ScaleSet: b.ss, AssignedUnstarted: max(0, st.Assigned-st.Running), Queued: len(b.asg.waiting(b.jobs))})
	}
	snap.Sets = append(snap.Sets, g.unserved()...)
	free := g.l.free()
	for h := range g.hosts {
		held := g.l.hosts[h].budget.minus(free[h])
		snap.Hosts = append(snap.Hosts, HostCounts{
			Host:     g.hosts[h].cfg.Name,
			Reserved: Resources{MilliCPUs: held.cpu, Memory: held.mem, Disk: held.disk},
			Runners:  g.l.runnerCounts(h),
		})
	}
	return snap
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
