package pool

import (
	"slices"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/r5/github"
)

// The allocation's timings: a standing slot's rotation, the shortest poll that
// proves a funded slot went unused, and the hold of an unassignable job,
// up to GitHub's 24-hour queue limit.
const (
	rotateAfter = 2 * time.Minute
	countedPoll = 30 * time.Second
	skipFirst   = 10 * time.Minute
	skipMax     = 24 * time.Hour
)

// Capacity answers the poll's maxCapacity: the scale set's slots, plus the
// jobs and standing slot the allocation funds it, or 0 at shutdown and for
// a scope proven public. A figure above the slots its assignments hold,
// idle runners included, needs a visibility proof under proofAge old;
// without one it is capped at those slots, and runProofs reads again at
// once for a stale proof or proofRetry after a failed read. Below its
// assignments only where some were never funded.
func (p *Pool) Capacity(ss github.ScaleSet, start time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.set(ss)
	s.flight = flight{}
	if p.stopping || s.scope.public {
		s.flight = flight{active: true}
		return 0
	}
	a, order, views := p.allocate(start)
	if p.rotate(s, start) {
		a, order, views = p.allocate(start)
	}
	i := slices.Index(order, s)
	v, g := &views[i], a.grants[i]
	f := flight{figure: v.slots, funded: min(len(v.open), v.covered), active: true}
	if v.fundable {
		f.figure += g.funded + b2i(g.standing)
		f.funded += g.funded
		f.standing = g.standing || v.covered > len(v.open)
	}
	if owed := s.asg.owed(); f.figure > owed {
		sc := s.scope
		sc.above = start
		if private := sc.private(); !private || start.Sub(sc.proof) >= proofAge {
			// A failed read is retried on its own clock (proofRetry).
			if private {
				p.wantProof(sc)
			}
			f = flight{figure: owed, active: true}
		}
	}
	switch {
	case f.standing && s.standSince.IsZero():
		s.standSince = start
	case !f.standing && !s.standSince.IsZero():
		s.standSince, s.standEnded = time.Time{}, start
	}
	s.flight = f
	return f.figure
}

// rotate ends an organization or enterprise scale set's standing slot
// still unfilled rotateAfter after the first poll carrying it began, so the
// next goes to the one whose last slot ended longest ago. While the
// budget covers all of them, the allocation that follows grants it again.
func (*Pool) rotate(s *set, start time.Time) bool {
	if !s.org() || s.standSince.IsZero() || start.Sub(s.standSince) < rotateAfter {
		return false
	}
	s.standSince, s.standEnded = time.Time{}, start
	return true
}

// Opened takes a new session's statistics. Assignments past the scale
// set's runners take reservations from the budget as far as it covers
// them; the rest are unbacked until the budget covers them (owed).
func (p *Pool) Opened(ss github.ScaleSet, st github.Stats) {
	p.mu.Lock()
	s := p.set(ss)
	s.open, s.opened, s.flight = true, true, flight{}
	need := max(0, st.Assigned+len(s.asg.kept)-len(s.runners))
	s.asg.unbacked = min(st.Assigned, need-p.fits(s, need))
	s.asg.observe(st)
	p.mu.Unlock()
	p.poke()
}

// fits counts how many of s's slots, up to n, the budget left after every
// other scale set's holdings and s's runners covers.
func (p *Pool) fits(s *set, n int) int {
	now := p.now()
	free, used := p.free(), 0
	for _, o := range p.sets {
		if o == s {
			continue
		}
		v := p.view(o, now)
		free = free.minus(v.held)
		if o.profile == s.profile {
			used += int(v.held.runners)
		}
	}
	for _, r := range s.runners {
		free, used = free.minus(r.res), used+1
	}
	for _, st := range p.strays {
		used += b2i(st.profile == s.profile)
	}
	slot, k := p.slot(s.profile), 0
	for k < n && slot.times(k+1).within(free) && (p.caps[s.profile] == 0 || used+k < p.caps[s.profile]) {
		k++
	}
	return k
}

// Polled takes an answered poll: its assignments, its runners' job
// messages, whether it filled the standing slot it carried, and whether it
// left a funded slot unfilled.
func (p *Pool) Polled(ss github.ScaleSet, poll *github.Poll) {
	p.mu.Lock()
	s := p.set(ss)
	reserved := max(len(s.runners), s.asg.owed())
	if s.flight.active {
		reserved = max(reserved, s.flight.figure)
	}
	s.asg.apply(poll)
	s.asg.fitKept(reserved)
	n := 0
	for i := range poll.Events {
		e := &poll.Events[i]
		switch e.Kind {
		case github.Assigned:
			n++
		case github.Started:
			p.started(s, e)
		case github.Completed:
			p.completed(s, e)
		}
	}
	if n > 0 {
		s.lastAssigned = poll.Ended
	}
	if s.flight.standing && n > s.flight.funded {
		s.standSince, s.standEnded = time.Time{}, poll.Ended
	}
	switch {
	case s.flight.funded == 0 || n > 0:
		s.misses = misses{}
	case poll.Idle || poll.Ended.Sub(poll.Started) >= countedPoll:
		if s.misses.n == 0 {
			s.misses.first = poll.Started
		}
		s.misses.n++
	}
	p.mu.Unlock()
	p.poke()
}

