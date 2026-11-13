package pool

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// Expected figures follow the allocation: a scale set reports its committed
// slots, one slot per ranked job the budget funds in queue order, and a
// standing slot from what is left, organization scale sets first. The
// layout's budget holds four default slots.

func TestCapacityFundsTheQueue(t *testing.T) {
	t.Run("waitingJobsThenAStandingSlot", func(t *testing.T) {
		fx := newFixture(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 2, 0)})
		// Two jobs, then the org's standing slot, then app's own.
		if got := fx.capacity(appDefault); got != 3 {
			t.Errorf("capacity = %d, want 3: two funded jobs and a standing slot", got)
		}
	})
	t.Run("anOlderJobElsewhereIsEarmarked", func(t *testing.T) {
		fx := newFixture(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/lib": jobs("default", 1, 1, 0), "acme/app": jobs("default", 2, 3, 1)})
		if got := fx.capacity(appDefault); got != 3 {
			t.Errorf("app capacity = %d, want 3: lib's older job keeps the fourth slot", got)
		}
		if got := fx.capacity(libDefault); got != 1 {
			t.Errorf("lib capacity = %d, want 1", got)
		}
	})
	t.Run("headroomInFlightIsNotOfferedTwice", func(t *testing.T) {
		fx := newFixture(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 1, 0)})
		total := 0
		for _, ss := range []github.ScaleSet{appDefault, libDefault, orgDefault, appLarge} {
			total += fx.capacity(ss)
		}
		if total != 4 {
			t.Errorf("figures in flight sum to %d slots, want the budget's 4", total)
		}
	})
	t.Run("aStaleProofReportsCommittedOnly", func(t *testing.T) {
		fx := newFixture(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 1, 0)})
		fx.advance(proofAge)
		if got := fx.capacity(appDefault); got != 0 {
			t.Errorf("capacity = %d, want 0 until the scope's visibility is read again", got)
		}
		select {
		case <-fx.p.verify:
		default:
			t.Error("no visibility read was asked for")
		}
		if !fx.p.scopes["acme/app"].wanted {
			t.Error("acme/app is not marked as wanting a proof")
		}
	})
	// A standing slot lets a job start within seconds and needs a proof
	// under a minute old, so the scope's refresh keeps one across idle
	// polls. A proof lands at any age after boot, the 10-minute pass or a
	// sibling's read.
	for _, age := range []time.Duration{0, 15 * time.Second, 25 * time.Second, 45 * time.Second} {
		t.Run("aFundedProofStaysFreshAcrossIdlePollsFrom"+age.String(), func(t *testing.T) {
			fx := newFixture(t)
			fx.skipRecheck()
			fx.advance(age)
			figures, reads := fx.pollIdle(t, 300*time.Second, "acme/app", idlePoll{ss: appDefault})
			if got := figures[appDefault]; !slices.Equal(got, []int{1, 1, 1, 1, 1, 1}) {
				t.Errorf("figures on six idle 50-second polls = %v, want the standing slot on each", got)
			}
			if len(reads) > 6 {
				t.Errorf("acme/app visibility reads = %d over six polls, want at most one a poll", len(reads))
			}
		})
	}
	t.Run("anUnfundedScopeAsksForNoProof", func(t *testing.T) {
		fx := newFixture(t)
		fx.advance(proofAge - time.Second)
		// The organization's, app's and large's standing slots take the budget.
		if got := fx.capacity(libDefault); got != 0 {
			t.Fatalf("lib capacity = %d, want 0", got)
		}
		if fx.p.scopes["acme/lib"].wanted {
			t.Error("a scope with nothing funded asked for a visibility read")
		}
		fx.skipRecheck()
		fx.p.proofs(t.Context())
		if got := fx.reads(); slices.Contains(got, "acme/lib") {
			t.Errorf("reads = %v, want no read of a scope with nothing funded", got)
		}
	})
	t.Run("zeroAtShutdown", func(t *testing.T) {
		fx := newFixture(t)
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 2})
		fx.p.stopping = true
		if got := fx.capacity(appDefault); got != 0 {
			t.Errorf("capacity = %d, want 0", got)
		}
	})
	t.Run("zeroForAPublicScopeWhichStartsNothing", func(t *testing.T) {
		fx := newFixture(t)
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 2})
		fx.p.mu.Lock()
		fx.p.prove(fx.p.scopes["acme/app"], false, nil, fx.at)
		fx.p.mu.Unlock()
		if got := fx.capacity(appDefault); got != 0 {
			t.Errorf("capacity = %d, want 0", got)
		}
		if n := fx.startAll(t); n != 0 {
			t.Errorf("starts = %d, want none for a public scope", n)
		}
	})
	t.Run("aCutNeverRevokesAnAssignment", func(t *testing.T) {
		fx := newFixture(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 4, 1)})
		start := fx.at
		if got := fx.capacity(appDefault); got != 4 {
			t.Fatalf("capacity = %d, want the budget's 4", got)
		}
		fx.polled(appDefault, start, &github.Stats{Assigned: 4})
		fx.sweep(map[string][]github.QueuedJob{"acme/lib": jobs("default", 2, 1, 0)})
		if got := fx.capacity(libDefault); got != 0 {
			t.Errorf("lib capacity = %d, want 0: every slot backs an assignment", got)
		}
		if got := fx.capacity(appDefault); got != 4 {
			t.Errorf("app capacity = %d, want its 4 assignments kept", got)
		}
		fx.startAll(t)
		if got := fx.capacity(libDefault); got != 0 {
			t.Errorf("lib capacity = %d, want 0: the runners took over the slots", got)
		}
	})
}

