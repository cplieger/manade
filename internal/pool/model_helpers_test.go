package pool

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// A model of github.com and the hosts around the ledger, which drives it
// as the pool does. GitHub assigns a scale set at most its poll's figure
// less TotalAssignedJobs, only while a poll is in flight, and never cancels
// an assignment but to reassign it; a runner takes an assigned job of its
// scale set; a job whose runner died stays running in GitHub's counts
// until GitHub fails it, lostFor later. The model holds the truth (the
// containers and GitHub's jobs), which the ledger learns only from polls
// and exit reports, so its oracles read the model, not the ledger.

// lostFor is how long GitHub keeps a job running after its runner died;
// idleFor, how long the pool lets a runner with no job live (its profile's
// idle_timeout).
const (
	lostFor = 9 * time.Minute
	idleFor = 2 * time.Minute
)

type mjob struct {
	created time.Time
	lostAt  time.Time
	runner  string
	run     int64
}

// mset is one scale set as GitHub sees it.
type mset struct {
	start    time.Time
	b        *book
	scope    string
	ss       github.ScaleSet
	queue    []*mjob
	assigned []*mjob
	running  []*mjob
	pending  []github.Event
	figure   int
	profile  int
	org      bool
	inFlight bool
}

func (s *mset) stats() github.Stats {
	return github.Stats{Assigned: len(s.assigned) + len(s.running), Running: len(s.running)}
}

func (s *mset) jobs() int { return len(s.queue) + len(s.assigned) + len(s.running) }

// mrun is a runner's container.
type mrun struct {
	started  time.Time
	set      *mset
	job      *mjob
	name     string
	res      res
	host     int
	alive    bool
	reported bool
}

// mhost is a host's truth: its gates, and whether it refuses starts.
type mhost struct {
	ready     []bool
	reachable bool
	serving   bool
	refuse    bool
}

type world struct {
	now   time.Time
	t     testing.TB
	cfg   *config.Config
	l     *ledger
	runs  map[string]*mrun
	sets  []*mset
	hosts []mhost
	seq   int
	runID int64
}

// setSpec is a scale set: its scope, its profile, and whether the scope is
// an organization's.
type setSpec struct {
	scope, profile string
	org            bool
}

func newWorld(t testing.TB, cfg *config.Config, sets ...setSpec) *world {
	w := &world{t: t, cfg: cfg, now: epoch, runs: map[string]*mrun{}}
	for range cfg.Hosts {
		w.hosts = append(w.hosts, mhost{ready: make([]bool, len(cfg.Profiles)), reachable: true, serving: true})
	}
	for p := range cfg.Profiles {
		for _, name := range cfg.Profiles[p].Hosts {
			w.hosts[slices.IndexFunc(cfg.Hosts, func(h config.Host) bool { return h.Name == name })].ready[p] = true
		}
	}
	for _, sp := range sets {
		s := &mset{org: sp.org, scope: sp.scope}
		s.profile = slices.IndexFunc(cfg.Profiles, func(p config.Profile) bool { return p.Name == sp.profile })
		w.sets = append(w.sets, s)
	}
	w.boot()
	for _, s := range w.sets {
		s.ss = s.b.ss
	}
	return w
}

// boot starts a new process: a new ledger adopts every busy runner a host
// lists with the limits it was built with, removes the idle ones, and
// opens each scale set's session with GitHub's statistics.
func (w *world) boot() {
	w.l = newLedger(w.cfg)
	w.gates()
	for _, s := range w.sets {
		s.b = served(w.l, s.scope, w.cfg.Profiles[s.profile].Name, s.org)
		s.pending, s.inFlight = nil, false
	}
	for _, name := range w.names() {
		r := w.runs[name]
		switch {
		case !r.alive:
			r.reported = true
		case r.job != nil:
			r.set.b.adopt(name, r.host, r.res, busy)
		default:
			r.alive, r.reported = false, true
		}
	}
	for _, s := range w.sets {
		w.l.opened(s.b, s.stats(), w.now)
	}
}

// names answers the containers' names in a stable order.
func (w *world) names() []string {
	var out []string
	for name := range w.runs {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// gates copies the hosts' truth to the ledger and settles it.
func (w *world) gates() {
	for h, mh := range w.hosts {
		hb := &w.l.hosts[h]
		hb.reachable, hb.serving = mh.reachable, mh.reachable && mh.serving
		copy(hb.ready, mh.ready)
	}
	w.l.settle(w.now)
}

func (w *world) advance(d time.Duration) {
	w.now = w.now.Add(d)
	w.github()
}

// github fails each job whose runner died lostFor ago.
func (w *world) github() {
	for _, s := range w.sets {
		s.running = slices.DeleteFunc(s.running, func(j *mjob) bool {
			if j.lostAt.IsZero() || w.now.Sub(j.lostAt) < lostFor {
				return false
			}
			s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.run, Name: "job", Runner: j.runner, Result: "failed", Finished: w.now})
			return true
		})
	}
}

// queue adds n jobs to s's queue, created now.
func (w *world) queue(s *mset, n int) {
	for range n {
		w.runID++
		s.queue = append(s.queue, &mjob{created: w.now, run: w.runID})
	}
}

