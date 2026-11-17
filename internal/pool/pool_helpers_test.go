package pool

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// A model of GitHub around a running pool over fake hosts, so the start
// path (JIT configs, POSTs, runner lists, exits, adoption, generation
// changes) is the one the control plane runs. GitHub assigns a scale set
// at most its poll's figure less TotalAssignedJobs, a running container with no job takes its scale
// set's oldest assigned job, a job whose container ended without finishing
// stays running in GitHub's counts until GitHub fails it, lostFor later,
// and an ephemeral runner's registration goes with its job.

type sjob struct {
	created, lostAt time.Time
	runner          string
	run, id         int64
}

// sset is one scale set as GitHub sees it.
type sset struct {
	scope, profile           string
	queue, assigned, running []*sjob
	pending                  []github.Event
	// openedID is the scale set whose session the model opened, 0 before
	// one: GitHub opens a session for each scale set ID.
	openedID int
}

func (s *sset) stats() github.Stats {
	return github.Stats{Available: len(s.queue), Assigned: len(s.assigned) + len(s.running), Running: len(s.running)}
}

func (s *sset) jobs() int { return len(s.queue) + len(s.assigned) + len(s.running) }

type smodel struct {
	r     *rig
	jobOf map[string]*sjob
	sets  []*sset
	runID int64
	// added counts the scopes the run added; lost, the scale sets GitHub
	// dropped.
	added, lost int
}

// newModel boots a quiet rig with every allowed scale set opened.
func newModel(r *rig) *smodel {
	m := &smodel{r: r, jobOf: map[string]*sjob{}}
	for _, b := range r.p.l.books {
		m.sets = append(m.sets, &sset{scope: b.ss.Scope, profile: b.ss.Profile})
	}
	m.open()
	return m
}

func (m *smodel) book(s *sset) *book { return m.r.book(s.scope, s.profile) }

// open hands each scale set's new session its statistics; a scale set
// the generation does not hold yet has no session.
func (m *smodel) open() {
	for _, s := range m.sets {
		if b := m.book(s); b != nil {
			m.r.reg.Opened(b.ss, s.stats())
			s.openedID = b.ss.ID
		}
	}
}

// routed reports that new jobs reach s: its scope is served and not
// stopping, and its profile may take the scope's new jobs. A scale set
// being stopped gets no new job, as the runner variable's delete or
// rewrite before the stop makes sure (§9.4).
func (m *smodel) routed(s *sset) bool {
	i := slices.IndexFunc(m.r.cfg.Scopes, func(sc config.Scope) bool { return sc.Key == s.scope })
	pr, ok := m.r.cfg.Profile(s.profile)
	return i >= 0 && !m.r.cfg.Scopes[i].Stopping && ok && pr.Allowed(s.scope) && !pr.Deleting && !slices.Contains(pr.Retiring, s.scope)
}

func (m *smodel) queue(s *sset, n int) {
	if !m.routed(s) {
		return
	}
	for range n {
		m.runID++
		s.queue = append(s.queue, &sjob{created: m.r.now, run: m.runID})
	}
}

// sweep hands the pool every scope's queued jobs: those no runner took.
func (m *smodel) sweep() {
	byScope := map[string][]github.QueuedJob{}
	for _, s := range m.sets {
		for _, j := range append(slices.Clone(s.queue), s.assigned...) {
			byScope[s.scope] = append(byScope[s.scope], github.QueuedJob{Created: j.created, Name: "job", Profile: s.profile, RunID: j.run})
		}
	}
	var qs []github.Queue
	for _, key := range slices.Sorted(maps.Keys(m.scopes())) {
		qs = append(qs, github.Queue{Scope: key, Jobs: byScope[key], Complete: true})
	}
	m.r.p.queued(m.r.now, qs)
}

func (m *smodel) scopes() map[string]bool {
	out := map[string]bool{}
	for _, s := range m.sets {
		out[s.scope] = true
	}
	return out
}

