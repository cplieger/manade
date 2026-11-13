package pool

import (
	"slices"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/r5/github"
)

func queuedJob(run int64, name string, minute int) github.QueuedJob {
	return github.QueuedJob{RunID: run, Name: name, Profile: "default", Created: epoch.Add(time.Duration(minute) * time.Minute)}
}

func assigned(run int64, name string) github.Event {
	return github.Event{Kind: github.Assigned, RunID: run, Name: name}
}

// cancelled is GitHub's reassignment cancel.
func cancelled(run int64, name string) github.Event {
	return github.Event{Kind: github.Completed, Result: "canceled", RunID: run, Name: name}
}

func names(jobs []github.QueuedJob) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.Name
	}
	return out
}

func poll(st *github.Stats, events ...github.Event) *github.Poll {
	return &github.Poll{Stats: st, Events: events}
}

func TestWaitingMatchesAssignmentsByRunAndName(t *testing.T) {
	// Listed out of queue order, as the sweep may list them.
	queued := []github.QueuedJob{queuedJob(3, "docs", 3), queuedJob(1, "lint", 0), queuedJob(2, "lint", 2), queuedJob(1, "test", 1)}
	t.Run("aNamedAssignmentLeavesTheQueue", func(t *testing.T) {
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 1}, assigned(2, "lint")))
		if got, want := names(a.waiting(queued)), []string{"lint", "test", "docs"}; !slices.Equal(got, want) {
			t.Errorf("waiting = %q, want %q", got, want)
		}
	})
	t.Run("unnamedAssignmentsTakeTheNewestUnmatched", func(t *testing.T) {
		// Two assignments a previous process received: the scale set still
		// ranks by its oldest unmatched job.
		a := newAssignments()
		a.observe(github.Stats{Assigned: 2})
		got := a.waiting(queued)
		if want := []github.QueuedJob{queued[1], queued[3]}; !slices.Equal(got, want) {
			t.Errorf("waiting = %v, want the two oldest", got)
		}
	})
	t.Run("runningJobsAreNotWaitingOnes", func(t *testing.T) {
		a := newAssignments()
		a.observe(github.Stats{Assigned: 3, Running: 2})
		if got := len(a.waiting(queued)); got != 3 {
			t.Errorf("waiting = %d jobs, want 3: one assignment not running", got)
		}
	})
	t.Run("namedOnesAreNotCountedTwice", func(t *testing.T) {
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 2}, assigned(3, "docs")))
		if got, want := names(a.waiting(queued)), []string{"lint", "test"}; !slices.Equal(got, want) {
			t.Errorf("waiting = %q, want %q: docs named, one unnamed taken from the newest", got, want)
		}
	})
	t.Run("aNameTwoJobsShareNamesNeither", func(t *testing.T) {
		twins := []github.QueuedJob{queuedJob(5, "build", 0), queuedJob(5, "build", 1), queuedJob(6, "lint", 2)}
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 1}, assigned(5, "build")))
		got := a.waiting(twins)
		if want := []github.QueuedJob{twins[0], twins[1]}; !slices.Equal(got, want) {
			t.Errorf("waiting = %v, want both builds kept and the newest job counted as the assignment", got)
		}
	})
	t.Run("theUnnamedCountFloorsAtZero", func(t *testing.T) {
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 1}, assigned(1, "lint"), assigned(1, "test")))
		if got := len(a.waiting(queued)); got != 2 {
			t.Errorf("waiting = %d jobs, want 2: more named than counted assigns no extra job", got)
		}
	})
	t.Run("aStartedJobIsNoLongerNamed", func(t *testing.T) {
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 1}, assigned(1, "lint")))
		a.apply(poll(&github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, RunID: 1, Name: "lint"}))
		if len(a.named) != 0 {
			t.Errorf("named = %v after the job started, want none", a.named)
		}
	})
	t.Run("aCompleteReadPrunesWhatItNoLongerLists", func(t *testing.T) {
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 2}, assigned(1, "lint"), assigned(9, "gone")))
		a.prune(queued)
		if _, ok := a.named[jobKey{"gone", 9}]; ok || len(a.named) != 1 {
			t.Errorf("named = %v, want only the listed lint", a.named)
		}
	})
}