// sweep hands the ledger each repository scale set's queued jobs: those no
// runner took.
func (w *world) sweep() {
	for _, s := range w.sets {
		if s.org {
			continue
		}
		var jobs []github.QueuedJob
		for _, j := range append(slices.Clone(s.queue), s.assigned...) {
			jobs = append(jobs, job(j.run, w.cfg.Profiles[s.profile].Name, j.created))
		}
		w.l.reread(s.b, jobs, w.now)
	}
}

// begin starts a poll of s at the figure the ledger reports.
func (w *world) begin(s *mset) {
	s.figure, _ = w.l.capacity(s.b, w.now, true)
	s.start, s.inFlight = w.now, true
	if s.figure < 0 {
		w.t.Fatalf("%v reports a figure of %d", s.ss, s.figure)
	}
}

// room is how many jobs GitHub may assign s during its poll in flight.
func (s *mset) room() int { return max(0, s.figure-s.stats().Assigned) }

// answer ends s's poll: GitHub assigns up to n of its queued jobs the
// figure allows, oldest first, and sends every pending message.
func (w *world) answer(s *mset, n int) {
	k := min(s.room(), n, len(s.queue))
	for _, j := range s.queue[:k] {
		s.pending = append(s.pending, github.Event{Kind: github.Assigned, RunID: j.run, Name: "job"})
	}
	s.assigned = append(s.assigned, s.queue[:k]...)
	s.queue = slices.Clone(s.queue[k:])
	events := s.pending
	s.pending, s.inFlight = nil, false
	st := s.stats()
	w.l.polled(s.b, &github.Poll{Started: s.start, Ended: w.now, Stats: &st, Events: events, Idle: len(events) == 0})
}

// starts runs the start worker: a runner for each assignment the ledger
// gives a slot, its start refused on a host that refuses starts, after
// which the scale set's starts wait for the next run. It checks every
// host's containers stay within its budget.
func (w *world) starts() {
	for _, s := range w.sets {
		for {
			w.seq++
			name := fmt.Sprintf("r%d", w.seq)
			h, ok := w.l.take(s.b, name)
			if !ok {
				break
			}
			mh := &w.hosts[h]
			if !mh.reachable || !mh.serving || !mh.ready[s.profile] {
				w.t.Fatalf("%v started %s on host %s, which does not take it (%+v)", s.ss, name, w.cfg.Hosts[h].Name, *mh)
			}
			if mh.refuse {
				w.l.end(s.b, name, w.now)
				break
			}
			w.runs[name] = &mrun{started: w.now, set: s, name: name, res: w.l.slot(s.profile, h), host: h, alive: true}
			w.checkBudget(h)
		}
	}
}

// retire removes each spare runner the ledger names, as the pool removes
// an idle runner: its registration first, which GitHub refuses once the
// runner took a job, then its container, whose end its host reports.
func (w *world) retire() {
	for _, names := range w.l.retire() {
		for _, name := range names {
			if r := w.runs[name]; r != nil && r.alive && r.job == nil {
				w.exit(r)
			}
		}
	}
}

// checkBudget fails the run when host h's live containers hold more than
// its budget.
func (w *world) checkBudget(h int) {
	var held res
	for _, r := range w.runs {
		if r.alive && r.host == h {
			held = held.plus(r.res)
		}
	}
	if !held.within(w.l.hosts[h].budget) {
		w.t.Fatalf("host %s's containers hold %+v, past its budget %+v", w.cfg.Hosts[h].Name, held, w.l.hosts[h].budget)
	}
}

// takeJobs has each live runner with no job take its scale set's oldest
// assigned job.
func (w *world) takeJobs() {
	for _, name := range w.names() {
		r := w.runs[name]
		s := r.set
		if !r.alive || r.job != nil || len(s.assigned) == 0 {
			continue
		}
		j := s.assigned[0]
		s.assigned = s.assigned[1:]
		j.runner, r.job = name, j
		s.running = append(s.running, j)
		s.pending = append(s.pending, github.Event{Kind: github.Started, RunID: j.run, Name: "job", Runner: name})
	}
}

// finish ends the job of runner r: GitHub sends its JobCompleted and its
// container exits.
func (w *world) finish(r *mrun) {
	s, j := r.set, r.job
	s.running = slices.DeleteFunc(s.running, func(o *mjob) bool { return o == j })
	s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.run, Name: "job", Runner: r.name, Result: "succeeded", Finished: w.now})
	w.exit(r)
}

// exit ends r's container; its host reports it if it answers.
func (w *world) exit(r *mrun) {
	r.alive = false
	if w.hosts[r.host].reachable {
		w.report(r)
	}
}

func (w *world) report(r *mrun) {
	r.reported = true
	if _, ok := r.set.b.runners[r.name]; ok {
		w.l.end(r.set.b, r.name, w.now)
	}
}

// crash kills every container on host h. A job its runner held stays
// running in GitHub's counts until GitHub fails it.
func (w *world) crash(h int) {
	for _, name := range w.names() {
		if r := w.runs[name]; r.alive && r.host == h {
			if r.job != nil {
				r.job.lostAt = w.now
			}
			w.exit(r)
		}
	}
}