func TestHeadReserve(t *testing.T) {
	// lib holds three slots, so 2 CPUs are left: the large head (4 CPUs)
	// does not fit, and a default job after it does.
	setup := func(t *testing.T) *fixture {
		fx := newFixture(t)
		fx.polled(libDefault, fx.at, &github.Stats{Assigned: 3})
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("large", 1, "big", 0), job("default", 2, "small", 1)}})
		return fx
	}
	t.Run("backfillUntilTheReserve", func(t *testing.T) {
		fx := setup(t)
		fx.advance(time.Minute + 59*time.Second)
		fx.proveAll()
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d, want the small job backfilled", got)
		}
	})
	t.Run("afterTheReserveOnlyCommitted", func(t *testing.T) {
		fx := setup(t)
		fx.advance(2 * time.Minute)
		fx.proveAll()
		if got := fx.capacity(appDefault); got != 0 {
			t.Errorf("app default capacity = %d, want 0 while the head waits", got)
		}
		if got := fx.capacity(orgDefault); got != 0 {
			t.Errorf("org capacity = %d, want no standing slot under the reserve", got)
		}
		if got := fx.capacity(libDefault); got != 3 {
			t.Errorf("lib capacity = %d, want its 3 committed slots", got)
		}
		if got := fx.p.Snapshot().HeadSince; !got.Equal(epoch) {
			t.Errorf("head since %v, want the large job's %v", got, epoch)
		}
	})
	t.Run("anUnfundableHeadHoldsNothing", func(t *testing.T) {
		fx := setup(t)
		fx.p.setImage(fx.p.byName["large"], "")
		fx.advance(10 * time.Minute)
		fx.proveAll()
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d, want the small job funded: an unfundable job is never the head", got)
		}
		snap := fx.p.Snapshot()
		if c := counts(snap, appLarge); c.ImageMissing != 1 || c.Queued != 0 {
			t.Errorf("large counts = %+v, want 1 image_missing and none queued", c)
		}
		if want := epoch.Add(time.Minute); !snap.HeadSince.Equal(want) {
			t.Errorf("head since %v, want the small job's %v", snap.HeadSince, want)
		}
	})
	t.Run("aCappedProfileIsNeverTheHead", func(t *testing.T) {
		capped := strings.Replace(layout, "large: {cpus: 4,", "large: {max_runners: 1, cpus: 4,", 1)
		fx := newFixtureFrom(t, load(t, capped), appDefault, appLarge, libDefault, orgDefault)
		fx.polled(appLarge, fx.at, &github.Stats{Assigned: 1})
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("large", 1, "big", 0), job("default", 2, "small", 1)}})
		fx.advance(10 * time.Minute)
		fx.proveAll()
		// large holds 4 CPUs: the small job takes 2, the org's standing slot
		// the rest.
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d, want the small job funded past a capped head", got)
		}
		if want := epoch.Add(time.Minute); !fx.p.Snapshot().HeadSince.Equal(want) {
			t.Errorf("head since %v, want the small job's", fx.p.Snapshot().HeadSince)
		}
	})
}

