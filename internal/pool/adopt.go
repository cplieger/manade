package pool

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// adopt takes a runner host i lists that no record holds: a previous
// process's, one whose scale set or scope the settings no longer hold, or
// a stray a generation change left to be judged. An ended or failed one is
// removed, one still being started is held as starting, and a running one
// is judged by adoptRunning. It reports false while a runner that may
// hold a job is left with no reservation.
func (g *generation) adopt(ctx context.Context, i int, a *action) bool {
	lr := &a.lr
	sc := g.scopeConfig(lr.Scope)
	switch lr.State {
	case hostapi.StateExited, hostapi.StateFailed:
		if !g.endListed(ctx, i, lr) {
			return true
		}
		if a.b != nil && lr.State == hostapi.StateExited {
			g.d.Metrics.RunnerExited(g.hosts[i].cfg.Name, a.b.ss, recordlessExit(lr))
		}
		if sc != nil {
			g.unregister(ctx, lr.Scope, lr.Name, lr.RunnerID)
		}
		return true
	case hostapi.StateCreating, hostapi.StateStarting:
		g.hold(i, a, starting)
		return true
	}
	if sc == nil {
		g.hold(i, a, busy)
		g.log.Warn("A runner's scope is no longer in the settings, so it keeps its reservation until it ends",
			"host", g.hosts[i].cfg.Name, "scope", lr.Scope, "profile", lr.Profile, "runner", lr.Name)
		return true
	}
	return g.adoptRunning(ctx, i, a, sc)
}

// adoptRunning judges a running runner no record holds, in scope sc: busy
// is held with the limits its host built it with, and idle is deregistered
// and removed, since a scale set with no session gets it no job. One of a
// dropped scope GitHub does not answer for, or any with no credential to
// ask with, keeps its reservation until it ends or passes its lifetime, so
// no host waits on a scope it cannot read.
func (g *generation) adoptRunning(ctx context.Context, i int, a *action, sc *config.Scope) bool {
	lr := &a.lr
	dropped := g.scopes[lr.Scope] == nil
	gr, found, err := g.gh.REST.Runner(ctx, sc, lr.RunnerID)
	switch {
	case err != nil && (dropped || errors.Is(err, errNoCredential)):
		g.hold(i, a, busy)
		return true
	case err != nil:
		if ctx.Err() == nil {
			g.log.Warn("Reading a runner no record holds failed, so the pass retries it", "host", g.hosts[i].cfg.Name, "scope", lr.Scope, "runner", lr.Name, "error", err)
		}
		return false
	case !found:
		return g.remove(ctx, i, lr.Name)
	case gr.Busy:
		g.hold(i, a, busy)
		return true
	}
	switch err := g.gh.ScaleSets.RemoveRunner(ctx, sc, lr.RunnerID); {
	case err == nil:
		return g.remove(ctx, i, lr.Name)
	case errors.Is(err, github.ErrRunnerBusy):
		g.hold(i, a, busy)
		return true
	case dropped:
		g.hold(i, a, busy)
		return true
	default:
		if ctx.Err() == nil {
			g.log.Warn("Removing an idle runner no record holds failed, so the pass retries it", "host", g.hosts[i].cfg.Name, "scope", lr.Scope, "runner", lr.Name, "error", err)
		}
		return false
	}
}

// hold gives an adopted runner its record, or a stray's reservation when
// no scale set of the settings holds it, with the limits its host reports.
func (g *generation) hold(i int, a *action, state runnerState) {
	lr := &a.lr
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.limits(i, &lr.Limits)
	if a.b == nil {
		created := g.now()
		if st, held := g.l.strays[lr.Name]; held {
			created = st.created
		}
		g.l.hold(lr.Name, &stray{res: r, host: i, profile: g.profileIndex(lr.Profile), created: created, scope: lr.Scope})
		g.l.settle(g.now())
		return
	}
	if _, taken := a.b.runners[lr.Name]; taken {
		return
	}
	rr := a.b.adopt(lr.Name, i, r, state)
	// The lifetime counts from adoption: a host's clock is never compared
	// with this one's.
	rr.id, rr.instance, rr.answered, rr.counted, rr.created = lr.RunnerID, a.inst, a.begin, state == busy, g.now()
	g.l.settle(g.now())
	g.log.Info("Adopted a runner", "host", g.hosts[i].cfg.Name, "scope", lr.Scope, "profile", lr.Profile, "runner", lr.Name, "state", string(lr.State))
}

// endStray removes an ended runner of no scale set the settings hold and
// frees its reservation.
func (g *generation) endStray(ctx context.Context, i int, lr *hostapi.Runner) {
	if !g.endListed(ctx, i, lr) {
		return
	}
	g.mu.Lock()
	g.l.release(lr.Name, g.now())
	g.mu.Unlock()
	g.unregister(ctx, lr.Scope, lr.Name, lr.RunnerID)
}

// endListed removes an ended runner no record holds, handing an exited
// one's end to the run history with its evidence, and reports whether its
// container is gone.
func (g *generation) endListed(ctx context.Context, i int, lr *hostapi.Runner) bool {
	if lr.State != hostapi.StateExited {
		return g.remove(ctx, i, lr.Name)
	}
	reason := recordlessExit(lr)
	ev, ok := g.endContainer(ctx, i, lr.Name, reason, lr.ExitCode)
	if ok {
		g.exited(ctx, g.hosts[i].cfg.Name, lr.Name, reason, &ev)
	}
	return ok
}

// limits is the reservation of a runner built with lim on host i: its
// pinned CPUs or quota, memory, and its disk where the host's budget
// counts disk; g.mu is held.
func (g *generation) limits(i int, lim *hostapi.Limits) res {
	r := res{cpu: lim.MilliCPUs, mem: lim.Memory, runners: 1}
	if lim.CPUSet != "" {
		r.cpu = 1000 * int64(cpusIn(lim.CPUSet))
	}
	if g.l.hosts[i].disk {
		r.disk = lim.Disk
	}
	return r
}

// cpusIn counts the CPUs of a cpuset list such as "0-1,3"; a malformed
// part counts none.
func cpusIn(set string) int {
	n := 0
	for part := range strings.SplitSeq(set, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				continue
			}
		}
		n += b - a + 1
	}
	return n
}
