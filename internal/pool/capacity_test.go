package pool

import (
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// longPoll is how long GitHub holds a poll with nothing to send.
const longPoll = 50 * time.Second

// pollLoop runs every scale set's polls as GitHub answers them, a second
// at a time until until: a poll answers at once when its figure lets
// GitHub assign a queued job or a message waits, and otherwise after
// longPoll; a new poll begins as each answers. at runs first each second.
func (w *world) pollLoop(until time.Time, at func()) {
	for ; w.now.Before(until); w.advance(time.Second) {
		at()
		for _, s := range w.sets {
			if s.inFlight && (s.room() > 0 && len(s.queue) > 0 || len(s.pending) > 0 || w.now.Sub(s.start) >= longPoll) {
				w.answer(s, len(s.queue))
				w.starts()
				w.takeJobs()
			}
			if !s.inFlight {
				w.sweep()
				w.begin(s)
			}
		}
	}
}

func TestHeadReserveTakesASlotBack(t *testing.T) {
	t.Parallel()
	// Revision 5's deployment on 2026-11-12: two 4-CPU runners filled an
	// 8-CPU budget, each exit was refilled within a second from one
	// repository's backlog, and another repository's older job waited over
	// 15 minutes with the head reserve active.
	cfg := testConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}}, []profileSpec{{name: "build", hosts: []string{"default"}, cpus: 4, mem: 8}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "build"}, setSpec{scope: "acme/lib", profile: "build"})
	app, lib := w.sets[0], w.sets[1]
	w.queue(app, 2)
	w.cycle(app)
	if len(app.running) != 2 {
		t.Fatalf("app runs %d jobs, want 2", len(app.running))
	}
	w.advance(time.Minute)
	w.queue(lib, 1)
	w.advance(time.Minute)
	w.queue(app, 10)
	exits := []time.Time{w.now.Add(2 * time.Minute), w.now.Add(4 * time.Minute)}
	var firstExit time.Time
	libPolls := 0
	w.pollLoop(w.now.Add(10*time.Minute), func() {
		for len(exits) > 0 && !w.now.Before(exits[0]) {
			if firstExit.IsZero() {
				firstExit = w.now
			}
			exits = exits[1:]
			for _, name := range w.names() {
				if r := w.runs[name]; r.alive && r.set == app {
					w.finish(r)
					break
				}
			}
		}
		if !firstExit.IsZero() && lib.inFlight && len(lib.running) == 0 && w.now.Equal(lib.start) {
			libPolls++
		}
	})
	if len(lib.running) != 1 {
		t.Fatalf("lib runs %d jobs at the end, want its 1", len(lib.running))
	}
	if libPolls > 2 {
		t.Errorf("lib's job started %d of its polls after the first exit, want within 2", libPolls)
	}
	if len(app.running)+len(app.assigned) > 2 {
		t.Errorf("app holds %d jobs, past the 2 slots its own runners freed", len(app.running)+len(app.assigned))
	}
}

