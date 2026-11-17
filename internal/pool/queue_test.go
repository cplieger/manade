package pool

import (
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// Expected figures follow the allocation: a scale set reports its reserved
// slots, one slot per ranked job the budget funds in queue order, and a
// standing slot from what is left, organization scale sets first. The
// layout's one host holds four default slots.

type layout struct {
	l                          *ledger
	app, large, lib, org, beta *book
	at                         time.Time
}

func newLayout() *layout {
	cfg := testConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}},
		[]profileSpec{{name: "default", hosts: []string{"default"}, cpus: 2, mem: 4}, {name: "large", hosts: []string{"default"}, cpus: 4, mem: 8}})
	l := openLedger(cfg)
	return &layout{
		l: l, at: epoch,
		app: served(l, "acme/app", "default", false), large: served(l, "acme/app", "large", false),
		lib: served(l, "acme/lib", "default", false), org: served(l, "acme", "default", true),
	}
}

// jobs answers n jobs of profile from run, created a minute apart from
// minute.
func jobs(profile string, run int64, n, minute int) []github.QueuedJob {
	var out []github.QueuedJob
	for i := range n {
		out = append(out, github.QueuedJob{RunID: run + int64(i), Name: "job", Profile: profile, Created: epoch.Add(time.Duration(minute+i) * time.Minute)})
	}
	return out
}

func (ly *layout) capacity(b *book) int {
	got, _ := ly.l.capacity(b, ly.at, true)
	return got
}

// polled hands b a poll begun at start and answered now.
func (ly *layout) polled(b *book, start time.Time, st *github.Stats, events ...github.Event) {
	ly.l.polled(b, &github.Poll{Started: start, Ended: ly.at, Stats: st, Events: events, Idle: len(events) == 0})
}

func (ly *layout) advance(d time.Duration) { ly.at = ly.at.Add(d) }

func TestCapacityFundsTheQueue(t *testing.T) {
	t.Parallel()
	t.Run("waitingJobsThenAStandingSlot", func(t *testing.T) {
		t.Parallel()
		ly := newLayout()
		ly.l.reread(ly.app, jobs("default", 1, 2, 0), ly.at)
		if got := ly.capacity(ly.app); got != 3 {
			t.Errorf("capacity = %d, want 3: two funded jobs and the standing slot the organization's leaves", got)
		}
	})
	t.Run("anOlderJobElsewhereIsEarmarked", func(t *testing.T) {
		t.Parallel()
		ly := newLayout()
		ly.l.reread(ly.lib, jobs("default", 1, 1, 0), ly.at)
		ly.l.reread(ly.app, jobs("default", 2, 3, 1), ly.at)
		if got := ly.capacity(ly.app); got != 3 {
			t.Errorf("app capacity = %d, want 3: lib's older job keeps the fourth slot", got)
		}
		if got := ly.capacity(ly.lib); got != 1 {
			t.Errorf("lib capacity = %d, want 1", got)
		}
	})
	t.Run("headroomInFlightIsNotOfferedTwice", func(t *testing.T) {
		t.Parallel()
		ly := newLayout()
		ly.l.reread(ly.app, jobs("default", 1, 1, 0), ly.at)
		total := 0
		for _, b := range []*book{ly.app, ly.lib, ly.org, ly.large} {
			total += ly.capacity(b)
		}
		if total != 4 {
			t.Errorf("figures in flight sum to %d slots, want the budget's 4", total)
		}
	})
	t.Run("aCutNeverRevokesAnAssignment", func(t *testing.T) {
		t.Parallel()
		ly := newLayout()
		ly.l.reread(ly.app, jobs("default", 1, 4, 1), ly.at)
		start := ly.at
		if got := ly.capacity(ly.app); got != 4 {
			t.Fatalf("capacity = %d, want the budget's 4", got)
		}
		ly.polled(ly.app, start, &github.Stats{Assigned: 4})
		ly.l.reread(ly.lib, jobs("default", 9, 1, 0), ly.at)
		if got := ly.capacity(ly.lib); got != 0 {
			t.Errorf("lib capacity = %d, want 0: every slot backs an assignment", got)
		}
		if got := ly.capacity(ly.app); got != 4 {
			t.Errorf("app capacity = %d, want its 4 assignments kept", got)
		}
		for i := range 4 {
			if _, ok := ly.l.take(ly.app, "r"+string(rune('0'+i))); !ok {
				t.Fatalf("take %d found no slot", i)
			}
		}
		if got := ly.capacity(ly.lib); got != 0 {
			t.Errorf("lib capacity = %d, want 0: the runners took over the slots", got)
		}
	})
	t.Run("aFundedJobIsNotOfferedAStandingSlotToo", func(t *testing.T) {
		t.Parallel()
		ly := newLayout()
		ly.l.reread(ly.org, nil, ly.at)
		ly.l.reread(ly.app, jobs("default", 1, 4, 0), ly.at)
		if got := ly.capacity(ly.org); got != 0 {
			t.Errorf("org capacity = %d, want 0: app's ranked jobs take the budget before standing slots", got)
		}
	})
}