func TestReassignmentCancelKeepsItsSlot(t *testing.T) {
	queued := []github.QueuedJob{queuedJob(1, "lint", 0), queuedJob(2, "test", 1)}
	setup := func() *assignments {
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 2}, assigned(1, "lint"), assigned(2, "test")))
		a.apply(poll(&github.Stats{Assigned: 1}, cancelled(1, "lint")))
		return a
	}
	t.Run("untilTheNextPollTheJobIsAssigned", func(t *testing.T) {
		a := setup()
		if got := names(a.waiting(queued)); len(got) != 0 {
			t.Errorf("waiting = %q, want none: the cancelled job stays assigned", got)
		}
		// GitHub counts one assignment now; the kept slot is reported on top.
		if got := a.committed(0); got != 2 {
			t.Errorf("committed = %d, want 2: one assignment and the kept slot", got)
		}
	})
	t.Run("aKeptJobIsNotSubtractedAsNamed", func(t *testing.T) {
		a := setup()
		a.observe(github.Stats{Assigned: 2})
		// test is named and lint kept, so the second assignment GitHub counts
		// is unnamed: it took docs, which must not be funded a second slot.
		withDocs := append(slices.Clone(queued), queuedJob(3, "docs", 2))
		if got := names(a.waiting(withDocs)); len(got) != 0 {
			t.Errorf("waiting = %q, want none", got)
		}
	})
	t.Run("reassignedOnTheNextPollTheSlotStays", func(t *testing.T) {
		a := setup()
		a.apply(poll(&github.Stats{Assigned: 2}, assigned(1, "lint")))
		if len(a.kept) != 0 || a.committed(0) != 2 {
			t.Errorf("kept = %v, committed = %d; want none kept and 2 committed", a.kept, a.committed(0))
		}
		if got := len(a.waiting(queued)); got != 0 {
			t.Errorf("waiting = %d jobs, want 0", got)
		}
	})
	// A message can carry a cancel and the job's reassignment together, in
	// either order: GitHub's count then holds the job, and a kept slot on
	// top would hold it twice.
	for _, tc := range []struct {
		name   string
		events []github.Event
	}{
		{name: "cancelThenReassignedInOneMessageHoldsOneSlot", events: []github.Event{cancelled(1, "lint"), assigned(1, "lint")}},
		{name: "reassignedThenCancelInOneMessageHoldsOneSlot", events: []github.Event{assigned(1, "lint"), cancelled(1, "lint")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAssignments()
			a.apply(poll(&github.Stats{Assigned: 2}, assigned(1, "lint"), assigned(2, "test")))
			a.apply(poll(&github.Stats{Assigned: 2}, tc.events...))
			if len(a.kept) != 0 || a.committed(0) != 2 {
				t.Errorf("kept = %v, committed = %d; want none kept and 2 committed", a.kept, a.committed(0))
			}
			if got := len(a.waiting(queued)); got != 0 {
				t.Errorf("waiting = %d jobs, want 0: lint is named", got)
			}
		})
	}
	t.Run("notReassignedTheJobWaitsAgain", func(t *testing.T) {
		a := setup()
		a.apply(poll(nil))
		if got, want := names(a.waiting(queued)), []string{"lint"}; !slices.Equal(got, want) {
			t.Errorf("waiting = %q, want %q", got, want)
		}
		if got := a.committed(0); got != 1 {
			t.Errorf("committed = %d, want 1: the kept slot is freed", got)
		}
	})
	t.Run("aUsersCancelFreesAtOnce", func(t *testing.T) {
		a := newAssignments()
		a.apply(poll(&github.Stats{Assigned: 1}, assigned(1, "lint")))
		e := cancelled(1, "lint")
		e.Finished = epoch
		a.apply(poll(&github.Stats{}, e))
		if len(a.kept) != 0 || a.committed(0) != 0 {
			t.Errorf("kept = %v, committed = %d; want nothing held", a.kept, a.committed(0))
		}
	})
	t.Run("anUnfundedCancelWaitsAtOnce", func(t *testing.T) {
		// A boot past the budget: three assignments, one funded.
		a := newAssignments()
		a.observe(github.Stats{Assigned: 3})
		a.unbacked = 2
		a.apply(poll(&github.Stats{Assigned: 2}, cancelled(1, "lint")))
		if len(a.kept) != 0 || a.unbacked != 1 {
			t.Errorf("kept = %v, unbacked = %d; want nothing kept and one unfunded left", a.kept, a.unbacked)
		}
		if got := a.committed(0); got != 1 {
			t.Errorf("committed = %d, want 1: no slot put back for the unfunded cancel", got)
		}
	})
}

