package pool

import (
	"slices"

	"github.com/cplieger/manade/internal/r5/github"
)

// jobKey names a job the way both a JobAssigned and the REST list can: its
// run and its display name. A message's job ID changes with every
// assignment, so nothing keys on it.
type jobKey struct {
	name string
	run  int64
}

// assignments is what GitHub has assigned to one scale set, as its messages
// and statistics report it.
type assignments struct {
	// named holds the assignments a message named that no later message
	// started, completed or cancelled.
	named map[jobKey]struct{}
	// kept holds the last poll's reassignment cancels, whose slots stay
	// reserved until the next poll returns.
	kept  map[jobKey]struct{}
	stats github.Stats
	// unbacked is how many assignments the previous process left that the
	// budget has not funded yet; they hold no reservation until owed funds
	// them.
	unbacked int
}

func newAssignments() *assignments {
	return &assignments{named: map[jobKey]struct{}{}, kept: map[jobKey]struct{}{}}
}

// owed is the slots the scale set's assignments hold: those with a
// reservation, and kept cancels. GitHub's count no longer holds a cancelled
// job, so a kept slot is counted on top of it, which leaves GitHub room to
// assign it again.
func (a *assignments) owed() int {
	return max(0, a.stats.Assigned-a.unbacked) + len(a.kept)
}

// committed is the slots owed beyond the live runners' own. A live runner
// holds the slot of the assignment it was started for, a kept one's too.
func (a *assignments) committed(live int) int {
	return max(0, a.owed()-live)
}

// apply takes one answered poll. The previous poll's kept cancels end
// with it: a job this poll assigns again is named and holds its slot
// through GitHub's count, and any other is waiting again.
// A cancel the same message reassigns is no kept cancel, in either order.
func (a *assignments) apply(p *github.Poll) {
	clear(a.kept)
	again := map[jobKey]bool{}
	for i := range p.Events {
		e := &p.Events[i]
		k := jobKey{e.Name, e.RunID}
		switch {
		case e.Kind == github.Assigned:
			a.named[k], again[k] = struct{}{}, true
		case e.Kind == github.Started:
			delete(a.named, k)
		case reassigned(e):
			_, ours := a.named[k]
			delete(a.named, k)
			if !ours && a.unbacked > 0 {
				a.unbacked--
				continue
			}
			a.kept[k] = struct{}{}
		default:
			delete(a.named, k)
		}
	}
	for k := range a.kept {
		if again[k] {
			delete(a.kept, k)
			a.named[k] = struct{}{}
		}
	}
	if p.Stats != nil {
		a.observe(*p.Stats)
	}
}

// observe takes new statistics. Unbacked assignments are among the
// assigned ones not yet running, so a fall in either bounds them.
func (a *assignments) observe(st github.Stats) {
	a.stats = st
	a.unbacked = min(a.unbacked, max(0, st.Assigned-st.Running))
}

// fitKept drops kept cancels past what slots, the slots reserved while the
// poll ran, leave beside the assignments it reports: GitHub may fill a
// cancelled job's slot with another job in the same message, and the job
// whose slot went then waits at once rather than hold a slot twice.
func (a *assignments) fitKept(slots int) {
	room := max(0, slots-max(0, a.stats.Assigned-a.unbacked))
	for k := range a.kept {
		if len(a.kept) <= room {
			return
		}
		delete(a.kept, k)
	}
}

// reassigned reports GitHub's reassignment cancel: canceled, with no runner
// and no finish time. It frees nothing, because GitHub assigns the job
// again on the next message.
func reassigned(e *github.Event) bool {
	return e.Kind == github.Completed && e.Result == "canceled" && e.Runner == "" && e.Finished.IsZero()
}

// waiting returns the scale set's waiting jobs among queued, its queued
// REST jobs: those no outstanding assignment names, less as many as the
// assignments no message names, taken from the newest so the scale set
// still ranks by its oldest unmatched job. A name two queued jobs of one
// run share names neither of them, and a kept cancel's job is assigned.
func (a *assignments) waiting(queued []github.QueuedJob) []github.QueuedJob {
	seen := map[jobKey]int{}
	for _, j := range queued {
		seen[jobKey{j.Name, j.RunID}]++
	}
	named := 0
	for k := range a.named {
		if seen[k] < 2 {
			named++
		}
	}
	var open []github.QueuedJob
	for _, j := range queued {
		k := jobKey{j.Name, j.RunID}
		_, n := a.named[k]
		_, kept := a.kept[k]
		if !kept && (!n || seen[k] > 1) {
			open = append(open, j)
		}
	}
	slices.SortStableFunc(open, func(x, y github.QueuedJob) int { return x.Created.Compare(y.Created) })
	unnamed := max(0, a.stats.Assigned-a.stats.Running-named)
	return open[:max(0, len(open)-unnamed)]
}

// prune forgets named assignments a complete read no longer lists as
// queued: their jobs started or ended, whether or not a message said so.
func (a *assignments) prune(queued []github.QueuedJob) {
	listed := map[jobKey]bool{}
	for _, j := range queued {
		listed[jobKey{j.Name, j.RunID}] = true
	}
	for k := range a.named {
		if !listed[k] {
			delete(a.named, k)
		}
	}
}