func TestTakeBack(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 8, mem: 32}, {name: "b", cpus: 8, mem: 32}},
		[]profileSpec{{name: "big", hosts: []string{"a"}, cpus: 6, mem: 8}, {name: "small", hosts: []string{"a", "b"}, cpus: 2, mem: 2}})
	setup := func(t *testing.T, smallRunners int) (*world, *mset, *mset) {
		t.Helper()
		w := newWorld(t, cfg, setSpec{scope: "acme/big", profile: "big"}, setSpec{scope: "acme/small", profile: "small"})
		big, small := w.sets[0], w.sets[1]
		w.queue(small, smallRunners)
		w.cycle(small)
		w.deliver(small)
		w.advance(time.Minute)
		w.queue(big, 1)
		w.advance(time.Minute)
		w.queue(small, 5)
		w.advance(3 * time.Minute)
		w.cycle(big)
		w.sweep()
		return w, big, small
	}
	t.Run("lessTheSlotsTheHeadNeeds", func(t *testing.T) {
		t.Parallel()
		// Four 2-CPU runners fill host a; the 6-CPU head needs three of
		// their slots back. Host b is not held, so small's backlog is
		// funded there, but only past its runners' figure.
		w, _, small := setup(t, 4)
		w.begin(small)
		if want := 4 - 3 + 4; small.figure != want {
			t.Errorf("small reports %d, want %d: its 4 runners less 3 on the held host, plus 4 funded on b", small.figure, want)
		}
		if got := pendOn(cfg, small.b); got["a"] != 0 || got["b"] != 1 {
			t.Errorf("small's slots by host = %v, want the 1 its figure lets GitHub fill, on b", got)
		}
		w.answer(small, 5)
		w.starts()
		for _, name := range w.names() {
			if r := w.runs[name]; r.alive && r.host == 0 {
				w.finish(r)
				break
			}
		}
		w.begin(small)
		w.answer(small, 5)
		w.starts()
		if n := w.liveOn(0); n != 3 {
			t.Errorf("host a runs %d containers after one exit, want 3: its freed slot goes to no refill", n)
		}
	})
	t.Run("neverBelowZero", func(t *testing.T) {
		t.Parallel()
		// Other's three runners and small's one fill host a; the head
		// needs three of either's slots back, more than small holds.
		one := testConfig([]hostSpec{{name: "a", cpus: 8, mem: 32}},
			[]profileSpec{{name: "big", hosts: []string{"a"}, cpus: 6, mem: 8}, {name: "small", hosts: []string{"a"}, cpus: 2, mem: 2}})
		w := newWorld(t, one, setSpec{scope: "acme/big", profile: "big"}, setSpec{scope: "acme/small", profile: "small"},
			setSpec{scope: "acme/other", profile: "small"})
		big, small, other := w.sets[0], w.sets[1], w.sets[2]
		w.queue(other, 3)
		w.queue(small, 1)
		w.cycle(other)
		w.cycle(small)
		w.deliver(other)
		w.deliver(small)
		w.advance(time.Minute)
		w.queue(big, 1)
		w.advance(3 * time.Minute)
		w.cycle(big)
		for _, s := range []*mset{small, other} {
			w.begin(s)
			if s.figure != 0 {
				t.Errorf("%s reports %d, want 0", s.scope, s.figure)
			}
			if n := sum(s.b.pend); n != 0 {
				t.Errorf("%s reserves %d slots on the held host", s.scope, n)
			}
		}
	})
	t.Run("theHeadsOwnSetKeepsItsSlots", func(t *testing.T) {
		t.Parallel()
		w, big, _ := setup(t, 4)
		w.begin(big)
		if big.figure != 0 {
			t.Errorf("big reports %d while its job does not fit, want 0", big.figure)
		}
	})
}

func TestLostRunnerHoldsACapacitySlot(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 4, mem: 8}}, []profileSpec{{name: "p", hosts: []string{"a"}, cpus: 2, mem: 2}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "p"})
	s := w.sets[0]
	w.queue(s, 2)
	w.cycle(s)
	w.cycle(s)
	w.crash(0)
	w.queue(s, 1)
	w.sweep()
	t.Run("outOfTheStartRule", func(t *testing.T) {
		w.answer(s, 0)
		if n := w.l.demand(s.b); n != 0 {
			t.Errorf("demand = %d with two lost runners and nothing new assigned, want 0", n)
		}
	})
	t.Run("aSlotEachAtNoBudget", func(t *testing.T) {
		w.begin(s)
		if s.figure != 2+2 {
			t.Errorf("figure = %d, want the 2 lost slots plus the 2 the free budget funds", s.figure)
		}
		w.answer(s, 1)
		w.starts()
		w.takeJobs()
		if len(s.running) != 3 {
			t.Errorf("running = %d, want the 2 lost jobs and the new one", len(s.running))
		}
	})
	t.Run("untilGitHubFailsTheJob", func(t *testing.T) {
		w.advance(lostFor)
		w.begin(s)
		w.answer(s, 0)
		if lost := s.b.lost(); lost != 0 {
			t.Errorf("lost = %d once GitHub failed the jobs, want 0", lost)
		}
	})
}

func TestCompletedRunnersSlotWaitsForItsExit(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 2, mem: 4}}, []profileSpec{{name: "p", hosts: []string{"a"}, cpus: 2, mem: 2}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "p"})
	s := w.sets[0]
	w.queue(s, 2)
	w.cycle(s)
	w.cycle(s)
	var r *mrun
	for _, name := range w.names() {
		r = w.runs[name]
	}
	// The job ends and GitHub refills the slot in the same answer, while
	// the runner's container still runs.
	w.begin(s)
	s.running = nil
	s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: r.job.run, Name: "job", Runner: r.name, Result: "succeeded"})
	w.answer(s, 1)
	w.starts()
	if alive := w.liveOn(0); alive != 1 {
		t.Fatalf("live containers = %d before the exit, want the completed one alone", alive)
	}
	if s.b.asg.unbacked != 0 {
		t.Errorf("unbacked = %d: the refill waits on the completed runner's slot, not as unplaced", s.b.asg.unbacked)
	}
	w.exit(r)
	w.starts()
	if alive := w.liveOn(0); alive != 1 {
		t.Errorf("live containers = %d after the exit, want the refill's runner", alive)
	}
}