func TestNotAllowedJobsAreCountedApart(t *testing.T) {
	fx := newFixture(t)
	fx.sweep(map[string][]github.QueuedJob{"acme/lib": {job("large", 1, "big", 0)}})
	snap := fx.p.Snapshot()
	if c := counts(snap, scaleSet("acme/lib", "large", 0)); c.NotAllowed != 1 || c.NotServing != 0 {
		t.Errorf("lib large counts = %+v, want 1 not_allowed", c)
	}
	if !snap.HeadSince.IsZero() {
		t.Errorf("head since %v, want no head", snap.HeadSince)
	}
}

func TestStandingSlotsRotateAmongOrganizations(t *testing.T) {
	cfg := load(t, `capacity: {cpus: 2, memory: 8GiB}
scopes:
  - repo: acme/app
  - org: acme
  - org: beta
profiles:
  default: {cpus: 2, memory: 4GiB}
`)
	acme, beta := orgDefault, scaleSet("beta", "default", 8)
	fx := newFixtureFrom(t, cfg, appDefault, acme, beta)
	idle := func(ss github.ScaleSet, d time.Duration) int {
		start := fx.at
		got := fx.capacity(ss)
		fx.advance(d)
		fx.polled(ss, start, nil)
		return got
	}
	if got := idle(acme, 50*time.Second); got != 1 {
		t.Fatalf("acme capacity = %d, want the one standing slot", got)
	}
	if got := fx.capacity(beta); got != 0 {
		t.Errorf("beta capacity = %d, want 0 while acme holds the slot", got)
	}
	fx.proveAll()
	if got := idle(acme, 50*time.Second); got != 1 {
		t.Errorf("acme capacity at 50s = %d, want its slot kept", got)
	}
	fx.advance(20 * time.Second)
	fx.proveAll()
	if got := fx.capacity(acme); got != 0 {
		t.Errorf("acme capacity at 2m = %d, want its unfilled slot withdrawn", got)
	}
	if got := fx.capacity(beta); got != 1 {
		t.Errorf("beta capacity = %d, want the slot rotated to it", got)
	}
	t.Run("aFilledSlotEnds", func(t *testing.T) {
		start := fx.at
		fx.advance(5 * time.Second)
		fx.polled(beta, start, &github.Stats{Assigned: 1}, assigned(9, "deploy"))
		fx.p.mu.Lock()
		s := fx.p.set(beta)
		since, ended := s.standSince, s.standEnded
		fx.p.mu.Unlock()
		if !since.IsZero() || !ended.Equal(fx.at) {
			t.Errorf("beta standing since %v, ended %v; want ended at the assignment", since, ended)
		}
	})
}

