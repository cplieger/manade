package pool

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// The pool's properties over a model of github.com: GitHub assigns a scale
// set at most maxCapacity - TotalAssignedJobs jobs during a poll and none
// outside one, a runner takes an assigned job, and a job ends or is
// cancelled for reassignment. A job's JobCompleted arrives with the next
// answer, after its container exited. Polls of different scale sets
// overlap. The model knows GitHub's true counts, which the pool only learns
// from messages, so its oracles do not read the pool's records.

// mjob is a queued job in the model.
type mjob struct {
	j      github.QueuedJob
	runner string
	set    *mset
}

// mset is one scale set as GitHub sees it.
type mset struct {
	ss github.ScaleSet
	// assigned holds its jobs no runner took; running, those a runner did.
	assigned []*mjob
	running  map[string]*mjob
	// ended holds when each runner whose JobCompleted GitHub has not yet
	// sent ended.
	ended   map[string]time.Time
	pending []github.Event
	start   time.Time
	scope   string
	cpu     int64
	figure  int
	// told is the total the pool last heard; live its runners.
	told     int
	live     int
	inFlight bool
	org      bool
}

func (s *mset) total() int { return len(s.assigned) + len(s.running) }

type model struct {
	fx    *fixture
	sets  []*mset
	jobs  map[string][]*mjob
	in    bytesIn
	runID int64
}

func newModel(t *testing.T, data []byte, cfg *config.Config) *model {
	md := &model{fx: newFixtureFrom(t, cfg, appDefault, appLarge, libDefault, orgDefault), jobs: map[string][]*mjob{}, in: bytesIn(data)}
	for _, ss := range []github.ScaleSet{appDefault, appLarge, libDefault, orgDefault} {
		cpu := int64(2000)
		if ss.Profile == "large" {
			cpu = 4000
		}
		md.sets = append(md.sets, &mset{ss: ss, scope: ss.Scope, cpu: cpu, running: map[string]*mjob{}, ended: map[string]time.Time{}, org: ss.Scope == "acme"})
	}
	return md
}

// unassigned is the model's queued jobs of s no assignment holds.
func (md *model) unassigned(s *mset) []*mjob {
	var out []*mjob
	for _, j := range md.jobs[s.scope] {
		if j.set == nil && j.runner == "" && j.j.Profile == s.ss.Profile {
			out = append(out, j)
		}
	}
	return out
}

// sweep hands the pool each repo scope's queued jobs: those no runner took.
func (md *model) sweep() {
	reads := map[string][]github.QueuedJob{}
	for _, scope := range []string{"acme/app", "acme/lib"} {
		reads[scope] = []github.QueuedJob{}
		for _, j := range md.jobs[scope] {
			if j.runner == "" {
				reads[scope] = append(reads[scope], j.j)
			}
		}
	}
	md.fx.sweep(reads)
}

func (md *model) queue() {
	scope, profile := "acme/lib", "default"
	switch md.in.n(3) {
	case 1:
		scope = "acme/app"
	case 2:
		scope, profile = "acme/app", "large"
	}
	for range 1 + md.in.n(3) {
		md.runID++
		md.jobs[scope] = append(md.jobs[scope], &mjob{j: github.QueuedJob{
			Profile: profile, RunID: md.runID, Name: "job", Created: md.fx.at,
		}})
	}
}

// begin starts a poll of s: the pool's figure is what GitHub may fill.
func (md *model) begin(t *testing.T, s *mset) {
	s.start, s.figure, s.inFlight = md.fx.at, md.fx.capacity(s.ss), true
	held := int64(0)
	for _, o := range md.sets {
		n := max(o.total(), o.live)
		if o.inFlight {
			n = max(n, o.figure)
		}
		held += int64(n) * o.cpu
	}
	if held > 8000 {
		t.Fatalf("GitHub may fill %d milli-CPUs of slots, past the budget's 8000", held)
	}
	waiting := len(md.unassigned(s))
	// A cancelled job is waiting again in the model, so it covers the
	// cancel's kept slot.
	if extra, idle := s.figure-s.told, max(0, s.live-s.told); extra > idle+waiting+1 {
		t.Fatalf("%v reports %d past its assignments, with %d idle runners and %d waiting jobs", s.ss.ScaleSet, extra, idle, waiting)
	}
}

// answer ends s's poll: GitHub assigns some of what the figure allows and
// sends every pending message.
func (md *model) answer(s *mset) {
	room := max(0, s.figure-s.total())
	events := s.pending
	s.pending = nil
	if s.org {
		// The pool cannot read an organization's queue: its jobs appear
		// only as GitHub assigns them.
		for range md.in.n(3) {
			md.runID++
			md.jobs[s.scope] = append(md.jobs[s.scope], &mjob{j: github.QueuedJob{Profile: s.ss.Profile, RunID: md.runID, Name: "org-job"}})
		}
	}
	for _, j := range md.unassigned(s)[:min(room, md.in.n(4), len(md.unassigned(s)))] {
		j.set = s
		s.assigned = append(s.assigned, j)
		events = append(events, github.Event{Kind: github.Assigned, RunID: j.j.RunID, Name: j.j.Name})
	}
	for _, e := range events {
		if e.Kind == github.Completed {
			delete(s.ended, e.Runner)
		}
	}
	d := []time.Duration{5 * time.Second, 50 * time.Second}[md.in.n(2)]
	md.fx.advance(d)
	s.inFlight, s.told = false, s.total()
	md.fx.p.Polled(s.ss, &github.Poll{Started: s.start, Ended: md.fx.at, Stats: &github.Stats{Assigned: s.total(), Running: len(s.running)}, Events: events, Idle: len(events) == 0})
}