// stop is a host's SIGTERM: each runner fails its job and reports it, and
// its container exits.
func (w *world) stop(h int) {
	for _, name := range w.names() {
		r := w.runs[name]
		if !r.alive || r.host != h {
			continue
		}
		if r.job == nil {
			w.exit(r)
			continue
		}
		s, j := r.set, r.job
		s.running = slices.DeleteFunc(s.running, func(o *mjob) bool { return o == j })
		s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.run, Name: "job", Runner: r.name, Result: "failed", Finished: w.now})
		w.exit(r)
	}
}

// reach sets whether host h answers; once it does, it reports every
// container that ended meanwhile.
func (w *world) reach(h int, on bool) {
	w.hosts[h].reachable = on
	if on {
		for _, name := range w.names() {
			if r := w.runs[name]; !r.alive && !r.reported && r.host == h {
				w.report(r)
			}
		}
	}
	w.gates()
}

// cancel is GitHub's reassignment cancel of s's newest assigned job, which
// goes back to the queue.
func (*world) cancel(s *mset) {
	if len(s.assigned) == 0 || s.inFlight {
		return
	}
	j := s.assigned[len(s.assigned)-1]
	s.assigned = s.assigned[:len(s.assigned)-1]
	s.queue = append([]*mjob{j}, s.queue...)
	slices.SortStableFunc(s.queue, func(x, y *mjob) int { return x.created.Compare(y.created) })
	s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.run, Name: "job", Result: "canceled"})
}

// cycle is one poll of s answered at once with every job its figure
// allows, followed by the start worker and the runners taking their jobs.
func (w *world) cycle(s *mset) {
	w.sweep()
	w.begin(s)
	w.answer(s, len(s.queue))
	w.retire()
	w.starts()
	w.takeJobs()
}

// deliver runs an idle poll of s, which hands the ledger every pending
// message.
func (w *world) deliver(s *mset) {
	w.begin(s)
	w.answer(s, 0)
}

// stuck describes, by scale set, each that holds an assignment no runner
// took although a host of its profile has room for one more runner and the
// profile is under its cap. The room is the host's budget less its live
// containers, the model's truth.
func (w *world) stuck() map[*mset]string {
	out := map[*mset]string{}
	for _, s := range w.sets {
		if len(s.assigned) == 0 {
			continue
		}
		pr := &w.cfg.Profiles[s.profile]
		live := 0
		for _, r := range w.runs {
			if r.alive && r.set.profile == s.profile {
				live++
			}
		}
		if pr.MaxRunners > 0 && live >= pr.MaxRunners {
			continue
		}
		for _, h := range w.l.profiles[s.profile].hosts {
			free := w.l.hosts[h].budget
			for _, r := range w.runs {
				if r.alive && r.host == h {
					free = free.minus(r.res)
				}
			}
			if w.l.slot(s.profile, h).within(free) {
				out[s] = fmt.Sprintf("%v: %d assigned with no runner, host %s free %+v", s.ss, len(s.assigned), w.cfg.Hosts[h].Name, free)
				break
			}
		}
	}
	return out
}

// heal ends every fault: each host answers, serves, takes starts and holds
// its profiles' images.
func (w *world) heal() {
	for h := range w.hosts {
		mh := &w.hosts[h]
		mh.serving, mh.refuse = true, false
		for p := range w.cfg.Profiles {
			mh.ready[p] = slices.Contains(w.cfg.Profiles[p].Hosts, w.cfg.Hosts[h].Name)
		}
		w.reach(h, true)
	}
}

// drain heals the world, lets GitHub fail every lost job and the
// unassignable holds lapse, then runs passes in which each running job
// ends, each runner idle past idleFor is removed, and each scale set polls
// once. From the second pass on, no
// assignment may wait while a host of its profile has room, and within
// passes every job must have run.
func (w *world) drain(passes int) {
	w.heal()
	w.advance(skipMax + lostFor)
	var before map[*mset]string
	for pass := range passes {
		for _, name := range w.names() {
			switch r := w.runs[name]; {
			case r.alive && r.job != nil:
				w.finish(r)
			case r.alive && w.now.Sub(r.started) >= idleFor:
				w.exit(r)
			}
		}
		for _, s := range w.sets {
			w.cycle(s)
		}
		w.advance(30 * time.Second)
		now := w.stuck()
		for s, why := range now {
			if _, again := before[s]; again && pass > 1 {
				w.t.Fatalf("pass %d: an assignment waits beside room for two passes: %s", pass, why)
			}
		}
		before = now
		if !slices.ContainsFunc(w.sets, func(s *mset) bool { return s.jobs() > 0 }) {
			return
		}
	}
	var left []string
	for _, s := range w.sets {
		left = append(left, fmt.Sprintf("%v queued %d assigned %d running %d", s.ss, len(s.queue), len(s.assigned), len(s.running)))
	}
	w.t.Fatalf("jobs left after %d passes: %s", passes, strings.Join(left, "; "))
}