func TestUnassignableJobs(t *testing.T) {
	setup := func(t *testing.T) *fixture {
		fx := newFixture(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0)}})
		return fx
	}
	// idlePoll runs one poll of ss that returns idle after d.
	idlePoll := func(fx *fixture, d time.Duration) {
		fx.proveAll()
		start := fx.at
		fx.capacity(appDefault)
		fx.advance(d)
		fx.polled(appDefault, start, nil)
	}
	t.Run("twoCountedPollsMarkTheJob", func(t *testing.T) {
		fx := setup(t)
		idlePoll(fx, 50*time.Second)
		idlePoll(fx, 50*time.Second)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0)}})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 1 || c.Queued != 0 {
			t.Errorf("counts = %+v, want the job unassignable and out of the queue", c)
		}
		if n := fx.rec.CountLevel(slog.LevelWarn, "GitHub did not assign a queued job"); n != 1 {
			t.Errorf("WARN lines = %d, want 1", n)
		}
		if got := fx.p.Snapshot().HeadSince; !got.IsZero() {
			t.Errorf("head since %v, want none: an unassignable job is never the head", got)
		}
		fx.proveAll()
		start := fx.at
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d, want only the standing slot: the job's funding is freed", got)
		}
		fx.polled(appDefault, start, nil)
		fx.advance(skipFirst)
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 0 || c.Queued != 1 {
			t.Errorf("counts after 10 minutes = %+v, want the job queued again", c)
		}
	})
	t.Run("theHoldDoubles", func(t *testing.T) {
		fx := setup(t)
		for range 2 {
			idlePoll(fx, 50*time.Second)
			idlePoll(fx, 50*time.Second)
			fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0)}})
			fx.advance(skipFirst)
		}
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 1 {
			t.Errorf("counts 10 minutes into the second verdict = %+v, want it still held for 20", c)
		}
	})
	t.Run("aJobNoLongerListedIsNotMarked", func(t *testing.T) {
		fx := setup(t)
		idlePoll(fx, 50*time.Second)
		idlePoll(fx, 50*time.Second)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": nil})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 0 {
			t.Errorf("counts = %+v, want nothing marked", c)
		}
	})
	t.Run("aSweepBeforeTheFirstCountedPollProvesNothing", func(t *testing.T) {
		fx := setup(t)
		idlePoll(fx, 50*time.Second)
		ended := fx.at
		idlePoll(fx, 50*time.Second)
		fx.p.queued(ended.Add(-50*time.Second), []github.Queue{{Scope: "acme/app", Jobs: []github.QueuedJob{job("default", 1, "lint", 0)}, Complete: true}})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 0 {
			t.Errorf("counts = %+v, want nothing marked by a sweep that ended before the first counted poll", c)
		}
	})
	t.Run("aShortPollNeitherCountsNorResets", func(t *testing.T) {
		fx := setup(t)
		idlePoll(fx, 50*time.Second)
		fx.proveAll()
		start := fx.at
		fx.capacity(appDefault)
		fx.advance(5 * time.Second)
		fx.polled(appDefault, start, &github.Stats{}, github.Event{Kind: github.Started, RunID: 5, Name: "other"})
		fx.p.mu.Lock()
		n := fx.p.set(appDefault).misses.n
		fx.p.mu.Unlock()
		if n != 1 {
			t.Errorf("misses = %d, want the first counted poll still counted", n)
		}
	})
	t.Run("aFilledPollResets", func(t *testing.T) {
		fx := setup(t)
		idlePoll(fx, 50*time.Second)
		fx.proveAll()
		start := fx.at
		fx.capacity(appDefault)
		fx.advance(35 * time.Second)
		fx.polled(appDefault, start, &github.Stats{Assigned: 1}, assigned(1, "lint"))
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0), job("default", 2, "test", 3)}})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 0 {
			t.Errorf("counts = %+v, want nothing marked: the filled poll reset the count", c)
		}
	})
}

