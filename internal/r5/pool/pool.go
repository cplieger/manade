package pool

import (
	"cmp"
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/github"
	"github.com/cplieger/manade/internal/r5/jobspec"
	"github.com/cplieger/manade/internal/r5/obs"
)

// GitHub is the scale set side the pool drives.
type GitHub interface {
	Ensure(ctx context.Context, scope *config.Scope) ([]github.ScaleSet, error)
	Listen(ctx context.Context, ss github.ScaleSet, h github.Handler)
	JIT(ctx context.Context, ss github.ScaleSet, name string) (github.JIT, error)
	RemoveRunner(ctx context.Context, scope string, id int64) error
	Recheck(ctx context.Context) error
}

// REST is the REST reads the pool makes besides the queue's.
type REST interface {
	CheckInstallations(ctx context.Context) error
	Private(ctx context.Context, s *config.Scope) (bool, error)
	Runner(ctx context.Context, s *config.Scope, id int64) (github.Runner, bool, error)
}

// Sweep is the queued-jobs reads behind the one queue.
type Sweep interface {
	Run(ctx context.Context, hot func() bool, sink func(ended time.Time, qs []github.Queue))
}

// Docker is the daemon side the pool drives.
type Docker interface {
	EnsureNetwork(ctx context.Context, spec docker.NetworkSpec) error
	CreateContainer(ctx context.Context, name string, req *docker.CreateRequest) (string, error)
	StartContainer(ctx context.Context, id string) error
	RemoveOwned(ctx context.Context, id string) error
	ListContainers(ctx context.Context) ([]docker.Summary, error)
	InspectContainer(ctx context.Context, id string) (docker.Container, bool, error)
	Logs(ctx context.Context, id string, n int) ([]string, error)
	CopyArchive(ctx context.Context, id, path string, w io.Writer, limit int64) (bool, error)
	ReadFile(ctx context.Context, id, path string, limit int64) ([]byte, bool, error)
	Events(ctx context.Context, fn func(containerID string)) error
	InspectImage(ctx context.Context, ref string) (docker.Image, bool, error)
	PullImage(ctx context.Context, ref string) error
	RemoveOwnedImage(ctx context.Context, id string) error
}

var (
	_ GitHub         = (*github.ScaleSets)(nil)
	_ REST           = (*github.REST)(nil)
	_ Sweep          = (*github.Sweep)(nil)
	_ Docker         = (*docker.Client)(nil)
	_ github.Handler = (*Pool)(nil)
)

// proofAge is how long a visibility proof lets a scope be funded.
const proofAge = time.Minute

// Deps is what the pool drives. Diag is DIAG_DIR opened as a root, nil
// when diagnostics are not kept. Dev picks the shipped images' dev tags,
// for a binary built on the dev channel.
type Deps struct {
	GitHub  GitHub
	REST    REST
	Sweep   Sweep
	Docker  Docker
	Host    *jobspec.Host
	Metrics *obs.Metrics
	Log     *slog.Logger
	Diag    *os.Root
	Dev     bool
}

// Pool is the one queue and the runners behind every scale set. Its
// methods are safe for concurrent use.
type Pool struct {
	gh     GitHub
	rest   REST
	sweep  Sweep
	dk     Docker
	host   *jobspec.Host
	m      *obs.Metrics
	log    *slog.Logger
	cfg    *config.Config
	diag   *os.Root
	now    func() time.Time
	kick   chan struct{}
	verify chan struct{}
	died   chan struct{}
	pull   chan struct{}
	byName map[string]int
	scopes map[string]*scope
	// probes records the probe containers whose creating call is running.
	probes map[string]bool
	// strays holds, by container ID, the runners of no configured scale
	// set that may hold a job: busy ones read through a configured scope,
	// and every one whose scope left. Their reservations stay off the
	// budget until they end: an exit, a vanished container or the lifetime.
	strays map[string]*stray
	// rechecked is when the last pass over every scope began; passed is
	// when the last good reconciler pass ended.
	rechecked, passed time.Time
	sets              []*set
	// images holds each profile's checked image ID, "" while it is missing,
	// and imgOf its reference's record. old holds the shipped image IDs
	// this process superseded and has not yet removed.
	images []string
	imgOf  []*image
	imgs   []*image
	old    []string
	caps   []int
	budget res
	// settle is how long a start whose create or start call went
	// unanswered waits for the daemon, read every settleEvery.
	settle, settleEvery time.Duration
	mu                  sync.Mutex
	// passMu runs one reconciler pass at a time.
	passMu sync.Mutex
	// booting holds health at starting until boot has added every scale set.
	booting, stopping bool
}

