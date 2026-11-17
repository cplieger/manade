package pool

import (
	"time"

	"github.com/cplieger/manade/internal/github"
)

// waitKey names a queued job of one profile the way a read and an
// assignment both can.
type waitKey struct {
	profile string
	job     jobKey
}

// dwell is how long one queued job has waited under each reason since a
// pass or read first saw it waiting; reason is the verdict it waits under
// since at, empty while it is assigned.
type dwell struct {
	at     time.Time
	spent  map[JobReason]time.Duration
	reason JobReason
}

// longest answers the reason d waited under longest, empty for none.
func (d *dwell) longest() JobReason {
	var out JobReason
	for _, r := range JobReasons {
		if t, ok := d.spent[r]; ok && (out == "" || t > d.spent[out]) {
			out = r
		}
	}
	return out
}

// watch adds the time since each queued job's last verdict to that
// verdict's reason, and takes each one's verdict now from the allocator;
// g.mu is held.
func (g *generation) watch(now time.Time) {
	a := g.l.allocate(now)
	verdicts := map[*scope]map[waitKey]JobReason{}
	waiting := g.waiting(now, &a)
	for i := range waiting {
		w := &waiting[i]
		if w.Assigned {
			continue
		}
		sc := g.scopes[w.Scope]
		if verdicts[sc] == nil {
			verdicts[sc] = map[waitKey]JobReason{}
		}
		verdicts[sc][waitKey{w.Profile, jobKey{w.Name, w.RunID}}] = w.Reason
	}
	for _, sc := range g.scopes {
		sc.watch(now, verdicts[sc])
	}
}

// watch moves sc's waiting jobs to their verdicts at now, and forgets each
// job its last whole read no longer lists.
func (sc *scope) watch(now time.Time, verdicts map[waitKey]JobReason) {
	listed := map[waitKey]bool{}
	for _, j := range sc.jobs {
		listed[waitKey{j.Profile, jobKey{j.Name, j.RunID}}] = true
	}
	for k, d := range sc.waits {
		if !listed[k] {
			delete(sc.waits, k)
			continue
		}
		if d.reason != "" {
			d.spent[d.reason] += now.Sub(d.at)
		}
		d.at = now
		d.take(verdicts[k])
	}
	for k, r := range verdicts {
		if sc.waits[k] == nil {
			d := &dwell{at: now, spent: map[JobReason]time.Duration{}}
			d.take(r)
			sc.waits[k] = d
		}
	}
}

// take makes r the reason d waits under; an empty r waits under none.
func (d *dwell) take(r JobReason) {
	d.reason = r
	if _, ok := d.spent[r]; r != "" && !ok {
		d.spent[r] = 0
	}
}

// waited answers, for a message e of profile's scale set that assigns its
// job, the reason the job waited under longest and ends its wait; empty
// for any other message and for a job never seen waiting. A job GitHub
// takes back waits again from its next verdict, on the same times.
func (sc *scope) waited(profile string, e *github.Event) JobReason {
	if e.Kind != github.Assigned {
		return ""
	}
	d := sc.waits[waitKey{profile, jobKey{e.Name, e.RunID}}]
	if d == nil {
		return ""
	}
	d.reason = ""
	return d.longest()
}