func TestKeptCancelIsNotWaiting(t *testing.T) {
	fx := newFixture(t)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 7, "build", 0)}})
	start := fx.at
	fx.capacity(appDefault)
	fx.polled(appDefault, start, &github.Stats{Assigned: 1}, assigned(7, "build"))
	fx.polled(appDefault, start, &github.Stats{}, cancelled(7, "build"))
	if c := counts(fx.p.Snapshot(), appDefault); c.Queued != 0 {
		t.Errorf("counts in the kept window = %+v, want the job still assigned", c)
	}
	fx.p.mu.Lock()
	v := fx.p.view(fx.p.set(appDefault), fx.at)
	fx.p.mu.Unlock()
	if v.slots != 1 {
		t.Errorf("slots = %d, want the kept slot", v.slots)
	}
	fx.polled(appDefault, start, nil)
	if c := counts(fx.p.Snapshot(), appDefault); c.Queued != 1 {
		t.Errorf("counts after the next poll = %+v, want the job waiting again", c)
	}
}

func TestBootStartsWhatTheBudgetFunds(t *testing.T) {
	fx := newFixture(t)
	fx.p.Opened(libDefault, github.Stats{Assigned: 6})
	if n := fx.startAll(t); n != 4 {
		t.Errorf("starts = %d, want the budget's 4", n)
	}
	if got := fx.capacity(libDefault); got != 4 {
		t.Errorf("capacity = %d, want only what is funded", got)
	}
	// GitHub cancels an unfunded assignment, which this process never named.
	fx.polled(libDefault, fx.at, &github.Stats{Assigned: 5}, cancelled(3, "deploy"))
	fx.p.mu.Lock()
	a := fx.p.set(libDefault).asg
	unbacked, kept := a.unbacked, len(a.kept)
	fx.p.mu.Unlock()
	if unbacked != 1 || kept != 0 {
		t.Errorf("unbacked = %d, kept = %d; want 1 and none: the cancel puts no slot back", unbacked, kept)
	}
}

func TestHeldStopsServing(t *testing.T) {
	fx := newFixture(t)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 2, 0)})
	fx.capacity(appDefault)
	fx.p.Held(appDefault, true)
	snap := fx.p.Snapshot()
	if c := counts(snap, appDefault); c.NotServing != 2 || c.Queued != 0 {
		t.Errorf("counts = %+v, want both jobs not_serving", c)
	}
	if got := fx.capacity(libDefault); got != 1 {
		t.Errorf("lib capacity = %d, want a standing slot: the held figure no longer reserves", got)
	}
}

func TestAPartialReadFundsNothingNew(t *testing.T) {
	fx := newFixture(t)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 1, 0)})
	fx.p.queued(fx.at, []github.Queue{{Scope: "acme/app", Jobs: jobs("default", 1, 3, 0)}})
	if got := fx.capacity(appDefault); got != 0 {
		t.Errorf("capacity = %d, want nothing funded on a partial read", got)
	}
	fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 2, 0)})
	if got := fx.capacity(appDefault); got != 3 {
		t.Errorf("capacity = %d, want the whole read's 2 jobs and a standing slot", got)
	}
}

func TestClosedSessionIsNotServing(t *testing.T) {
	fx := newFixture(t)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 1, 0)})
	fx.p.Closed(appDefault)
	if c := counts(fx.p.Snapshot(), appDefault); c.NotServing != 1 {
		t.Errorf("counts = %+v, want the job not_serving while no session is open", c)
	}
}

func TestBeatRefreshesTheLease(t *testing.T) {
	fx := newFixture(t)
	fx.advance(time.Minute)
	fx.p.Beat(appDefault)
	fx.p.mu.Lock()
	defer fx.p.mu.Unlock()
	if got := fx.p.set(appDefault).beat; !got.Equal(fx.at) {
		t.Errorf("beat = %v, want %v", got, fx.at)
	}
}