// New builds the pool for cfg. It holds no scale set until addSet and
// funds no profile until its image is set.
func New(cfg *config.Config, d *Deps) *Pool {
	p := &Pool{
		gh: d.GitHub, rest: d.REST, sweep: d.Sweep, dk: d.Docker, host: d.Host, m: d.Metrics, log: cmp.Or(d.Log, slog.Default()), diag: d.Diag,
		cfg: cfg, now: time.Now, settle: startSettle, settleEvery: settlePoll, booting: true, kick: make(chan struct{}, 1), verify: make(chan struct{}, 1), died: make(chan struct{}, 1),
		pull: make(chan struct{}, 1), probes: map[string]bool{}, strays: map[string]*stray{}, byName: map[string]int{}, caps: make([]int, len(cfg.Profiles)),
		images: make([]string, len(cfg.Profiles)), scopes: map[string]*scope{},
		budget: res{cfg.Capacity.MilliCPUs, cfg.Capacity.Memory, unbounded, unbounded},
	}
	if cfg.Capacity.Disk > 0 {
		p.budget.disk = cfg.Capacity.Disk
	}
	if cfg.Capacity.MaxRunners > 0 {
		p.budget.runners = int64(cfg.Capacity.MaxRunners)
	}
	byRef := map[string]*image{}
	for i := range cfg.Profiles {
		pr := &cfg.Profiles[i]
		p.byName[pr.Name] = i
		p.caps[i] = pr.MaxRunners
		ref := jobspec.ImageRef(pr, d.Dev)
		img := byRef[ref]
		if img == nil {
			img = &image{ref: ref, digest: pr.Pinned(), shipped: pr.RunnerImage != ""}
			byRef[ref] = img
			p.imgs = append(p.imgs, img)
		}
		img.profiles = append(img.profiles, i)
		p.imgOf = append(p.imgOf, img)
	}
	for i := range cfg.Scopes {
		p.scopes[cfg.Scopes[i].Key] = &scope{cfg: &cfg.Scopes[i]}
	}
	return p
}

// scope is one scope's records.
type scope struct {
	cfg *config.Scope
	// stop ends the scope's listeners, which closes its sessions; nil
	// until Run starts them.
	stop context.CancelFunc
	// proof is when a read last proved the scope private; read is when its
	// last visibility read began, and failed when its reads began failing,
	// zero while they succeed. above is when a poll last reported capacity
	// above its assignments, which keeps the proof refreshed (proofs).
	proof, read, failed, above time.Time
	// jobs is the last complete read of the scope's queued jobs.
	jobs []github.QueuedJob
	// partial reports that the last read was not complete.
	partial bool
	public  bool
	// wanted reports that a poll found the proof stale while it would
	// report capacity above its assignments (Capacity).
	wanted bool
}

func (sc *scope) jobsOf(profile string) []github.QueuedJob {
	var out []github.QueuedJob
	for _, j := range sc.jobs {
		if j.Profile == profile {
			out = append(out, j)
		}
	}
	return out
}