func (p *Pool) started(s *set, e *github.Event) {
	if r := s.runners[e.Runner]; r != nil {
		r.state = busy
	}
	if !e.Queued.IsZero() && !e.RunnerAssigned.IsZero() {
		p.m.JobAssigned(s.ss.Profile, e.RunnerAssigned.Sub(e.Queued))
	}
	p.log.Info("Job started", "scope", s.ss.Scope, "profile", s.ss.Profile, "runner", textsafe.SanitizeSingleLineBounded(e.Runner, 64),
		"run_id", e.RunID, "job", textsafe.SanitizeSingleLineBounded(e.Name, 128))
}

// completed records a job's end. A job no runner took has no outcome to
// record, a reassignment cancel among them.
func (p *Pool) completed(s *set, e *github.Event) {
	if e.Runner == "" {
		return
	}
	delete(s.spent, e.Runner)
	if r := s.runners[e.Runner]; r != nil {
		r.completed = true
	}
	if !e.RunnerAssigned.IsZero() && !e.Finished.IsZero() {
		p.m.JobFinished(s.ss.Profile, jobResult(e.Result), e.Finished.Sub(e.RunnerAssigned))
	}
	p.log.Info("Job finished", "scope", s.ss.Scope, "profile", s.ss.Profile, "runner", textsafe.SanitizeSingleLineBounded(e.Runner, 64),
		"run_id", e.RunID, "job", textsafe.SanitizeSingleLineBounded(e.Name, 128), "result", textsafe.SanitizeSingleLineBounded(e.Result, 32))
}

// Held records whether another process holds the scale set's session. A
// held scale set reports nothing, so its last figure no longer reserves.
func (p *Pool) Held(ss github.ScaleSet, held bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.set(ss)
	s.held = held
	if held {
		s.flight = flight{}
	}
	p.m.ScopeServing(s.scope.cfg.Key, p.serving(s.scope))
}

// Closed records that the scale set's session ended. Its last figure
// stays reserved until a session opens and reports the statistics.
func (p *Pool) Closed(ss github.ScaleSet) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.set(ss).open = false
}

// Beat refreshes the scale set's listener lease.
func (p *Pool) Beat(ss github.ScaleSet) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.set(ss).beat = p.now()
}

// queued takes one sweep's reads, the sweep ending at ended. A scope read
// in part keeps its last whole read and funds nothing new until a sweep
// reads it whole. A whole read forgets the assignments and unassignable
// jobs it no longer lists, and gives a scale set whose last two counted
// polls left a funded slot unfilled its verdict.
func (p *Pool) queued(ended time.Time, qs []github.Queue) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, q := range qs {
		sc := p.scopes[q.Scope]
		if sc == nil {
			continue
		}
		sc.partial = !q.Complete
		if !q.Complete {
			continue
		}
		sc.jobs = q.Jobs
		for _, s := range p.sets {
			if s.scope == sc {
				p.reread(s, sc.jobsOf(s.ss.Profile), ended)
			}
		}
	}
}

// reread takes a whole read of s's queued jobs.
func (p *Pool) reread(s *set, jobs []github.QueuedJob, ended time.Time) {
	s.asg.prune(jobs)
	listed := map[jobKey]bool{}
	for _, j := range jobs {
		listed[jobKey{j.Name, j.RunID}] = true
	}
	for k := range s.skipped {
		if !listed[k] {
			delete(s.skipped, k)
		}
	}
	if s.misses.n >= 2 && ended.After(s.misses.first) {
		p.skip(s, jobs, ended)
	}
}

// skip marks s's waiting jobs unassignable from ended: out of the queue
// for skipFirst, doubling with each further verdict up to skipMax.
func (p *Pool) skip(s *set, jobs []github.QueuedJob, ended time.Time) {
	s.misses = misses{}
	for _, j := range s.asg.waiting(jobs) {
		k := jobKey{j.Name, j.RunID}
		prev := s.skipped[k]
		if ended.Before(prev.until) {
			continue
		}
		wait := skipFirst
		if prev.wait > 0 {
			wait = min(2*prev.wait, skipMax)
		}
		s.skipped[k] = skip{until: ended.Add(wait), wait: wait}
		p.log.Warn("GitHub did not assign a queued job, so the pool skips it until its retry time",
			"scope", s.ss.Scope, "profile", s.ss.Profile, "run_id", j.RunID,
			"job", textsafe.SanitizeSingleLineBounded(j.Name, 128), "retry", ended.Add(wait).UTC().Format(time.RFC3339))
	}
}