func TestHeadReserve(t *testing.T) {
	t.Parallel()
	// lib holds three slots, so 2 CPUs are left: the large head (4 CPUs)
	// does not fit, and a default job after it does.
	setup := func() *layout {
		ly := newLayout()
		ly.l.opened(ly.lib, github.Stats{Assigned: 3}, ly.at)
		ly.l.reread(ly.large, jobs("large", 1, 1, 0), ly.at)
		ly.l.reread(ly.app, jobs("default", 2, 1, 1), ly.at)
		return ly
	}
	t.Run("backfillUntilTheReserve", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		ly.advance(time.Minute + 59*time.Second)
		if got := ly.capacity(ly.app); got != 1 {
			t.Errorf("capacity = %d, want the default job backfilled", got)
		}
	})
	t.Run("afterTheReserveNoGrantOnItsHost", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		ly.advance(2 * time.Minute)
		if got := ly.capacity(ly.app); got != 0 {
			t.Errorf("app default capacity = %d, want 0 while the head waits", got)
		}
		if got := ly.capacity(ly.org); got != 0 {
			t.Errorf("org capacity = %d, want no standing slot under the reserve", got)
		}
		if got := ly.capacity(ly.lib); got != 3 {
			t.Errorf("lib capacity = %d, want its 3 assignments: none is a running job whose end would free a slot", got)
		}
		if n := sum(ly.lib.pend); n != 3 {
			t.Errorf("lib reserves %d slots, want its 3 assignments': a figure below them revokes none", n)
		}
		a := ly.l.allocate(ly.at)
		if !a.claimant[1] || !a.headSince.Equal(epoch) {
			t.Errorf("head since %v, claimants %v; want the large job's book holding, since %v", a.headSince, a.claimant, epoch)
		}
	})
	t.Run("aHeadNoHostCanServeHoldsNothing", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		ly.l.hosts[0].ready[1] = false
		ly.advance(10 * time.Minute)
		if got := ly.capacity(ly.app); got != 1 {
			t.Errorf("capacity = %d, want the default job funded: a job no host serves is never the head", got)
		}
		if a := ly.l.allocate(ly.at); reserving(&a) || !a.headSince.Equal(epoch.Add(time.Minute)) {
			t.Errorf("head since %v, claimants %v; want app's default job, holding nothing", a.headSince, a.claimant)
		}
	})
	t.Run("aCappedProfileIsNeverTheHead", func(t *testing.T) {
		t.Parallel()
		ly := newLayout()
		ly.l.profiles[1].max = 1
		ly.l.opened(ly.large, github.Stats{Assigned: 1}, ly.at)
		ly.l.reread(ly.large, jobs("large", 1, 1, 0), ly.at)
		ly.l.reread(ly.app, jobs("default", 2, 1, 1), ly.at)
		ly.advance(10 * time.Minute)
		a := ly.l.allocate(ly.at)
		if !a.headSince.Equal(epoch.Add(time.Minute)) || len(a.grants[0]) != 1 || reserving(&a) {
			t.Errorf("head since %v, app grants %v, claimants %v; want app's default job the head and funded past the capped large one", a.headSince, a.grants[0], a.claimant)
		}
	})
}