func (w *world) liveOn(h int) int {
	n := 0
	for _, r := range w.runs {
		if r.alive && r.host == h {
			n++
		}
	}
	return n
}

func TestFigureWithoutAFreshProof(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 8, mem: 16}}, []profileSpec{{name: "p", hosts: []string{"a"}, cpus: 2, mem: 2}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "p"})
	s := w.sets[0]
	w.queue(s, 3)
	w.sweep()
	figure, above := w.l.capacity(s.b, w.now, false)
	if figure != 0 || !above {
		t.Errorf("capacity = %d, above %v; want 0, cut to the assignments, and above", figure, above)
	}
	if n := sum(s.b.pend); n != 0 {
		t.Errorf("reserved %d slots for a figure it did not report", n)
	}
	figure, above = w.l.capacity(s.b, w.now, true)
	if figure != 4 || !above {
		t.Errorf("capacity = %d, above %v; want the 3 jobs and a standing slot, and above", figure, above)
	}
}

func TestNotServedReportsZero(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 8, mem: 16}}, []profileSpec{{name: "p", hosts: []string{"a"}, cpus: 2, mem: 2}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "p"})
	s := w.sets[0]
	w.queue(s, 2)
	w.cycle(s)
	s.b.serve = false
	w.begin(s)
	if s.figure != 0 {
		t.Errorf("figure = %d for a scale set not served, want 0", s.figure)
	}
	w.answer(s, 0)
	if n := sum(s.b.pend); n != 0 {
		t.Errorf("reserved %d slots for a scale set not served", n)
	}
}

func TestRetireNamesSpareRunnersOnAHeldHost(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 4, mem: 16}, {name: "b", cpus: 4, mem: 16}},
		[]profileSpec{{name: "big", hosts: []string{"a"}, cpus: 4, mem: 8}, {name: "small", hosts: []string{"a", "b"}, cpus: 2, mem: 2}})
	setup := func(waiting int) (*ledger, *book, *book) {
		l := openLedger(cfg)
		big, small := served(l, "acme/big", "big", false), served(l, "acme/small", "small", false)
		l.opened(small, github.Stats{Assigned: waiting}, epoch)
		for _, r := range []struct {
			name string
			host int
		}{{"a1", 0}, {"a2", 0}, {"b1", 1}} {
			small.adopt(r.name, r.host, l.slot(1, r.host), idle)
		}
		l.reread(big, jobs("big", 1, 1, 0), epoch)
		return l, big, small
	}
	t.Run("asManyAsTheHeadNeeds", func(t *testing.T) {
		t.Parallel()
		l, big, small := setup(0)
		l.capacity(big, epoch.Add(5*time.Minute), true)
		if got := l.retire()[small]; len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
			t.Errorf("retire = %v, want a1 and a2: the held host's spares, and not b1", got)
		}
	})
	t.Run("notTheRunnersAnAssignmentWaitsFor", func(t *testing.T) {
		t.Parallel()
		l, big, small := setup(2)
		l.capacity(big, epoch.Add(5*time.Minute), true)
		if got := l.retire()[small]; len(got) != 1 {
			t.Errorf("retire = %v, want one: two of the three idle runners serve the 2 waiting assignments", got)
		}
	})
	t.Run("neverARunnerStillStarting", func(t *testing.T) {
		t.Parallel()
		l, big, small := setup(0)
		small.runners["a1"].state = starting
		l.capacity(big, epoch.Add(5*time.Minute), true)
		if got := l.retire()[small]; len(got) != 1 || got[0] != "a2" {
			t.Errorf("retire = %v, want a2 alone: a1 is still starting", got)
		}
	})
	t.Run("nothingWithoutTheReserve", func(t *testing.T) {
		t.Parallel()
		l, big, _ := setup(0)
		l.capacity(big, epoch.Add(time.Minute), true)
		if got := l.retire(); len(got) != 0 {
			t.Errorf("retire = %v before the head waited the reserve's 2 minutes, want none", got)
		}
	})
}

func TestARunnerOnAnUnreachableHostCoversNoAssignment(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 2, mem: 8}, {name: "b", cpus: 2, mem: 8}}, []profileSpec{{name: "p", hosts: []string{"a", "b"}, cpus: 2, mem: 2}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "p"})
	s := w.sets[0]
	w.queue(s, 1)
	w.sweep()
	w.begin(s)
	w.answer(s, 1)
	w.starts()
	if n := w.liveOn(0); n != 1 {
		t.Fatalf("runners on a = %d, want the assignment's", n)
	}
	w.reach(0, false)
	w.starts()
	if n := w.liveOn(1); n != 1 {
		t.Errorf("runners on b = %d, want one: a's idle runner may be gone with its host", n)
	}
}
