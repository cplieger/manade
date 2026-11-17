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
	b, _ := g.book(ss)
	if b == nil {
		g.mu.Unlock()
		return
	}
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
	g.watch(g.now())
	s.polled = poll.Ended
	g.l.polled(b, poll)
	for i := range poll.Events {
		e := &poll.Events[i]
		g.d.Runs.Job(ss, g.runnerHost(b, e.Runner), e, s.scope.waited(ss.Profile, e))
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

// runnerHost names the host holding runner name of b, or a stray's, empty
// when no record holds it; g.mu is held.
func (g *generation) runnerHost(b *book, name string) string {
	if r, ok := b.runners[name]; ok {
		return g.hosts[r.host].cfg.Name
	}
	if st, ok := g.l.strays[name]; ok {
		return g.hosts[st.host].cfg.Name
	}
	return ""
}

// held records whether another process holds the scale set's session, and
// the holder GitHub named. A held scale set reports nothing, so its last
// figure no longer reserves.
func (g *generation) held(ss github.ScaleSet, held bool, owner string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, s := g.book(ss)
	if b == nil {
		return
	}
	s.held, s.holder = held, owner
	g.gate()
	if held {
		g.l.dropFlight(b, g.now())
	}
}

// lost records that GitHub no longer has the scale set: it reports
// nothing, so its last figure no longer reserves, and the scope's ensure
// runs at once.
func (g *generation) lost(ss github.ScaleSet) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, s := g.book(ss)
	if b == nil {
		return
	}
	s.lost, s.held, s.holder = true, false, ""
	g.gate()
	g.l.dropFlight(b, g.now())
	wake(g.reensure)
}

// serving reports whether sc is served: scopeDetail names no reason it is
// not; g.mu is held.
func (g *generation) serving(sc *scope) bool { return g.scopeDetail(sc) == "" }

// scopeDetail names why sc is not served, the first that holds, or none;
// g.mu is held.
func (g *generation) scopeDetail(sc *scope) string {
	anySet := func(f func(*set) bool) bool {
		for _, s := range g.sets {
			if s.scope == sc && f(s) {
				return true
			}
		}
		return false
	}
	switch {
	case g.stopping:
		return DetailPoolStopping
	case !g.credentialed:
		return DetailNotConnected
	case sc.ensureErr != nil:
		return ensureDetail(sc.ensureErr)
	case sc.public:
		return DetailPublic
	case anySet(func(s *set) bool { return s.lost }):
		return DetailScaleSetLost
	case anySet(func(s *set) bool { return s.held }):
		return DetailHeldByOtherPool
	case !sc.private():
		return DetailVisibilityUnproved
	}
	return ""
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
		sc.jobs, sc.readAt, sc.readDue = q.Jobs, ended, q.Next
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
	g.watch(g.now())
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