// set is one scale set's records.
type set struct {
	// beat is the listener's last Beat; lastAssigned its last JobAssigned.
	beat, lastAssigned time.Time
	// standSince is when the first poll carrying its standing slot began,
	// zero without one; standEnded is when its last standing slot ended.
	standSince, standEnded time.Time
	// retryAt holds starts back after a failed one.
	retryAt time.Time
	// spent holds when each runner that ended with no JobCompleted naming
	// it ended (end).
	spent   map[string]time.Time
	asg     *assignments
	skipped map[jobKey]skip
	runners map[string]*runner
	scope   *scope
	misses  misses
	ss      github.ScaleSet
	flight  flight
	profile int
	// opened reports a session opened at least once since boot.
	open, opened, held bool
}

func (s *set) org() bool { return s.scope.cfg.Kind != config.ScopeRepo }

// end deletes r's record at now, which frees its reservation; p.mu is
// held. A runner no JobCompleted named still counts against the scale
// set's starts for spentFor, because GitHub's count drops only with that
// message, up to a poll after the exit.
func (s *set) end(r *runner, now time.Time) {
	delete(s.runners, r.name)
	if !r.completed {
		s.spent[r.name] = now
	}
}

// spending counts the runners that ended within spentFor of now with no
// JobCompleted, and forgets the older ones; p.mu is held.
func (s *set) spending(now time.Time) int {
	for name, at := range s.spent {
		if now.Sub(at) >= spentFor {
			delete(s.spent, name)
		}
	}
	return len(s.spent)
}

// flight is the figure the scale set's last poll began with, and what it
// funded. It stands until the next poll begins or a session opens, so the
// headroom it carried stays reserved while GitHub may still fill it.
type flight struct {
	// funded is how many waiting jobs the figure funded.
	figure, funded   int
	standing, active bool
}

// misses counts polls that left a funded slot unfilled; first is when the
// first counted one began.
type misses struct {
	first time.Time
	n     int
}

// skip is an unassignable job: out of the queue until until, and wait is
// the hold its next verdict doubles.
type skip struct {
	until time.Time
	wait  time.Duration
}

type runnerState int

const (
	starting runnerState = iota
	idle
	busy
)

// runner is one runner's record. Its reservation is held from the moment
// the record exists until it is deleted, so it is freed exactly once.
type runner struct {
	set     *set
	created time.Time
	name    string
	image   string
	// container is "" until the container is created.
	container string
	res       res
	id        int64
	state     runnerState
	// completed reports that a JobCompleted named the runner.
	completed bool
}

// addSet records a scale set Ensure found or created.
func (p *Pool) addSet(ss github.ScaleSet) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sets = append(p.sets, &set{
		asg: newAssignments(), scope: p.scopes[ss.Scope], ss: ss, profile: p.byName[ss.Profile],
		runners: map[string]*runner{}, skipped: map[jobKey]skip{}, spent: map[string]time.Time{},
	})
}

// set finds the scale set's records; Handler calls only name a scale set
// addSet recorded.
func (p *Pool) set(ss github.ScaleSet) *set {
	for _, s := range p.sets {
		if s.ss.ScaleSet == ss.ScaleSet {
			return s
		}
	}
	return nil
}

// setImage puts a checked image ID in service for a profile, "" for none.
func (p *Pool) setImage(profile int, id string) {
	p.mu.Lock()
	p.images[profile] = id
	p.mu.Unlock()
	p.poke()
}

// prove records a visibility read of sc begun at at; p.mu is held. A scope
// proven public stays public until a restart: its sessions are closed and
// its idle runners removed.
func (p *Pool) prove(sc *scope, private bool, err error, at time.Time) {
	sc.wanted, sc.read = false, at
	switch {
	case err != nil:
		if sc.failed.IsZero() {
			sc.failed = at
			p.log.Warn("Reading a scope's visibility failed, so the pool funds nothing for it until a read succeeds",
				"scope", sc.cfg.Key, "error", err)
		}
	case !private:
		sc.public = true
		p.log.Error("A scope's repository is public or its runner group admits public repositories, so the pool stops serving it",
			"scope", sc.cfg.Key)
		if sc.stop != nil {
			sc.stop()
		}
		wake(p.died)
	default:
		if !sc.failed.IsZero() {
			p.log.Info("A scope's visibility read succeeded again, so the pool serves it", "scope", sc.cfg.Key, "after", at.Sub(sc.failed).String())
		}
		sc.failed, sc.proof = time.Time{}, at
	}
	p.m.ScopeServing(sc.cfg.Key, p.serving(sc))
}

