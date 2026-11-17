package pool

import (
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// assigned has GitHub assign run's queued job to b as request id.
func (r *rig) assigned(b *book, run, id int64) {
	r.poll(b, github.Stats{Assigned: 1}, github.Event{Kind: github.Assigned, Name: "job", RunID: run, RequestID: id})
}

// waitedOf answers the reason handed with request id's assignment.
func (f *fakeRuns) waitedOf(t *testing.T, id int64) JobReason {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.e.RequestID == id && j.e.Kind == github.Assigned {
			return j.waited
		}
	}
	t.Fatalf("no assignment of request %d was handed over: %+v", id, f.jobs)
	return ""
}

// disconnect sets whether the rig's one host answers, then runs a pass.
func (r *rig) disconnect(down bool) {
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = down })
	r.pass()
}

// A job's assignment hands over the reason it waited under longest from
// its first listing to its assignment, whichever came first or last.
func TestAnAssignmentCarriesTheLongestWaitReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		down, serving time.Duration
		want          JobReason
	}{
		{name: "theEarlierReason", down: 10 * time.Minute, serving: time.Minute, want: JobNoHostServing},
		{name: "theLaterReason", down: time.Minute, serving: 10 * time.Minute, want: JobAwaitingAssignment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := quietRig(t, oneHost())
			r.disconnect(true)
			r.listed(app, "small", 1)
			r.now = r.now.Add(tc.down / 2)
			r.pass()
			r.now = r.now.Add(tc.down / 2)
			r.disconnect(false)
			r.now = r.now.Add(tc.serving)
			r.assigned(r.small(), 1, 11)
			if got := r.runs.waitedOf(t, 11); got != tc.want {
				t.Errorf("waited under %q, want %q", got, tc.want)
			}
		})
	}
}

// Only the assignment carries the reason; a job no pass or read saw
// waiting carries none.
func TestOnlyAWatchedAssignmentCarriesAReason(t *testing.T) {
	t.Parallel()
	t.Run("unlisted", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.pass()
		r.assigned(r.small(), 1, 11)
		if got := r.runs.waitedOf(t, 11); got != "" {
			t.Errorf("an unlisted job waited under %q, want none", got)
		}
	})
	t.Run("laterMessages", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.pass()
		r.listed(app, "small", 1)
		r.now = r.now.Add(time.Minute)
		r.assigned(r.small(), 1, 11)
		r.poll(r.small(), github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Name: "job", RunID: 1, RequestID: 11, Runner: "x"})
		if got := r.runs.waitedOf(t, 11); got != JobAwaitingAssignment {
			t.Errorf("the assignment waited under %q, want awaiting_assignment", got)
		}
		r.runs.mu.Lock()
		defer r.runs.mu.Unlock()
		for _, j := range r.runs.jobs {
			if j.e.Kind != github.Assigned && j.waited != "" {
				t.Errorf("a %v message carried %q, want only the assignment to", j.e.Kind, j.waited)
			}
		}
	})
}

// The reason times outlive a settings change: a generation takes over its
// scope's waiting jobs with what they waited under.
func TestTheLongestReasonSurvivesAGenerationChange(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.disconnect(true)
	r.listed(app, "small", 1)
	r.now = r.now.Add(10 * time.Minute)
	r.apply(cloneConfig(r.cfg))
	r.disconnect(false)
	r.now = r.now.Add(2 * time.Minute)
	r.assigned(r.small(), 1, 11)
	if got := r.runs.waitedOf(t, 11); got != JobNoHostServing {
		t.Errorf("waited under %q after a generation change, want the old generation's no_host_serving", got)
	}
}

// A reassigned job waits again, and its next assignment carries the
// longest reason over both waits; the time it spent assigned is no wait.
func TestAReassignedJobAddsItsSecondWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                      string
		serving, assigned, second time.Duration
		want                      JobReason
	}{
		{name: "bothWaitsCount", serving: 3 * time.Minute, second: 2 * time.Minute, want: JobAwaitingAssignment},
		{name: "theAssignedTimeIsNoWait", serving: time.Minute, assigned: 5 * time.Minute, second: 2 * time.Minute, want: JobNoHostServing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := quietRig(t, oneHost())
			r.disconnect(true)
			r.listed(app, "small", 1)
			r.now = r.now.Add(4 * time.Minute)
			r.disconnect(false)
			r.now = r.now.Add(tc.serving)
			r.assigned(r.small(), 1, 11)
			if got := r.runs.waitedOf(t, 11); got != JobNoHostServing {
				t.Fatalf("first assignment waited under %q, want no_host_serving", got)
			}
			r.now = r.now.Add(tc.assigned)
			r.poll(r.small(), github.Stats{}, github.Event{Kind: github.Completed, Name: "job", RunID: 1, RequestID: 11, Result: "canceled"})
			r.poll(r.small(), github.Stats{})
			r.pass()
			r.now = r.now.Add(tc.second)
			r.pass()
			r.assigned(r.small(), 1, 12)
			if got := r.runs.waitedOf(t, 12); got != tc.want {
				t.Errorf("second assignment waited under %q, want %q", got, tc.want)
			}
		})
	}
}

// A job's wait starts at the read that lists it, under the verdict then.
func TestAWaitStartsAtTheReadThatListsIt(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.pass()
	r.listed(app, "small", 1)
	r.now = r.now.Add(10 * time.Minute)
	r.disconnect(true)
	r.now = r.now.Add(3 * time.Minute)
	r.disconnect(false)
	r.assigned(r.small(), 1, 11)
	if got := r.runs.waitedOf(t, 11); got != JobAwaitingAssignment {
		t.Errorf("waited under %q, want the 10 minutes of awaiting_assignment from its read", got)
	}
}

// A job a whole read no longer lists is forgotten: the same name and run
// listed again starts its reasons afresh.
func TestAJobNoLongerListedIsForgotten(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.disconnect(true)
	r.listed(app, "small", 1)
	r.now = r.now.Add(10 * time.Minute)
	r.pass()
	r.listed(app, "small")
	r.disconnect(false)
	r.listed(app, "small", 1)
	r.now = r.now.Add(time.Minute)
	r.assigned(r.small(), 1, 11)
	if got := r.runs.waitedOf(t, 11); got != JobAwaitingAssignment {
		t.Errorf("waited under %q, want only the second listing's awaiting_assignment", got)
	}
}