// stillCounted is how long the pool takes GitHub's total to count a runner
// that ended with no JobCompleted: about one poll.
const stillCounted = time.Minute

// starts runs the start worker once: it must start exactly what GitHub's
// assignments lack, each against a reservation.
func (md *model) starts(t *testing.T) {
	// The pool learns assignments only from messages: what it starts
	// follows each scale set's total as of its last answer, which still
	// counts a job whose runner ended until its JobCompleted arrives.
	want := map[*mset]int{}
	sum := 0
	for _, s := range md.sets {
		counted := 0
		for _, at := range s.ended {
			if md.fx.at.Sub(at) < stillCounted {
				counted++
			}
		}
		want[s] = max(0, s.told-s.live-counted)
		sum += want[s]
	}
	if got := md.fx.startAll(t); got != sum {
		t.Fatalf("started %d runners, want %d", got, sum)
	}
	for _, s := range md.sets {
		s.live += want[s]
	}
}

// take has an idle runner of s take its oldest assigned job.
func (md *model) take(s *mset) {
	if len(s.assigned) == 0 {
		return
	}
	md.fx.p.mu.Lock()
	var name string
	for n, r := range md.fx.p.set(s.ss).runners {
		if _, taken := s.running[n]; !taken && r.state == idle {
			name = n
			break
		}
	}
	md.fx.p.mu.Unlock()
	if name == "" {
		return
	}
	j := s.assigned[0]
	s.assigned = s.assigned[1:]
	j.runner, s.running[name] = name, j
	s.pending = append(s.pending, github.Event{Kind: github.Started, RunID: j.j.RunID, Name: j.j.Name, Runner: name})
}

// finish ends a running job of s; its container exits and a reconciler
// pass removes it and its record.
func (md *model) finish(t *testing.T, s *mset) {
	for name, j := range s.running {
		delete(s.running, name)
		md.drop(j)
		s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.j.RunID, Name: j.j.Name, Runner: name, Result: "succeeded"})
		md.fx.dk.exit(name, 0, false)
		if listed, _ := md.fx.p.reconcile(t.Context()); !listed {
			t.Fatal("the reconciler pass failed")
		}
		s.live--
		s.ended[name] = md.fx.at
		return
	}
}

func (md *model) drop(j *mjob) {
	for scope, js := range md.jobs {
		for i, o := range js {
			if o == j {
				md.jobs[scope] = append(js[:i:i], js[i+1:]...)
				return
			}
		}
	}
}

// cancel is GitHub's reassignment cancel of s's newest unstarted job.
func (*model) cancel(s *mset) {
	if len(s.assigned) == 0 || s.inFlight {
		return
	}
	j := s.assigned[len(s.assigned)-1]
	s.assigned = s.assigned[:len(s.assigned)-1]
	j.set = nil
	s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.j.RunID, Name: j.j.Name, Result: "canceled"})
}

func (md *model) checkReserved(t *testing.T) {
	var want int64
	for _, s := range md.sets {
		want += int64(s.live) * s.cpu
	}
	if got := md.fx.p.Snapshot().Reserved.MilliCPUs; got != want {
		t.Fatalf("reserved = %d milli-CPUs, want the live runners' %d", got, want)
	}
}

func (md *model) step(t *testing.T) {
	s := md.sets[md.in.n(len(md.sets))]
	switch md.in.n(9) {
	case 0:
		md.queue()
		md.sweep()
	case 1, 2:
		if !s.inFlight {
			md.begin(t, s)
		}
	case 3:
		if s.inFlight {
			md.answer(s)
		}
	case 4:
		md.starts(t)
	case 5:
		md.take(s)
	case 6:
		md.finish(t, s)
	case 7:
		md.cancel(s)
	case 8:
		md.fx.advance(time.Duration(md.in.n(90)) * time.Second)
		if md.in.n(4) != 0 {
			md.fx.proveAll()
		}
		md.sweep()
	}
	md.checkReserved(t)
}

func poolSeeds(f *testing.F) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 200 {
		b := make([]byte, 400)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		f.Add(b)
	}
}

func FuzzPool_holdsTheBudget(f *testing.F) {
	poolSeeds(f)
	// No runner reaches its idle_timeout or max_lifetime within a run, so
	// the reconciler removes only the containers whose jobs ended.
	long := strings.NewReplacer("memory: 4GiB}", "memory: 4GiB, idle_timeout: 24h, max_lifetime: 24h}",
		"memory: 8GiB,", "memory: 8GiB, idle_timeout: 24h, max_lifetime: 24h,").Replace(layout)
	cfg := load(f, long)
	f.Fuzz(func(t *testing.T, data []byte) {
		md := newModel(t, data, cfg)
		for len(md.in) > 0 {
			md.step(t)
		}
	})
}
