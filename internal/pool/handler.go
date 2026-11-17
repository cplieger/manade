package pool

import (
	"strings"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/github"
)

// capacity answers the poll's maxCapacity (ledger.capacity owns the
// figure), 0 at shutdown, for a scope proven public and for a scale set
// the generation does not hold. A figure above the slots its assignments
// hold needs a visibility proof under proofAge old; without one it is cut
// to those slots, and the visibility loop reads again at once.
func (g *generation) capacity(ss github.ScaleSet, start time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, s := g.book(ss)
	if b == nil {
		return 0
	}
	g.gate()
	sc := s.scope
	fresh := sc.private() && start.Sub(sc.proof) < proofAge
	figure, above := g.l.capacity(b, start, fresh)
	if above {
		sc.above = start
		if !fresh && sc.private() {
			g.wantProof(sc)
		}
	}
	g.poke()
	return figure
}

// opened takes a new session's statistics: its assignments get slots as
// far as the budget covers them, and the rest are funded first as it
// frees, since GitHub never cancels an assignment of a scale set that
// keeps polling.
func (g *generation) opened(ss github.ScaleSet, st github.Stats) {
	g.mu.Lock()
	b, s := g.book(ss)
	if b == nil {
		g.mu.Unlock()
		return
	}
	s.opened = true
	g.gate()
	g.l.opened(b, st, g.now())
	g.mu.Unlock()
	g.poke()
}

// polled takes an answered poll: its assignments and its runners' job
// messages.
func (g *generation) polled(ss github.ScaleSet, poll *github.Poll) {
	g.mu.Lock()
	b, s := g.book(ss)
	if b == nil {
		g.mu.Unlock()
		return
	}
	g.gate()
	g.l.polled(b, poll)
	for i := range poll.Events {
		e := &poll.Events[i]
		switch e.Kind {
		case github.Started:
			if !e.Queued.IsZero() && !e.RunnerAssigned.IsZero() {
				g.d.Metrics.JobAssigned(ss.Profile, e.RunnerAssigned.Sub(e.Queued))
			}
			g.log.Info("Job started", "scope", ss.Scope, "profile", ss.Profile, "runner", textsafe.SanitizeSingleLineBounded(e.Runner, 64),
				"run_id", e.RunID, "job", textsafe.SanitizeSingleLineBounded(e.Name, 128))
		case github.Completed:
			if e.Runner == "" {
				continue
			}
			delete(s.scope.unregister, e.Runner)
			if !e.RunnerAssigned.IsZero() && !e.Finished.IsZero() {
				g.d.Metrics.JobFinished(ss.Profile, jobResult(e.Result), e.Finished.Sub(e.RunnerAssigned))
			}
			g.log.Info("Job finished", "scope", ss.Scope, "profile", ss.Profile, "runner", textsafe.SanitizeSingleLineBounded(e.Runner, 64),
				"run_id", e.RunID, "job", textsafe.SanitizeSingleLineBounded(e.Name, 128), "result", textsafe.SanitizeSingleLineBounded(e.Result, 32))
		}
	}
	g.mu.Unlock()
	g.poke()
}

// held records whether another process holds the scale set's session. A
// held scale set reports nothing, so its last figure no longer reserves.
func (g *generation) held(ss github.ScaleSet, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, s := g.book(ss)
	if b == nil {
		return
	}
	s.held = held
	g.gate()
	if held {
		g.l.dropFlight(b, g.now())
	}
	g.d.Metrics.ScopeServing(ss.Scope, g.serving(s.scope))
}

// beat refreshes the scale set's listener lease.
func (g *generation) beat(ss github.ScaleSet) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, s := g.book(ss); s != nil {
		s.beat = g.now()
	}
}

// serving reports whether sc is served: its scale sets ensured, private,
// and no session of it held by another process; g.mu is held.
func (g *generation) serving(sc *scope) bool {
	if sc.ensureErr != nil || !sc.private() {
		return false
	}
	for _, s := range g.sets {
		if s.scope == sc && s.held {
			return false
		}
	}
	return true
}

// queued takes one sweep's reads, the sweep ending at ended. A scope read
// in part keeps its last whole read and funds nothing new until a sweep
// reads it whole.
func (g *generation) queued(ended time.Time, qs []github.Queue) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, q := range qs {
		sc := g.scopes[q.Scope]
		if sc == nil {
			continue
		}
		sc.partial = !q.Complete
		if !q.Complete {
			continue
		}
		sc.jobs, sc.readAt = q.Jobs, ended
		for _, b := range g.l.books {
			if g.sets[b].scope != sc {
				continue
			}
			for _, sk := range g.l.reread(b, sc.jobsOf(b.ss.Profile), ended) {
				g.log.Warn("GitHub did not assign a queued job, so the pool skips it until its retry time",
					"scope", b.ss.Scope, "profile", b.ss.Profile, "run_id", sk.job.RunID,
					"job", textsafe.SanitizeSingleLineBounded(sk.job.Name, 128), "retry", sk.until.UTC().Format(time.RFC3339))
			}
		}
	}
	g.gate()
}

// jobResult maps a JobCompleted's result to the pool's vocabulary.
func jobResult(r string) JobResult {
	switch strings.ToLower(r) {
	case "succeeded":
		return JobSucceeded
	case "failed":
		return JobFailed
	case "canceled":
		return JobCanceled
	}
	return JobOther
}