// poll runs one poll of s: GitHub assigns up to n queued jobs the figure
// allows, oldest first, and sends every pending message.
func (m *smodel) poll(s *sset, n int) {
	b := m.book(s)
	if b == nil {
		s.openedID = 0
		return
	}
	if m.r.p.sets[b].lost {
		return
	}
	if s.openedID != b.ss.ID {
		m.r.reg.Opened(b.ss, s.stats())
		s.openedID = b.ss.ID
	}
	figure := m.r.reg.Capacity(b.ss, m.r.now)
	if figure < 0 {
		m.r.t.Fatalf("%v reports a figure of %d", b.ss, figure)
	}
	k := min(max(0, figure-s.stats().Assigned), n, len(s.queue))
	for _, j := range s.queue[:k] {
		s.pending = append(s.pending, github.Event{Kind: github.Assigned, RunID: j.run, Name: "job"})
	}
	s.assigned = append(s.assigned, s.queue[:k]...)
	s.queue = slices.Clone(s.queue[k:])
	events := s.pending
	s.pending = nil
	st := s.stats()
	m.r.reg.Polled(b.ss, &github.Poll{Started: m.r.now, Ended: m.r.now, Stats: &st, Events: events, Idle: len(events) == 0})
}

// containers answers every running container across the hosts, by name.
func (m *smodel) containers() map[string]hostapi.Runner {
	out := map[string]hostapi.Runner{}
	for _, h := range m.r.hosts {
		h.mu.Lock()
		for name, rr := range h.runs {
			if rr.State == hostapi.StateRunning {
				out[name] = *rr
			}
		}
		h.mu.Unlock()
	}
	return out
}

func (m *smodel) setOf(rr *hostapi.Runner) *sset {
	for _, s := range m.sets {
		if s.scope == rr.Scope && s.profile == rr.Profile {
			return s
		}
	}
	return nil
}

// take has each running container with no job take its scale set's oldest
// assigned job.
func (m *smodel) take() {
	cs := m.containers()
	for _, name := range slices.Sorted(maps.Keys(cs)) {
		rr := cs[name]
		s := m.setOf(&rr)
		if _, held := m.jobOf[name]; held || s == nil || len(s.assigned) == 0 {
			continue
		}
		j := s.assigned[0]
		s.assigned = s.assigned[1:]
		j.runner, j.id = name, rr.RunnerID
		s.running = append(s.running, j)
		m.jobOf[name] = j
		s.pending = append(s.pending, github.Event{Kind: github.Started, RunID: j.run, Name: "job", Runner: name})
		m.r.gh.set(func(g *fakeGitHub) { g.busy[j.id] = true })
	}
}

// finish ends the job of runner name: GitHub sends its JobCompleted, the
// ephemeral runner's registration goes, and its container exits.
func (m *smodel) finish(name string) {
	j := m.jobOf[name]
	delete(m.jobOf, name)
	s := m.owner(j)
	s.running = slices.DeleteFunc(s.running, func(o *sjob) bool { return o == j })
	s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.run, Name: "job", Runner: name, Result: "succeeded", Finished: m.r.now})
	m.r.gh.set(func(g *fakeGitHub) { delete(g.busy, j.id); delete(g.regs, j.id) })
	for _, h := range m.r.hosts {
		h.exit(name, 0)
	}
}

func (m *smodel) owner(j *sjob) *sset {
	for _, s := range m.sets {
		if slices.Contains(s.running, j) {
			return s
		}
	}
	m.r.t.Fatalf("job %d is running in no scale set", j.run)
	return nil
}

// reap marks lost each job whose container no longer runs.
func (m *smodel) reap() {
	cs := m.containers()
	for name, j := range m.jobOf {
		if _, ok := cs[name]; !ok {
			j.lostAt = m.r.now
			delete(m.jobOf, name)
		}
	}
}

// advance moves the clock; GitHub fails each job lost lostFor ago and
// deletes its runner's registration.
func (m *smodel) advance(d time.Duration) {
	m.reap()
	m.r.now = m.r.now.Add(d)
	for _, s := range m.sets {
		s.running = slices.DeleteFunc(s.running, func(j *sjob) bool {
			if j.lostAt.IsZero() || m.r.now.Sub(j.lostAt) < lostFor {
				return false
			}
			s.pending = append(s.pending, github.Event{Kind: github.Completed, RunID: j.run, Name: "job", Runner: j.runner, Result: "failed", Finished: m.r.now})
			m.r.gh.set(func(g *fakeGitHub) { delete(g.busy, j.id); delete(g.regs, j.id) })
			return true
		})
	}
}