func TestHeadReserveHoldsOnlyItsHosts(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 4, mem: 16}, {name: "b", cpus: 4, mem: 16}},
		[]profileSpec{
			{name: "large", hosts: []string{"a"}, cpus: 4, mem: 8},
			{name: "small", hosts: []string{"a", "b"}, cpus: 1, mem: 1},
			{name: "bonly", hosts: []string{"b"}, cpus: 1, mem: 1},
		})
	l := openLedger(cfg)
	filler := served(l, "acme/fill", "small", false)
	large := served(l, "acme/app", "large", false)
	small := served(l, "acme/lib", "small", false)
	bonly := served(l, "acme/b", "bonly", false)
	l.opened(filler, github.Stats{Assigned: 1}, epoch)
	l.reread(large, jobs("large", 1, 1, 0), epoch)
	l.reread(small, jobs("small", 2, 2, 1), epoch)
	l.reread(bonly, jobs("bonly", 4, 1, 2), epoch)
	at := epoch.Add(5 * time.Minute)
	a := l.allocate(at)
	if !a.claimant[1] || !a.held[0] || a.held[1] {
		t.Fatalf("held = %v, claimants %v; want host a alone held for the large head", a.held, a.claimant)
	}
	if got := a.grants[2]; len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Errorf("small's grants by host = %v, want both on b, past the held a", got)
	}
	if got := a.grants[3]; len(got) != 1 || got[0] != 1 {
		t.Errorf("bonly's grants by host = %v, want its job on b", got)
	}
}

func TestUnplacedAssignmentsComeFirst(t *testing.T) {
	t.Parallel()
	ly := newLayout()
	ly.l.reread(ly.app, jobs("default", 1, 3, 0), ly.at)
	ly.l.opened(ly.lib, github.Stats{Assigned: 6}, ly.at)
	if n := sum(ly.lib.pend); n != 4 || ly.lib.asg.unbacked != 2 {
		t.Fatalf("lib reserves %d, unbacked %d; want the budget's 4 and 2 unplaced", n, ly.lib.asg.unbacked)
	}
	a := ly.l.allocate(ly.at)
	if !a.claimant[2] || !a.held[0] {
		t.Errorf("claimants %v, held %v; want lib's unplaced assignments holding the host at once", a.claimant, a.held)
	}
	if got := ly.capacity(ly.app); got != 0 {
		t.Errorf("app capacity = %d, want 0: its older waiting jobs come after lib's assignments", got)
	}
}

func TestStandingSlotsRotateAmongOrganizations(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "default", cpus: 2, mem: 8}}, []profileSpec{{name: "default", hosts: []string{"default"}, cpus: 2, mem: 4}})
	l := openLedger(cfg)
	ly := &layout{l: l, at: epoch, app: served(l, "acme/app", "default", false), org: served(l, "acme", "default", true), beta: served(l, "beta", "default", true)}
	idle := func(b *book, d time.Duration) int {
		start := ly.at
		got := ly.capacity(b)
		ly.advance(d)
		ly.polled(b, start, nil)
		return got
	}
	if got := idle(ly.org, 50*time.Second); got != 1 {
		t.Fatalf("acme capacity = %d, want the one standing slot", got)
	}
	if got := ly.capacity(ly.beta); got != 0 {
		t.Errorf("beta capacity = %d, want 0 while acme holds the slot", got)
	}
	if got := idle(ly.org, 50*time.Second); got != 1 {
		t.Errorf("acme capacity at 50s = %d, want its slot kept", got)
	}
	ly.advance(20 * time.Second)
	if got := ly.capacity(ly.org); got != 0 {
		t.Errorf("acme capacity at 2m = %d, want its unfilled slot withdrawn", got)
	}
	if got := ly.capacity(ly.beta); got != 1 {
		t.Errorf("beta capacity = %d, want the slot rotated to it", got)
	}
	t.Run("aFilledSlotEnds", func(t *testing.T) {
		start := ly.at
		ly.advance(5 * time.Second)
		ly.polled(ly.beta, start, &github.Stats{Assigned: 1}, assigned(9, "deploy"))
		if !ly.beta.standSince.IsZero() || !ly.beta.standEnded.Equal(ly.at) {
			t.Errorf("beta standing since %v, ended %v; want ended at the assignment", ly.beta.standSince, ly.beta.standEnded)
		}
	})
}

