package pool

import (
	"slices"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// Snapshot is the pool's state at one moment, the source of its read
// series.
type Snapshot struct {
	// HeadSince is when the oldest claim on the queue began waiting, zero
	// with none.
	HeadSince time.Time
	Sets      []SetCounts
	Hosts     []HostCounts
	// Revision is the settings revision the generation serves.
	Revision uint64
}

// SetCounts sorts one scale set's jobs: assigned with no runner started
// (D9's backstop), or waiting, each waiting job queued, unassignable, held
// by an unserved scale set, or with no host to take it. A profile not
// allowed in a scope has no scale set there, so its jobs count as
// NotAllowed under the scope and profile alone.
type SetCounts struct {
	github.ScaleSet
	AssignedUnstarted, Queued, Unassignable, NotServing, NoHost, NotAllowed int
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
	snap := Snapshot{Revision: g.rev}
	if a := g.l.allocate(now); a.headFound {
		snap.HeadSince = a.headSince
	}
	for _, b := range g.l.books {
		snap.Sets = append(snap.Sets, g.l.jobCounts(b, now))
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

// jobCounts sorts b's jobs as of now.
func (l *ledger) jobCounts(b *book, now time.Time) SetCounts {
	st := b.asg.stats
	c := SetCounts{ScaleSet: b.ss, AssignedUnstarted: max(0, st.Assigned-st.Running)}
	for _, j := range b.asg.waiting(b.jobs) {
		switch {
		case now.Before(b.skipped[jobKey{j.Name, j.RunID}].until):
			c.Unassignable++
		case !b.fundable:
			c.NotServing++
		case !l.anyEligible(b.profile):
			c.NoHost++
		default:
			c.Queued++
		}
	}
	return c
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

// unserved counts the queued jobs of profiles with no scale set in their
// scope: not allowed there, or allowed and not yet ensured; g.mu is held.
func (g *generation) unserved() []SetCounts {
	var out []SetCounts
	for i := range g.cfg.Scopes {
		sc := g.scopes[g.cfg.Scopes[i].Key]
		for j := range g.cfg.Profiles {
			pr := &g.cfg.Profiles[j]
			n := len(sc.jobsOf(pr.Name))
			if n == 0 || g.bookOf(sc.cfg.Key, pr.Name) != nil {
				continue
			}
			c := SetCounts{Scope: sc.cfg.Key, Profile: pr.Name, NotServing: n}
			if !pr.Allowed(sc.cfg.Key) {
				c.NotServing, c.NotAllowed = 0, n
			}
			out = append(out, c)
		}
	}
	return out
}