// step runs one choice: GitHub's, a host's, the control plane's or the
// clock's.
func (m *smodel) step(in *bytesIn) {
	s := m.sets[in.n(len(m.sets))]
	h := m.r.hosts[in.n(len(m.r.hosts))]
	switch in.n(16) {
	case 0, 1:
		m.queue(s, 1+in.n(3))
		m.sweep()
	case 2, 3:
		m.poll(s, in.n(4))
	case 4:
		m.r.starts()
		m.take()
	case 5:
		if names := slices.Sorted(maps.Keys(m.jobOf)); len(names) > 0 {
			m.finish(names[in.n(len(names))])
		}
	case 6:
		m.r.pass()
		m.settle()
	case 7:
		h.set(func(f *fakeHost) {
			switch in.n(3) {
			case 0:
				f.down = !f.down
			case 1:
				f.disconnected = !f.disconnected
			default:
				// A host booting its section answers its status alone.
				if f.notUp == nil {
					f.notUp = hostapi.ErrStarting
				} else {
					f.notUp = nil
				}
			}
		})
	case 8:
		m.reap()
		h.restart(255)
		m.reap()
	case 9:
		h.set(func(f *fakeHost) {
			switch in.n(4) {
			case 0:
				f.refuse = !f.refuse
			case 1:
				f.failStart = !f.failStart
			case 2:
				f.timeouts = in.n(3)
			default:
				p := m.r.cfg.Profiles[in.n(len(m.r.cfg.Profiles))].Name
				f.notReady[p] = !f.notReady[p]
			}
		})
	case 10:
		switch in.n(3) {
		case 0:
			m.r.gh.set(func(g *fakeGitHub) { g.jitFail = !g.jitFail })
		case 1:
			m.r.gh.set(func(g *fakeGitHub) { g.ensureFail = !g.ensureFail })
		default:
			m.lose(s)
		}
	case 11:
		if in.n(3) == 0 {
			m.restart()
		}
	case 12, 13:
		m.reconfigure(in)
	default:
		m.advance(time.Duration(in.n(120)) * time.Second)
	}
}

// lose has GitHub drop s's scale set, as it purges one no session polled
// for a day: its assigned jobs wait in the queue again, the pool's next
// session create answers not found, and an ensure creates the scale set
// anew under another ID.
func (m *smodel) lose(s *sset) {
	b := m.book(s)
	if b == nil || m.r.p.sets[b].lost {
		return
	}
	m.lost++
	s.queue = append(slices.Clone(s.assigned), s.queue...)
	s.assigned = nil
	s.pending = slices.DeleteFunc(s.pending, func(e github.Event) bool { return e.Kind == github.Assigned })
	m.r.gh.set(func(g *fakeGitHub) { g.ids[setKey{s.scope, s.profile}] = 1000 + m.lost })
	m.r.reg.Lost(b.ss)
}