func TestUnassignableJobs(t *testing.T) {
	t.Parallel()
	lint := []github.QueuedJob{{RunID: 1, Name: "lint", Profile: "default", Created: epoch}}
	setup := func() *layout {
		ly := newLayout()
		ly.l.reread(ly.app, lint, ly.at)
		return ly
	}
	idlePoll := func(ly *layout, d time.Duration) {
		start := ly.at
		ly.capacity(ly.app)
		ly.advance(d)
		ly.polled(ly.app, start, nil)
	}
	t.Run("twoCountedPollsMarkTheJob", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		idlePoll(ly, 50*time.Second)
		idlePoll(ly, 50*time.Second)
		marked := ly.l.reread(ly.app, lint, ly.at)
		if len(marked) != 1 || !marked[0].until.Equal(ly.at.Add(skipFirst)) {
			t.Fatalf("marked = %+v, want lint until %v", marked, ly.at.Add(skipFirst))
		}
		if n := len(ly.l.open(ly.app, ly.at)); n != 0 {
			t.Errorf("open = %d, want the job out of the queue", n)
		}
		if a := ly.l.allocate(ly.at); a.headFound {
			t.Errorf("head since %v, want none: an unassignable job is never the head", a.headSince)
		}
		start := ly.at
		if got := ly.capacity(ly.app); got != 1 {
			t.Errorf("capacity = %d, want only the standing slot: the job's funding is freed", got)
		}
		ly.polled(ly.app, start, nil)
		ly.advance(skipFirst)
		if n := len(ly.l.open(ly.app, ly.at)); n != 1 {
			t.Errorf("open after 10 minutes = %d, want the job queued again", n)
		}
	})
	t.Run("theHoldDoubles", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		for range 2 {
			idlePoll(ly, 50*time.Second)
			idlePoll(ly, 50*time.Second)
			ly.l.reread(ly.app, lint, ly.at)
			ly.advance(skipFirst)
		}
		if n := len(ly.l.open(ly.app, ly.at)); n != 0 {
			t.Errorf("open 10 minutes into the second verdict = %d, want it still held for 20", n)
		}
	})
	t.Run("aJobNoLongerListedIsNotMarked", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		idlePoll(ly, 50*time.Second)
		idlePoll(ly, 50*time.Second)
		if marked := ly.l.reread(ly.app, nil, ly.at); len(marked) != 0 {
			t.Errorf("marked = %+v, want nothing", marked)
		}
	})
	t.Run("aSweepBeforeTheFirstCountedPollProvesNothing", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		idlePoll(ly, 50*time.Second)
		ended := ly.at
		idlePoll(ly, 50*time.Second)
		if marked := ly.l.reread(ly.app, lint, ended.Add(-50*time.Second)); len(marked) != 0 {
			t.Errorf("marked = %+v by a sweep that ended before the first counted poll", marked)
		}
	})
	t.Run("aShortPollNeitherCountsNorResets", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		idlePoll(ly, 50*time.Second)
		start := ly.at
		ly.capacity(ly.app)
		ly.advance(5 * time.Second)
		ly.polled(ly.app, start, &github.Stats{}, github.Event{Kind: github.Started, RunID: 5, Name: "other"})
		if n := ly.app.misses.n; n != 1 {
			t.Errorf("misses = %d, want the first counted poll still counted", n)
		}
	})
	t.Run("aFilledPollResets", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		idlePoll(ly, 50*time.Second)
		start := ly.at
		ly.capacity(ly.app)
		ly.advance(35 * time.Second)
		ly.polled(ly.app, start, &github.Stats{Assigned: 1}, assigned(1, "lint"))
		if marked := ly.l.reread(ly.app, append(lint, github.QueuedJob{RunID: 2, Name: "test", Profile: "default", Created: epoch}), ly.at); len(marked) != 0 {
			t.Errorf("marked = %+v, want nothing: the filled poll reset the count", marked)
		}
	})
	t.Run("aTakenBackFigureFundsNothing", func(t *testing.T) {
		t.Parallel()
		ly := setup()
		ly.l.opened(ly.lib, github.Stats{Assigned: 4, Running: 4}, ly.at)
		for i := range 4 {
			ly.lib.adopt("r"+string(rune('0'+i)), 0, ly.l.slot(0, 0), busy)
		}
		ly.l.reread(ly.large, jobs("large", 7, 1, -5), ly.at)
		ly.advance(time.Minute)
		ly.capacity(ly.large)
		start := ly.at
		ly.capacity(ly.lib)
		if ly.lib.flight.funded != 0 {
			t.Errorf("funded = %d under the reserve, want 0: a poll it cannot fill is no miss", ly.lib.flight.funded)
		}
		ly.advance(50 * time.Second)
		ly.polled(ly.lib, start, nil)
		if ly.lib.misses.n != 0 {
			t.Errorf("misses = %d, want 0", ly.lib.misses.n)
		}
	})
}