func TestJobMessagesReachTheRunners(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.startAll(t)
	var name string
	fx.p.mu.Lock()
	for n := range fx.p.set(appDefault).runners {
		name = n
	}
	fx.p.mu.Unlock()
	queued := epoch.Add(-time.Minute)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1, Running: 1},
		github.Event{Kind: github.Started, Runner: name, RunID: 1, Name: "lint", Queued: queued, RunnerAssigned: epoch})
	if got := fx.runners(appDefault); got[busy] != 1 {
		t.Errorf("runners = %v, want one busy", got)
	}
	fx.polled(appDefault, fx.at, &github.Stats{},
		github.Event{Kind: github.Completed, Runner: name, RunID: 1, Name: "lint", Result: "failed", RunnerAssigned: epoch, Finished: epoch.Add(time.Minute)})
	fx.p.mu.Lock()
	done := fx.p.set(appDefault).runners[name].completed
	fx.p.mu.Unlock()
	if !done {
		t.Error("the runner is not marked as having completed its job")
	}
	for _, want := range []string{"Job started", "Job finished"} {
		if n := fx.rec.CountLevel(slog.LevelInfo, want); n != 1 {
			t.Errorf("%q lines = %d, want 1", want, n)
		}
	}
}

func TestJobResult(t *testing.T) {
	for in, want := range map[string]string{"succeeded": "succeeded", "failed": "failed", "canceled": "canceled", "skipped": "other", "": "other"} {
		if got := jobResult(in); string(got) != want {
			t.Errorf("jobResult(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStandingSlotsStayWhileTheBudgetCoversEveryOrganization(t *testing.T) {
	cfg := load(t, `capacity: {cpus: 4, memory: 8GiB}
scopes:
  - org: acme
  - org: beta
profiles:
  default: {cpus: 2, memory: 4GiB}
`)
	beta := scaleSet("beta", "default", 8)
	fx := newFixtureFrom(t, cfg, orgDefault, beta)
	for range 4 {
		fx.proveAll()
		for _, ss := range []github.ScaleSet{orgDefault, beta} {
			if got := fx.capacity(ss); got != 1 {
				t.Fatalf("%v capacity at %v = %d, want its standing slot kept", ss.ScaleSet, fx.at.Sub(epoch), got)
			}
		}
		fx.advance(50 * time.Second)
	}
}

func TestUnassignableEdges(t *testing.T) {
	setup := func(t *testing.T) *fixture {
		fx := newFixture(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0)}})
		return fx
	}
	poll := func(fx *fixture, d time.Duration, idle bool) {
		fx.proveAll()
		start := fx.at
		fx.capacity(appDefault)
		fx.advance(d)
		if idle {
			fx.polled(appDefault, start, nil)
			return
		}
		fx.polled(appDefault, start, &github.Stats{}, github.Event{Kind: github.Started, RunID: 9, Name: "other"})
	}
	t.Run("aShortIdlePollCounts", func(t *testing.T) {
		fx := setup(t)
		poll(fx, 10*time.Second, true)
		poll(fx, 10*time.Second, true)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0)}})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 1 {
			t.Errorf("counts = %+v, want the job marked: an idle poll counts at any length", c)
		}
	})
	t.Run("aPollThatFundedNothingResets", func(t *testing.T) {
		fx := setup(t)
		poll(fx, 50*time.Second, true)
		fx.p.setImage(fx.p.byName["default"], "")
		poll(fx, 50*time.Second, true)
		fx.p.setImage(fx.p.byName["default"], "sha256:default")
		poll(fx, 50*time.Second, true)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0)}})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 0 {
			t.Errorf("counts = %+v, want nothing marked after one counted poll in a row", c)
		}
	})
	t.Run("aHeldJobIsNotMarkedAgain", func(t *testing.T) {
		fx := setup(t)
		poll(fx, 50*time.Second, true)
		poll(fx, 50*time.Second, true)
		listed := []github.QueuedJob{job("default", 1, "lint", 0), job("default", 2, "test", 5)}
		fx.sweep(map[string][]github.QueuedJob{"acme/app": listed[:1]})
		marked := fx.at
		fx.sweep(map[string][]github.QueuedJob{"acme/app": listed})
		poll(fx, 50*time.Second, true)
		poll(fx, 50*time.Second, true)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": listed})
		fx.p.mu.Lock()
		until := fx.p.set(appDefault).skipped[jobKey{"lint", 1}].until
		fx.p.mu.Unlock()
		if want := marked.Add(skipFirst); !until.Equal(want) {
			t.Errorf("lint held until %v, want its first verdict's %v", until, want)
		}
	})
	t.Run("aJobOffTheListIsForgotten", func(t *testing.T) {
		fx := setup(t)
		poll(fx, 50*time.Second, true)
		poll(fx, 50*time.Second, true)
		lint := []github.QueuedJob{job("default", 1, "lint", 0)}
		fx.sweep(map[string][]github.QueuedJob{"acme/app": lint})
		fx.sweep(map[string][]github.QueuedJob{"acme/app": nil})
		fx.sweep(map[string][]github.QueuedJob{"acme/app": lint})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 0 || c.Queued != 1 {
			t.Errorf("counts = %+v, want the relisted job queued with no hold", c)
		}
	})
	t.Run("aShortBusyPollIsNotCounted", func(t *testing.T) {
		fx := setup(t)
		poll(fx, 50*time.Second, true)
		poll(fx, 10*time.Second, false)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0)}})
		if c := counts(fx.p.Snapshot(), appDefault); c.Unassignable != 0 {
			t.Errorf("counts = %+v, want nothing marked", c)
		}
	})
}