func TestCommittedCountsEachAssignmentOnce(t *testing.T) {
	// A runner started for an assigned job is live before GitHub counts its
	// job as running, so it holds that assignment's slot.
	a := newAssignments()
	a.observe(github.Stats{Assigned: 3, Running: 1})
	for live, want := range []int{3, 2, 1, 0, 0} {
		if got := a.committed(live); got != want {
			t.Errorf("committed(%d) = %d, want %d", live, got, want)
		}
	}
}

func TestUnbackedFollowsTheStatistics(t *testing.T) {
	a := newAssignments()
	a.observe(github.Stats{Assigned: 4})
	a.unbacked = 3
	if got := a.committed(0); got != 1 {
		t.Errorf("committed = %d, want 1 funded assignment", got)
	}
	a.observe(github.Stats{Assigned: 4, Running: 3})
	if a.unbacked != 1 {
		t.Errorf("unbacked = %d, want 1: only one assignment is still not running", a.unbacked)
	}
}

func TestALiveRunnerHoldsAKeptSlot(t *testing.T) {
	// A runner was starting for lint when GitHub cancelled it for
	// reassignment: the runner's slot is the job's, so none is added.
	a := newAssignments()
	a.apply(poll(&github.Stats{Assigned: 1}, assigned(1, "lint")))
	a.apply(poll(&github.Stats{}, cancelled(1, "lint")))
	if got := a.committed(1); got != 0 {
		t.Errorf("committed(1) = %d, want 0: the live runner holds the kept slot", got)
	}
	if got := a.committed(0); got != 1 {
		t.Errorf("committed(0) = %d, want the kept slot", got)
	}
}

func TestFitKeptDropsASlotGitHubRefilled(t *testing.T) {
	// One slot was reserved; the message cancels lint and assigns test,
	// so lint's slot went to test and lint waits at once.
	a := newAssignments()
	a.apply(poll(&github.Stats{Assigned: 1}, assigned(1, "lint")))
	a.apply(poll(&github.Stats{Assigned: 1}, assigned(2, "test"), cancelled(1, "lint")))
	a.fitKept(1)
	if len(a.kept) != 0 || a.owed() != 1 {
		t.Errorf("kept = %v, owed = %d; want lint dropped and one slot owed", a.kept, a.owed())
	}
	t.Run("unbackedAssignmentsHoldNoSlot", func(t *testing.T) {
		// A boot funded one of three assignments; the funded one, lint, is
		// cancelled for reassignment into its own reserved slot.
		c := newAssignments()
		c.apply(poll(&github.Stats{Assigned: 3}, assigned(1, "lint")))
		c.unbacked = 2
		c.apply(poll(&github.Stats{Assigned: 2}, cancelled(1, "lint")))
		c.fitKept(1)
		if len(c.kept) != 1 {
			t.Errorf("kept = %v, want lint kept: the unbacked assignments hold no slot", c.kept)
		}
	})
	b := newAssignments()
	b.apply(poll(&github.Stats{Assigned: 1}, assigned(1, "lint")))
	b.apply(poll(&github.Stats{}, cancelled(1, "lint")))
	b.fitKept(1)
	if len(b.kept) != 1 {
		t.Errorf("kept = %v, want lint kept in the slot no one refilled", b.kept)
	}
}