// reconfigure commits a new settings revision: a profile resized within
// every host it lists, a profile's max_runners, hosts or their order, a
// host's capacity above every profile it serves, the head reserve, a
// scope turned off or back on, a profile leaving a scope or coming back
// to it, a new scope, or the same settings again. A host may be left
// running its previous section.
func (m *smodel) reconfigure(in *bytesIn) {
	cfg := cloneConfig(m.r.cfg)
	switch in.n(9) {
	case 0:
		pr := &cfg.Profiles[in.n(len(cfg.Profiles))]
		cpus, mem := int64(1<<40), int64(1<<40)
		for _, name := range pr.Hosts {
			h, _ := cfg.Host(name)
			cpus, mem = min(cpus, h.Capacity.MilliCPUs/1000), min(mem, h.Capacity.Memory/gib)
		}
		pr.MilliCPUs, pr.Memory = (1+int64(in.n(int(cpus))))*1000, (1+int64(in.n(int(mem))))*gib
	case 1:
		cfg.Profiles[in.n(len(cfg.Profiles))].MaxRunners = []int{0, 0, 1, 3}[in.n(4)]
	case 2:
		pr := &cfg.Profiles[in.n(len(cfg.Profiles))]
		var fit []string
		for i := range cfg.Hosts {
			if c := cfg.Hosts[i].Capacity; c.MilliCPUs >= pr.MilliCPUs && c.Memory >= pr.Memory {
				fit = append(fit, cfg.Hosts[i].Name)
			}
		}
		perm := rand.New(rand.NewPCG(uint64(in.n(256)), 0)).Perm(len(fit))
		pr.Hosts = nil
		for _, k := range perm[:1+in.n(len(fit))] {
			pr.Hosts = append(pr.Hosts, fit[k])
		}
	case 3:
		h := &cfg.Hosts[in.n(len(cfg.Hosts))]
		cpus, mem := int64(1), int64(1)
		for i := range cfg.Profiles {
			if pr := &cfg.Profiles[i]; slices.Contains(pr.Hosts, h.Name) {
				cpus, mem = max(cpus, pr.MilliCPUs/1000), max(mem, pr.Memory/gib)
			}
		}
		h.Capacity.MilliCPUs, h.Capacity.Memory = (cpus+int64(in.n(8)))*1000, (mem+int64(in.n(16)))*gib
		h.Capacity.MaxRunners = []int{0, 0, 2, 4}[in.n(4)]
	case 4:
		cfg.HeadReserveAfter = time.Duration(in.n(4)) * time.Minute
	case 5:
		if len(cfg.Scopes) > 0 {
			sc := &cfg.Scopes[in.n(len(cfg.Scopes))]
			sc.Stopping = !sc.Stopping
		}
	case 6:
		m.retire(cfg, m.sets[in.n(len(m.sets))])
	case 7:
		m.addScope(cfg)
	}
	var lagging []string
	if in.n(4) == 0 {
		lagging = append(lagging, m.r.hosts[in.n(len(m.r.hosts))].name)
	}
	m.r.apply(cfg, lagging...)
}

// retire has s's profile leave s's scope, or come back to it when it is
// leaving already.
func (*smodel) retire(cfg *config.Config, s *sset) {
	pr, ok := cfg.Profile(s.profile)
	switch {
	case !ok || !pr.Serves(s.scope) || !slices.ContainsFunc(cfg.Scopes, func(sc config.Scope) bool { return sc.Key == s.scope }):
	case slices.Contains(pr.Retiring, s.scope):
		pr.Retiring = slices.DeleteFunc(slices.Clone(pr.Retiring), func(k string) bool { return k == s.scope })
	default:
		pr.Retiring = append(slices.Clone(pr.Retiring), s.scope)
	}
}

// addScope serves a new repository scope with every profile that serves
// it, each a scale set GitHub models from then on.
func (m *smodel) addScope(cfg *config.Config) {
	m.added++
	key := fmt.Sprintf("acme/n%d", m.added)
	cfg.Scopes = append(cfg.Scopes, config.Scope{Key: key, Owner: "acme", Repo: fmt.Sprintf("n%d", m.added), Kind: config.ScopeRepo, DefaultProfile: defaultOf(cfg, key)})
	for i := range cfg.Profiles {
		if cfg.Profiles[i].Serves(key) {
			m.sets = append(m.sets, &sset{scope: key, profile: cfg.Profiles[i].Name})
		}
	}
}

// settle commits what the settings owner does with each stop the pool
// handed over at the revision in force: the scope written off, or the
// retiring entry cleared with the scope taken from the profile. A stop of
// an older revision is dropped.
func (m *smodel) settle() {
	cfg, ended := cloneConfig(m.r.cfg), false
	for _, call := range m.r.settler.take() {
		if call.rev != m.r.rev {
			continue
		}
		for _, st := range call.ended {
			ended = true
			if st.Profile == "" {
				cfg.Scopes = slices.DeleteFunc(cfg.Scopes, func(sc config.Scope) bool { return sc.Key == st.Scope })
				continue
			}
			pr, _ := cfg.Profile(st.Profile)
			pr.Retiring = slices.DeleteFunc(slices.Clone(pr.Retiring), func(k string) bool { return k == st.Scope })
			if pr.Scopes == nil {
				pr.Scopes = []string{}
				for _, sc := range cfg.Scopes {
					pr.Scopes = append(pr.Scopes, sc.Key)
				}
			}
			pr.Scopes = slices.DeleteFunc(slices.Clone(pr.Scopes), func(k string) bool { return k == st.Scope })
		}
	}
	if ended {
		m.r.apply(cfg)
	}
}