// private reports that sc's last visibility read proved it private.
func (sc *scope) private() bool { return !sc.public && sc.failed.IsZero() && !sc.proof.IsZero() }

// serving reports whether sc is served: private, and no session of it held
// by another process.
func (p *Pool) serving(sc *scope) bool {
	if !sc.private() {
		return false
	}
	for _, s := range p.sets {
		if s.scope == sc && s.held {
			return false
		}
	}
	return true
}

// wantProof asks for sc's visibility to be read again.
func (p *Pool) wantProof(sc *scope) {
	sc.wanted = true
	wake(p.verify)
}

// poke wakes the start worker.
func (p *Pool) poke() { wake(p.kick) }

// wake signals a coalescing channel of one.
func wake(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (p *Pool) slot(profile int) res {
	pr := &p.cfg.Profiles[profile]
	r := res{cpu: pr.MilliCPUs, mem: pr.Memory, runners: 1}
	if p.cfg.Capacity.Disk > 0 {
		r.disk = pr.Disk
	}
	return r
}

// view is a scale set's state as one allocation sees it.
type view struct {
	// reason is the unfundable reason, "" when it may be funded.
	reason string
	// open holds its waiting jobs, oldest first, less the unassignable.
	open    []github.QueuedJob
	held    res
	live    int
	slots   int
	covered int
	skipped int
	// fundable is reason "" and a whole last queue read.
	fundable bool
}

// view reads s at now. Its slots are the runners and the assignments that
// hold a reservation; past them, an idle runner and the headroom of the poll
// in flight cover its oldest waiting jobs.
func (p *Pool) view(s *set, now time.Time) view {
	var v view
	var runners res
	for _, r := range s.runners {
		v.live++
		runners = runners.plus(r.res)
	}
	owed := s.asg.owed()
	v.slots = v.live + s.asg.committed(v.live)
	extra := 0
	if s.flight.active {
		extra = max(0, s.flight.figure-v.slots)
	}
	v.held = runners.plus(p.slot(s.profile).times(v.slots - v.live + extra))
	v.covered = v.slots - owed + extra
	for _, j := range s.asg.waiting(s.scope.jobsOf(s.ss.Profile)) {
		if k := (jobKey{j.Name, j.RunID}); now.Before(s.skipped[k].until) {
			v.skipped++
			continue
		}
		v.open = append(v.open, j)
	}
	switch {
	case p.stopping || !s.open || s.held || !s.scope.private():
		v.reason = "not_serving"
	case p.images[s.profile] == "":
		v.reason = "image_missing"
	}
	v.fundable = v.reason == "" && !s.scope.partial
	return v
}

// allocate runs one allocation at now over every scale set, in the order
// standing slots are offered: organization and enterprise scale sets by
// when their last standing slot ended, never-held first, then repo scale
// sets, most recently assigned first.
func (p *Pool) allocate(now time.Time) (allocation, []*set, []view) {
	order := slices.Clone(p.sets)
	slices.SortStableFunc(order, func(x, y *set) int {
		if c := cmp.Compare(b2i(y.org()), b2i(x.org())); c != 0 || x.org() {
			return cmp.Or(c, x.standEnded.Compare(y.standEnded))
		}
		return y.lastAssigned.Compare(x.lastAssigned)
	})
	views := make([]view, len(order))
	cs := make([]claim, len(order))
	for i, s := range order {
		v := p.view(s, now)
		views[i] = v
		cs[i] = claim{held: v.held, slot: p.slot(s.profile), profile: s.profile, covered: v.covered, fundable: v.fundable, org: s.org()}
		for _, j := range v.open {
			cs[i].waiting = append(cs[i].waiting, j.Created)
		}
	}
	budget := p.budget
	for _, st := range p.strays {
		if st.profile < 0 {
			budget = budget.minus(st.r.res)
			continue
		}
		cs = append(cs, claim{held: st.r.res, profile: st.profile})
	}
	return allocate(budget, p.caps, cs, now, p.cfg.HeadReserveAfter), order, views
}

// stray is a held runner of no configured scale set (Pool.strays): a
// record with no set, its scope's key, and its profile's index, -1 for a
// profile the config no longer has. One of a configured profile counts
// against its max_runners.
type stray struct {
	r       *runner
	scope   string
	profile int
}

// free is the budget less what the strays hold; p.mu is held.
func (p *Pool) free() res {
	b := p.budget
	for _, st := range p.strays {
		b = b.minus(st.r.res)
	}
	return b
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// hot reports whether the budget left after every holding fits one more
// runner of some profile: the sweep then reads every started run's jobs.
func (p *Pool) hot() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	free := p.free()
	now := p.now()
	for _, s := range p.sets {
		free = free.minus(p.view(s, now).held)
	}
	for i := range p.cfg.Profiles {
		if p.slot(i).within(free) {
			return true
		}
	}
	return false
}

// Snapshot is the pool's state for obs, its Source.
func (p *Pool) Snapshot() obs.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	a, order, views := p.allocate(now)
	var snap obs.Snapshot
	if a.head >= 0 {
		snap.HeadSince = a.headSince
	}
	held := p.budget.minus(p.free())
	for i, s := range order {
		v := &views[i]
		c := obs.Counts{ScaleSet: s.ss.ScaleSet, Unassignable: v.skipped, AssignedUnstarted: max(0, s.asg.stats.Assigned-v.live)}
		for _, r := range s.runners {
			held = held.plus(r.res)
			switch r.state {
			case starting:
				c.Starting++
			case idle:
				c.Idle++
			case busy:
				c.Busy++
			}
		}
		switch v.reason {
		case "":
			c.Queued = len(v.open)
		case "not_serving":
			c.NotServing = len(v.open)
		default:
			c.ImageMissing = len(v.open)
		}
		snap.Counts = append(snap.Counts, c)
	}
	snap.Counts = append(snap.Counts, p.unserved()...)
	snap.Reserved.MilliCPUs, snap.Reserved.Memory, snap.Reserved.Disk = held.cpu, held.mem, held.disk
	return snap
}

// unserved counts the queued jobs of profiles with no scale set in their
// scope: not allowed there, or allowed and not yet ensured.
func (p *Pool) unserved() []obs.Counts {
	var out []obs.Counts
	for i := range p.cfg.Scopes {
		sc := p.scopes[p.cfg.Scopes[i].Key]
		for j := range p.cfg.Profiles {
			pr := &p.cfg.Profiles[j]
			ss := obs.ScaleSet{Scope: sc.cfg.Key, Profile: pr.Name}
			n := len(sc.jobsOf(pr.Name))
			if n == 0 || slices.ContainsFunc(p.sets, func(s *set) bool { return s.ss.ScaleSet == ss }) {
				continue
			}
			c := obs.Counts{ScaleSet: ss, NotServing: n}
			if !pr.Allowed(sc.cfg.Key) {
				c.NotServing, c.NotAllowed = 0, n
			}
			out = append(out, c)
		}
	}
	return out
}

// jobResult maps a JobCompleted's result to obs's vocabulary.
func jobResult(r string) obs.JobResult {
	switch strings.ToLower(r) {
	case "succeeded":
		return obs.JobSucceeded
	case "failed":
		return obs.JobFailed
	case "canceled":
		return obs.JobCanceled
	}
	return obs.JobOther
}