func TestBootBacksAssignmentsAfterOtherHoldings(t *testing.T) {
	t.Run("otherScaleSets", func(t *testing.T) {
		fx := newFixture(t)
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 2})
		fx.p.Opened(libDefault, github.Stats{Assigned: 6})
		fx.p.mu.Lock()
		unbacked := fx.p.set(libDefault).asg.unbacked
		fx.p.mu.Unlock()
		if unbacked != 4 {
			t.Errorf("unbacked = %d, want 4: app's 2 slots leave room for 2", unbacked)
		}
	})
	t.Run("fundedOnceTheBudgetFrees", func(t *testing.T) {
		// Live on 2026-11-13: a restart left one assignment unfunded and
		// GitHub kept it assigned, so the job never got a runner.
		fx := newFixture(t)
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 2})
		if n := fx.startAll(t); n != 2 {
			t.Fatalf("app starts = %d, want 2", n)
		}
		fx.p.Opened(libDefault, github.Stats{Assigned: 6})
		if n := fx.startAll(t); n != 2 {
			t.Fatalf("lib starts at boot = %d, want the 2 the budget covers", n)
		}
		fx.p.mu.Lock()
		var app []string
		for name := range fx.p.set(appDefault).runners {
			app = append(app, name)
		}
		fx.p.mu.Unlock()
		for _, name := range app {
			fx.dk.exit(name, 0, false)
		}
		fx.polled(appDefault, fx.at, &github.Stats{},
			github.Event{Kind: github.Completed, Runner: app[0], RunID: 1, Name: "job-0", Result: "succeeded", Finished: epoch},
			github.Event{Kind: github.Completed, Runner: app[1], RunID: 1, Name: "job-1", Result: "succeeded", Finished: epoch})
		fx.reconcile(t)
		if n := fx.startAll(t); n != 2 {
			t.Errorf("lib starts once app's runners end = %d, want 2 more", n)
		}
		fx.p.mu.Lock()
		unbacked := fx.p.set(libDefault).asg.unbacked
		fx.p.mu.Unlock()
		if unbacked != 2 {
			t.Errorf("unbacked = %d, want 2 still waiting for the budget", unbacked)
		}
	})
	t.Run("theProfileCap", func(t *testing.T) {
		capped := strings.Replace(layout, "default: {cpus: 2,", "default: {max_runners: 1, cpus: 2,", 1)
		fx := newFixtureFrom(t, load(t, capped), appDefault, appLarge, libDefault, orgDefault)
		fx.p.Opened(libDefault, github.Stats{Assigned: 3})
		if n := fx.startAll(t); n != 1 {
			t.Errorf("starts = %d, want the cap's 1", n)
		}
	})
}