// restart is a new control-plane process over the same hosts and GitHub.
func (m *smodel) restart() {
	m.r.restart()
	m.open()
}

// heal ends every fault.
func (m *smodel) heal() {
	for _, h := range m.r.hosts {
		h.set(func(f *fakeHost) {
			f.down, f.disconnected, f.refuse, f.failStart, f.timeouts, f.notUp = false, false, false, false, 0, nil
			f.section = sectionHash(m.r.rev)
			clear(f.notReady)
		})
	}
	m.r.gh.set(func(g *fakeGitHub) { g.jitFail, g.ensureFail = false, false })
}

// stuck describes each scale set that holds an assignment no runner took
// although a host of its profile has room for one more runner, by the
// hosts' running containers, and the profile is under its cap.
func (m *smodel) stuck() map[*sset]string {
	out := map[*sset]string{}
	cs := m.containers()
	for _, s := range m.sets {
		if len(s.assigned) == 0 {
			continue
		}
		p := m.r.p.l.byName[s.profile]
		pr := &m.r.cfg.Profiles[p]
		live := 0
		for _, rr := range cs {
			if rr.Profile == s.profile {
				live++
			}
		}
		if pr.MaxRunners > 0 && live >= pr.MaxRunners {
			continue
		}
		for _, hi := range m.r.p.l.profiles[p].hosts {
			free := m.r.p.l.hosts[hi].budget
			for name, rr := range cs {
				if _, on := m.r.hosts[hi].runs[name]; on {
					free = free.minus(m.r.p.limits(hi, &rr.Limits))
				}
			}
			if m.r.p.l.slot(p, hi).within(free) {
				out[s] = fmt.Sprintf("%s/%s: %d assigned with no runner, host %s free %+v", s.scope, s.profile, len(s.assigned), m.r.cfg.Hosts[hi].Name, free)
				break
			}
		}
	}
	return out
}

// drain heals the world, ends every job a runner holds, lets GitHub fail
// every lost job and the unassignable holds lapse, then runs passes in
// which each running job ends, the pool reconciles, each scale set polls
// and the runners take their jobs. From the second pass on no assignment
// may wait beside room for two passes, and within passes every job runs.
func (m *smodel) drain(passes int) {
	m.heal()
	m.reap()
	for _, name := range slices.Sorted(maps.Keys(m.jobOf)) {
		m.finish(name)
	}
	m.advance(skipMax + lostFor)
	var before map[*sset]string
	for pass := range passes {
		for _, name := range slices.Sorted(maps.Keys(m.jobOf)) {
			m.finish(name)
		}
		m.r.pass()
		m.settle()
		m.sweep()
		for _, s := range m.sets {
			m.poll(s, len(s.queue))
		}
		m.r.starts()
		m.take()
		m.advance(30 * time.Second)
		now := m.stuck()
		for s, why := range now {
			if _, again := before[s]; again && pass > 1 {
				m.r.t.Fatalf("pass %d: an assignment waits beside room for two passes: %s", pass, why)
			}
		}
		before = now
		if !slices.ContainsFunc(m.sets, func(s *sset) bool { return s.jobs() > 0 }) {
			m.unstarted()
			return
		}
	}
	var left []string
	for _, s := range m.sets {
		left = append(left, fmt.Sprintf("%s/%s queued %d assigned %d running %d", s.scope, s.profile, len(s.queue), len(s.assigned), len(s.running)))
	}
	m.r.t.Fatalf("jobs left after %d passes: %s", passes, strings.Join(left, "; "))
}

// unstarted fails the run when, once every job has run and each scale set
// has polled, the pool's snapshot still counts an assignment with no
// runner started: manade_jobs_assigned_unstarted, D9's backstop.
func (m *smodel) unstarted() {
	for _, s := range m.sets {
		m.poll(s, 0)
	}
	for _, c := range m.r.reg.Snapshot().Sets {
		if c.AssignedUnstarted != 0 {
			m.r.t.Fatalf("%s/%s counts %d assignments with no runner started once every job ran", c.Scope, c.Profile, c.AssignedUnstarted)
		}
	}
}