func TestKeptCancelIsNotWaiting(t *testing.T) {
	t.Parallel()
	ly := newLayout()
	build := []github.QueuedJob{{RunID: 7, Name: "build", Profile: "default", Created: epoch}}
	ly.l.reread(ly.app, build, ly.at)
	start := ly.at
	ly.capacity(ly.app)
	ly.polled(ly.app, start, &github.Stats{Assigned: 1}, assigned(7, "build"))
	ly.polled(ly.app, start, &github.Stats{}, cancelled(7, "build"))
	if n := len(ly.l.open(ly.app, ly.at)); n != 0 {
		t.Errorf("open in the kept window = %d, want the job still assigned", n)
	}
	if n := ly.app.asg.owed(); n != 1 || sum(ly.app.pend) < n {
		t.Errorf("owed = %d with %d reserved, want the kept slot reserved", n, sum(ly.app.pend))
	}
	ly.polled(ly.app, start, nil)
	if n := len(ly.l.open(ly.app, ly.at)); n != 1 {
		t.Errorf("open after the next poll = %d, want the job waiting again", n)
	}
}

func TestBootReservesWhatTheBudgetFunds(t *testing.T) {
	t.Parallel()
	ly := newLayout()
	ly.l.opened(ly.lib, github.Stats{Assigned: 6}, ly.at)
	started := 0
	for i := range 6 {
		if _, ok := ly.l.take(ly.lib, "r"+string(rune('0'+i))); ok {
			started++
		}
	}
	if started != 4 {
		t.Errorf("starts = %d, want the budget's 4", started)
	}
	if got := ly.capacity(ly.lib); got != 4 {
		t.Errorf("capacity = %d, want only what holds a slot", got)
	}
	// GitHub cancels an unfunded assignment, which this process never named.
	ly.polled(ly.lib, ly.at, &github.Stats{Assigned: 5}, cancelled(3, "deploy"))
	if a := ly.lib.asg; a.unbacked != 1 || len(a.kept) != 0 {
		t.Errorf("unbacked = %d, kept = %d; want 1 and none: the cancel puts no slot back", a.unbacked, len(a.kept))
	}
}