func TestJobMessagesFeedTheHistograms(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1, Running: 1},
		github.Event{Kind: github.Started, Runner: "elsewhere", RunID: 1, Name: "lint", Queued: epoch, RunnerAssigned: epoch.Add(time.Minute)})
	fx.polled(appDefault, fx.at, &github.Stats{},
		github.Event{Kind: github.Completed, Runner: "elsewhere", RunID: 1, Name: "lint", Result: "succeeded", RunnerAssigned: epoch, Finished: epoch.Add(time.Hour)},
		cancelled(2, "test"))
	page := scrape(t, fx.m)
	for _, want := range []string{
		`manade_job_queue_seconds_count{profile="default"} 1`,
		`manade_job_duration_seconds_count{profile="default",result="succeeded"} 1`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if strings.Contains(page, `result="canceled"} 1`) {
		t.Error("a reassignment cancel was observed as a job duration")
	}
	if n := fx.rec.CountLevel(slog.LevelInfo, "Job finished"); n != 1 {
		t.Errorf("finish lines = %d, want 1: a cancel no runner took is no outcome", n)
	}
}

func TestAHeldScaleSetLeavesItsSiblingsServed(t *testing.T) {
	fx := newFixture(t)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("large", 1, "big", 0), job("default", 2, "small", 1)}})
	fx.p.Held(appLarge, true)
	if got := fx.capacity(appDefault); got != 2 {
		t.Errorf("app default capacity = %d, want its job and a standing slot", got)
	}
	if !strings.Contains(scrape(t, fx.m), `manade_scope_serving{scope="acme/app"} 0`) {
		t.Error("acme/app reads as serving with a scale set held elsewhere")
	}
}

func TestACancelBesideANewAssignmentKeepsTheFlightsSlot(t *testing.T) {
	fx := newFixture(t)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "lint", 0), job("default", 2, "test", 1)}})
	start := fx.at
	fx.capacity(appDefault)
	fx.polled(appDefault, start, &github.Stats{Assigned: 1}, assigned(1, "lint"))
	start = fx.at
	if got := fx.capacity(appDefault); got != 3 {
		t.Fatalf("capacity = %d, want lint's slot, test's and a standing slot", got)
	}
	// The poll's figure had room for test beside lint's reassignment.
	fx.polled(appDefault, start, &github.Stats{Assigned: 1}, assigned(2, "test"), cancelled(1, "lint"))
	if c := counts(fx.p.Snapshot(), appDefault); c.Queued != 0 {
		t.Errorf("counts = %+v, want lint kept in the slot the figure carried", c)
	}
}

func TestAPartialReadPrunesNothing(t *testing.T) {
	fx := newFixture(t)
	listed := []github.QueuedJob{job("default", 1, "lint", 0), job("default", 2, "test", 5)}
	fx.sweep(map[string][]github.QueuedJob{"acme/app": listed})
	// Another job runs; lint is assigned and test waits.
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 2, Running: 1}, assigned(1, "lint"))
	fx.p.queued(fx.at, []github.Queue{{Scope: "acme/app"}})
	fx.sweep(map[string][]github.QueuedJob{"acme/app": listed})
	if got, want := fx.p.Snapshot().HeadSince, epoch.Add(5*time.Minute); !got.Equal(want) {
		t.Errorf("head since %v, want test's %v: lint is still named as assigned", got, want)
	}
}

func TestAnIdleRunnerCoversAWaitingJob(t *testing.T) {
	// A user cancelled the job a runner was started for, so the runner is
	// idle and GitHub hands it the next job: that job needs no new slot.
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.startAll(t)
	user := cancelled(1, "lint")
	user.Finished = epoch
	fx.polled(appDefault, fx.at, &github.Stats{}, user)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 2, "test", 0)}})
	if got := fx.capacity(appDefault); got != 2 {
		t.Errorf("capacity = %d, want the idle runner and a standing slot", got)
	}
}
