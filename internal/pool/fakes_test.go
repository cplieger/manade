package pool

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// poolConfig is testConfig with one repository scope per name, the pool's
// name, and each profile's lifetimes.
func poolConfig(hosts []hostSpec, profiles []profileSpec, scopes ...string) *config.Config {
	cfg := testConfig(hosts, profiles)
	cfg.PoolName, cfg.RunnerVariable = "manade", "MANADE_RUNNER"
	for i := range cfg.Profiles {
		cfg.Profiles[i].MaxLifetime, cfg.Profiles[i].IdleTimeout = 2*time.Hour, idleFor
	}
	for _, key := range scopes {
		owner, repo, _ := strings.Cut(key, "/")
		cfg.Scopes = append(cfg.Scopes, config.Scope{Key: key, Owner: owner, Repo: repo, Kind: config.ScopeRepo, DefaultProfile: defaultOf(cfg, key)})
	}
	return cfg
}

// defaultOf is the first profile by name the scope with key may use, as
// the parser asks of a served scope.
func defaultOf(cfg *config.Config, key string) string {
	for i := range cfg.Profiles {
		if cfg.Profiles[i].Usable(key) {
			return cfg.Profiles[i].Name
		}
	}
	return ""
}

// cloneConfig copies cfg deep enough that editing the copy's hosts,
// profiles and scopes leaves cfg as it was.
func cloneConfig(cfg *config.Config) *config.Config {
	c := *cfg
	c.Hosts = slices.Clone(cfg.Hosts)
	c.Scopes = slices.Clone(cfg.Scopes)
	c.Profiles = slices.Clone(cfg.Profiles)
	for i := range c.Profiles {
		c.Profiles[i].Hosts = slices.Clone(cfg.Profiles[i].Hosts)
	}
	return &c
}

var errLink = errors.New("fake: the link is down")

// fakeHost is a host's API over an in-memory record of its runners, each
// entry standing for its record and its container alike.
type fakeHost struct {
	// notUp, set, is a host waiting for its first section or booting it:
	// its status says so and every other call answers notUp.
	notUp        error
	hold         chan struct{}
	runs         map[string]*hostapi.Runner
	probe        func(ctx context.Context) (hostapi.ProbeReport, error)
	notReady     map[string]bool
	reasons      map[string]hostapi.Reason
	cfg          *config.Config
	held         chan struct{}
	name         string
	section      string
	state        hostapi.SectionState
	deleted      []string
	probes       int
	streams      int
	timeouts     int
	posts        int
	inst         int
	lost         int
	mu           sync.Mutex
	down         bool
	disconnected bool
	refuse       bool
	failStart    bool
}

func newFakeHost(cfg *config.Config, name, section string) *fakeHost {
	return &fakeHost{cfg: cfg, name: name, section: section, runs: map[string]*hostapi.Runner{}, notReady: map[string]bool{}, reasons: map[string]hostapi.Reason{}}
}

func (f *fakeHost) instance() string { return fmt.Sprintf("%s-%d", f.name, f.inst) }

func (f *fakeHost) Status(context.Context) (hostapi.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return hostapi.Status{}, errLink
	}
	if f.notUp != nil {
		return hostapi.Status{InstanceID: f.instance(), Section: hostapi.SectionStatus{State: cmp.Or(f.state, hostapi.SectionWaiting)}}, nil
	}
	h, _ := f.cfg.Host(f.name)
	st := hostapi.Status{
		InstanceID: f.instance(), PolicyHash: "policy",
		Section:       hostapi.SectionStatus{State: cmp.Or(f.state, hostapi.SectionRunning), Hash: f.section},
		Limits:        hostapi.HostLimits{CPUMode: hostapi.CPUQuota, MilliCPUs: max(h.Capacity.MilliCPUs, 64000), Memory: 1 << 50},
		Memory:        hostapi.MemoryStatus{Limit: 1 << 50, Admissible: 1 << 49, Reserve: 1 << 28},
		Data:          hostapi.DataStatus{Size: 1 << 50, Free: 1 << 49},
		ProbeDeadline: time.Minute,
	}
	for i := range f.cfg.Profiles {
		if pr := &f.cfg.Profiles[i]; slices.Contains(pr.Hosts, f.name) {
			st.Profiles = append(st.Profiles, hostapi.ProfileStatus{Name: pr.Name, Ready: !f.notReady[pr.Name], Reason: f.reasons[pr.Name]})
		}
	}
	return st, nil
}

func (f *fakeHost) CreateRunner(_ context.Context, req *hostapi.RunnerRequest) (hostapi.RunnerAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	if f.down {
		return hostapi.RunnerAnswer{}, errLink
	}
	if f.notUp != nil {
		return hostapi.RunnerAnswer{}, f.notUp
	}
	if r := f.runs[req.RunnerName]; r != nil {
		return hostapi.RunnerAnswer{InstanceID: f.instance(), Runner: *r}, nil
	}
	pr, ok := f.cfg.Profile(req.Profile)
	if !ok || !slices.Contains(pr.Hosts, f.name) {
		return hostapi.RunnerAnswer{}, hostapi.ErrUnknownProfile
	}
	if f.refuse || f.notReady[req.Profile] {
		return hostapi.RunnerAnswer{}, &hostapi.NotReadyError{Profile: req.Profile, Reason: hostapi.ReasonImageMissing}
	}
	r := &hostapi.Runner{
		Name: req.RunnerName, Profile: req.Profile, Scope: req.Scope, RunnerID: req.RunnerID, State: hostapi.StateRunning,
		Limits: hostapi.Limits{MilliCPUs: pr.MilliCPUs, Memory: pr.Memory, Disk: pr.Disk},
	}
	if f.failStart {
		r.State, r.Reason = hostapi.StateFailed, "the daemon refused the create"
	}
	f.runs[r.Name] = r
	if f.timeouts > 0 {
		f.timeouts--
		return hostapi.RunnerAnswer{}, context.DeadlineExceeded
	}
	return hostapi.RunnerAnswer{InstanceID: f.instance(), Runner: *r}, nil
}

func (f *fakeHost) Runners(context.Context) (hostapi.RunnerList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return hostapi.RunnerList{}, errLink
	}
	if f.notUp != nil {
		return hostapi.RunnerList{}, f.notUp
	}
	l := hostapi.RunnerList{InstanceID: f.instance()}
	for _, name := range slices.Sorted(maps.Keys(f.runs)) {
		l.Runners = append(l.Runners, *f.runs[name])
	}
	return l, nil
}

func (f *fakeHost) DeleteRunner(_ context.Context, name string, diag bool) ([]byte, error) {
	f.mu.Lock()
	hold, held := f.hold, f.held
	f.mu.Unlock()
	if hold != nil {
		held <- struct{}{}
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errLink
	}
	delete(f.runs, name)
	f.deleted = append(f.deleted, name)
	if f.lost > 0 {
		f.lost--
		return nil, errLink
	}
	if diag {
		return []byte("diag of " + name), nil
	}
	return nil, nil
}

func (*fakeHost) Logs(_ context.Context, name string, tail int) ([]byte, error) {
	return fmt.Appendf(nil, "the last %d lines of %s", tail, name), nil
}

func (f *fakeHost) Probe(ctx context.Context, _ time.Duration) (hostapi.ProbeReport, error) {
	f.mu.Lock()
	f.probes++
	probe, down, notUp := f.probe, f.down, f.notUp
	f.mu.Unlock()
	switch {
	case down:
		return hostapi.ProbeReport{}, errLink
	case notUp != nil:
		return hostapi.ProbeReport{}, notUp
	case probe != nil:
		return probe(ctx)
	}
	return hostapi.ProbeReport{Proven: true}, nil
}

func (f *fakeHost) Events(ctx context.Context, _ func(*hostapi.Event)) error {
	f.mu.Lock()
	f.streams++
	notUp := f.notUp
	f.mu.Unlock()
	if notUp != nil {
		return notUp
	}
	<-ctx.Done()
	return ctx.Err()
}

// exitUsage is what every runner the fake host ends reports it used.
var exitUsage = hostapi.Usage{CPU: 90 * time.Second, Ran: 5 * time.Minute, PeakMemory: 300 << 20}

// exit ends runner name's container with code, reporting exitUsage.
func (f *fakeHost) exit(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.runs[name]; r != nil && r.State == hostapi.StateRunning {
		u := exitUsage
		r.State, r.ExitCode, r.Usage = hostapi.StateExited, &code, &u
	}
}

// restart is a new supervisor: every running container ends with code,
// and the records of runners with no container are gone.
func (f *fakeHost) restart(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inst++
	for name, r := range f.runs {
		switch r.State {
		case hostapi.StateRunning:
			c := code
			r.State, r.ExitCode = hostapi.StateExited, &c
		case hostapi.StateFailed, hostapi.StateCreating, hostapi.StateStarting:
			delete(f.runs, name)
		}
	}
}

func (f *fakeHost) running() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name, r := range f.runs {
		if r.State == hostapi.StateRunning {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeHost) set(fn func(*fakeHost)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeLinks is the host listener's registry over the fake hosts: a host
// is connected unless it is marked disconnected.
type fakeLinks struct{ hosts []*fakeHost }

func (l *fakeLinks) Host(name string) (HostAPI, bool) {
	for _, h := range l.hosts {
		if h.name != name {
			continue
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return h, !h.disconnected
	}
	return nil, false
}

// fakeGitHub is GitHub's scale sets and REST reads: registrations by ID,
// and the ones that hold a job, which GitHub refuses to remove.
type fakeGitHub struct {
	cfg     *config.Config
	regs    map[int64]bool
	busy    map[int64]bool
	public  map[string]bool
	removed []int64
	// vars holds each scope's variables by scope and name without case;
	// varOps, every variable write in order, as "set scope name value" or
	// "delete scope name".
	vars   map[string]string
	varOps []string
	// gone holds the scale sets GitHub no longer has, by goneKey;
	// existsErr, the scopes whose scale-set reads fail. asked and
	// askedSets are every such read's scope and scale set.
	gone      map[string]bool
	existsErr map[string]bool
	asked     []config.Scope
	askedSets []github.ScaleSet
	next      int64
	// idBase is added to every ID Ensure answers, and ids replaces the ID
	// of a scale set GitHub created again.
	idBase int
	ids    map[setKey]int
	// varErr answers every variable call when set.
	varErr error
	// ensureHold, when set, runs at the start of each ensure, outside mu;
	// with ensureBlock, each ensure answers only once its context ends.
	ensureHold  func()
	ensureBlock bool
	mu          sync.Mutex
	// jitFail refuses each JIT config; ensureFail, each ensure.
	jitFail, ensureFail bool
}

func newFakeGitHub(cfg *config.Config) *fakeGitHub {
	return &fakeGitHub{
		cfg: cfg, regs: map[int64]bool{}, busy: map[int64]bool{}, public: map[string]bool{}, vars: map[string]string{},
		gone: map[string]bool{}, existsErr: map[string]bool{}, ids: map[setKey]int{},
	}
}

func varKey(scope, name string) string { return strings.ToLower(scope + "\x00" + name) }

func (g *fakeGitHub) Variable(_ context.Context, s *config.Scope, name string) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.varErr != nil {
		return "", false, g.varErr
	}
	v, ok := g.vars[varKey(s.Key, name)]
	return v, ok, nil
}

func (g *fakeGitHub) SetVariable(_ context.Context, s *config.Scope, name, value string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.varErr != nil {
		return g.varErr
	}
	g.vars[varKey(s.Key, name)] = value
	g.varOps = append(g.varOps, "set "+s.Key+" "+name+" "+value)
	return nil
}

func (g *fakeGitHub) DeleteVariable(_ context.Context, s *config.Scope, name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.varErr != nil {
		return g.varErr
	}
	delete(g.vars, varKey(s.Key, name))
	g.varOps = append(g.varOps, "delete "+s.Key+" "+name)
	return nil
}

// variable answers scope's variable name as GitHub holds it.
func (g *fakeGitHub) variable(scope, name string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.vars[varKey(scope, name)]
	return v, ok
}

// takeOps answers the variable writes since the last call.
func (g *fakeGitHub) takeOps() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.varOps
	g.varOps = nil
	return out
}

func (g *fakeGitHub) Ensure(ctx context.Context, _ *config.Config, sc *config.Scope) ([]github.ScaleSet, error) {
	g.mu.Lock()
	hold, block := g.ensureHold, g.ensureBlock
	g.mu.Unlock()
	if hold != nil {
		hold()
	}
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ensureFail {
		return nil, errors.New("fake: GitHub is unreachable")
	}
	var out []github.ScaleSet
	for i := range g.cfg.Profiles {
		if pr := &g.cfg.Profiles[i]; pr.Serves(sc.Key) {
			id := g.idBase + i + 1
			if v, ok := g.ids[setKey{sc.Key, pr.Name}]; ok {
				id = v
			}
			out = append(out, github.ScaleSet{Scope: sc.Key, Profile: pr.Name, ID: id})
		}
	}
	return out, nil
}

func (*fakeGitHub) Listen(ctx context.Context, _ *config.Scope, _ github.ScaleSet, _ string, _ github.Handler) {
	<-ctx.Done()
}

func (g *fakeGitHub) JIT(context.Context, *config.Scope, github.ScaleSet, string) (github.JIT, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.jitFail {
		return github.JIT{}, errors.New("fake: JIT refused")
	}
	g.next++
	g.regs[g.next] = true
	return github.JIT{Config: "jit", RunnerID: g.next}, nil
}

func (g *fakeGitHub) RemoveRunner(_ context.Context, _ *config.Scope, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy[id] {
		return github.ErrRunnerBusy
	}
	delete(g.regs, id)
	g.removed = append(g.removed, id)
	return nil
}

func (*fakeGitHub) Recheck(context.Context) error { return nil }

// goneKey names a scale set in fakeGitHub.gone.
func goneKey(scope string, id int) string { return strings.ToLower(scope) + "\x00" + strconv.Itoa(id) }

func (g *fakeGitHub) Exists(_ context.Context, s *config.Scope, ss github.ScaleSet) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked, g.askedSets = append(g.asked, *s), append(g.askedSets, ss)
	if g.existsErr[s.Key] {
		return false, errors.New("fake: GitHub refused the read")
	}
	return !g.gone[goneKey(s.Key, ss.ID)], nil
}

func (g *fakeGitHub) Private(_ context.Context, s *config.Scope) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.public[s.Key], nil
}

func (g *fakeGitHub) Runner(_ context.Context, _ *config.Scope, id int64) (github.Runner, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return github.Runner{Busy: g.busy[id]}, g.regs[id], nil
}

func (*fakeGitHub) Run(ctx context.Context, _ *config.Config, _ func() bool, _ func(time.Time, []github.Queue)) {
	<-ctx.Done()
}

func (g *fakeGitHub) set(fn func(*fakeGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn(g)
}

func (g *fakeGitHub) removals(id int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.removed {
		if r == id {
			n++
		}
	}
	return n
}

func (g *fakeGitHub) wasRemoved(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Contains(g.removed, id)
}

// fakeMetrics counts what the pool records.
type fakeMetrics struct {
	linkErrors map[string]int
	starts     map[string]int
	exits      map[ExitReason]int
	// events holds each RunnerExited and Generation call in order;
	// generations, each Generation's revision and settings.
	events      []string
	generations []generationCall
	mu          sync.Mutex
}

type generationCall struct {
	cfg *config.Config
	rev uint64
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{linkErrors: map[string]int{}, starts: map[string]int{}, exits: map[ExitReason]int{}}
}

func (m *fakeMetrics) RunnerStarted(_ string, _ github.ScaleSet, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.starts[fmt.Sprint(ok)]++
}

func (m *fakeMetrics) RunnerExited(_ string, ss github.ScaleSet, reason ExitReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exits[reason]++
	m.events = append(m.events, "exit "+ss.Profile+" "+string(reason))
}

func (*fakeMetrics) JobAssigned(string, time.Duration)            {}
func (*fakeMetrics) JobFinished(string, JobResult, time.Duration) {}
func (m *fakeMetrics) HostLinkError(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.linkErrors[host]++
}

func (m *fakeMetrics) linkErrorsOf(host string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.linkErrors[host]
}

func (m *fakeMetrics) Generation(cfg *config.Config, rev uint64, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generations = append(m.generations, generationCall{cfg: cfg, rev: rev})
	m.events = append(m.events, fmt.Sprint("generation ", rev))
}

// fakeSettler keeps each Settle call; the test decides what the settings
// owner commits.
type fakeSettler struct {
	calls []settleCall
	mu    sync.Mutex
}

type settleCall struct {
	ended []Stop
	rev   uint64
}

func (s *fakeSettler) Settle(rev uint64, ended []Stop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, settleCall{rev: rev, ended: ended})
}

// take answers the calls since the last take.
func (s *fakeSettler) take() []settleCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.calls
	s.calls = nil
	return out
}

// fakeRecords is the store's scale-set and route records, which outlive a
// restart.
type fakeRecords struct {
	sets   map[setKey]github.ScaleSet
	routes map[string]Route
	// drops is every record dropped, in order.
	drops []github.ScaleSet
	// fail refuses every read and write.
	fail bool
	mu   sync.Mutex
}

func (f *fakeRecords) ScaleSets(context.Context) ([]github.ScaleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("fake: the store refused the read")
	}
	return slices.Collect(maps.Values(f.sets)), nil
}

func (f *fakeRecords) RecordScaleSets(_ context.Context, sets []github.ScaleSet, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("fake: the store refused the write")
	}
	for _, ss := range sets {
		f.sets[keyOf(ss)] = ss
	}
	return nil
}

func (f *fakeRecords) DropScaleSet(_ context.Context, ss github.ScaleSet, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("fake: the store refused the write")
	}
	if cur, ok := f.sets[keyOf(ss)]; ok && cur.ID == ss.ID {
		delete(f.sets, keyOf(ss))
		f.drops = append(f.drops, ss)
	}
	return nil
}

func newRecords() *fakeRecords {
	return &fakeRecords{sets: map[setKey]github.ScaleSet{}, routes: map[string]Route{}}
}

func (f *fakeRecords) Routes(context.Context) ([]Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("fake: the store refused the read")
	}
	return slices.Collect(maps.Values(f.routes)), nil
}

func (f *fakeRecords) RecordRoute(_ context.Context, rt Route, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("fake: the store refused the write")
	}
	f.routes[strings.ToLower(rt.Scope)] = rt
	return nil
}

// fakeRuns keeps what the pool hands the run history.
type fakeRuns struct {
	listed map[string][][]string
	jobs   []runJob
	exits  []RunnerExit
	mu     sync.Mutex
}

// runJob is one job message the run history took.
type runJob struct {
	e      github.Event
	ss     github.ScaleSet
	host   string
	waited JobReason
}

func newFakeRuns() *fakeRuns { return &fakeRuns{listed: map[string][][]string{}} }

func (f *fakeRuns) Job(ss github.ScaleSet, host string, e *github.Event, waited JobReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, runJob{ss: ss, host: host, e: *e, waited: waited})
}

func (f *fakeRuns) Exited(_ context.Context, x *RunnerExit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exits = append(f.exits, *x)
}

func (f *fakeRuns) Listed(_ context.Context, host string, runners []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed[host] = append(f.listed[host], slices.Sorted(slices.Values(runners)))
}

// exitOf answers the exits handed over for runner name.
func (f *fakeRuns) exitOf(name string) []RunnerExit {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RunnerExit
	for _, x := range f.exits {
		if x.Runner == name {
			out = append(out, x)
		}
	}
	return out
}

// fakeHostLog keeps what the pool hands the host history.
type fakeHostLog struct {
	states   []noted
	proofs   []proved
	statuses map[string]int
	mu       sync.Mutex
}

// noted is one state handed over.
type noted struct {
	at                  time.Time
	host, state, detail string
}

// proved is one proof handed over.
type proved struct {
	rep           *hostapi.ProbeReport
	host, failure string
}

func newFakeHostLog() *fakeHostLog { return &fakeHostLog{statuses: map[string]int{}} }

func (f *fakeHostLog) State(host string, at time.Time, state HostState, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, noted{at: at, host: host, state: string(state), detail: detail})
}

func (f *fakeHostLog) Status(host string, _ time.Time, _ *hostapi.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[host]++
}

func (f *fakeHostLog) Proved(host string, _ time.Time, rep *hostapi.ProbeReport, failure string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proofs = append(f.proofs, proved{host: host, rep: rep, failure: failure})
}

// statesOf answers the states handed over for host, each as state or
// state/detail.
func (f *fakeHostLog) statesOf(host string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, n := range f.states {
		if n.host == host {
			out = append(out, strings.TrimSuffix(n.state+"/"+n.detail, "/"))
		}
	}
	return out
}

// rig is a pool over fake hosts and a fake GitHub, on a clock the test
// moves, with a log the test can read. Its generations are installed in
// line and run no loops, so each step runs where the test calls it.
type rig struct {
	t   testing.TB
	now time.Time
	cfg *config.Config
	reg *Registry
	p   *generation
	gh  *fakeGitHub
	// side is the credential's GitHub side every revision names.
	side    *GitHub
	m       *fakeMetrics
	log     *logBuf
	links   *fakeLinks
	settler *fakeSettler
	records *fakeRecords
	// runs, hostLog and sessions outlive a restart, as the store and
	// GitHub do.
	sessions *fakeSessions
	runs     *fakeRuns
	hostLog  *fakeHostLog
	hosts    []*fakeHost
	rev      uint64
}

func newRig(t testing.TB, cfg *config.Config) *rig { return buildRig(t, cfg, &logBuf{}) }

// quietRig is newRig with its log discarded.
func quietRig(t testing.TB, cfg *config.Config) *rig { return buildRig(t, cfg, &logBuf{discard: true}) }

func buildRig(t testing.TB, cfg *config.Config, log *logBuf) *rig {
	gh := newFakeGitHub(cfg)
	r := &rig{
		t: t, now: epoch, cfg: cfg, gh: gh, side: sideOf(gh), log: log, rev: 1, settler: &fakeSettler{}, records: newRecords(),
		sessions: &fakeSessions{t: t}, runs: newFakeRuns(), hostLog: newFakeHostLog(),
	}
	for i := range cfg.Hosts {
		r.hosts = append(r.hosts, newFakeHost(cfg, cfg.Hosts[i].Name, sectionHash(r.rev)))
	}
	r.links = &fakeLinks{hosts: r.hosts}
	r.restart()
	return r
}

// sideOf is g as one credential's GitHub side.
func sideOf(g *fakeGitHub) *GitHub { return &GitHub{ScaleSets: g, REST: g, Sweep: g} }

// sectionHash is the hash of the section revision rev delivers.
func sectionHash(rev uint64) string { return fmt.Sprintf("section-%d", rev) }

// settings is the rig's current revision as the pool reads it.
func (r *rig) settings() *Settings {
	s := &Settings{Config: r.cfg, GitHub: r.side, Revision: r.rev, Sections: map[string]string{}}
	for i := range r.cfg.Hosts {
		s.Sections[r.cfg.Hosts[i].Name] = sectionHash(r.rev)
	}
	return s
}

// restart is a new control-plane process over the same hosts and GitHub:
// the old process's sessions end with it, and a new registry boots its
// first generation, reads every scope's visibility, proves every host and
// opens its sessions.
func (r *rig) restart() {
	r.sessions.endAll()
	r.m = newFakeMetrics()
	r.reg = NewRegistry(r.settings(), &Deps{
		Links: r.links, Metrics: r.m, Runs: r.runs, Hosts: r.hostLog, Settle: r.settler, Records: r.records, Routes: r.records,
		Log: slog.New(slog.NewTextHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	r.reg.now = func() time.Time { return r.now }
	r.reg.spawn = r.sessions.spawn
	r.reg.record(context.Background(), nil)
	r.install(r.reg.take())
}

// apply commits cfg as the next revision: every host is delivered its new
// section, unless lagging names it, and the registry installs the
// generation that serves it.
func (r *rig) apply(cfg *config.Config, lagging ...string) {
	r.cfg, r.rev = cfg, r.rev+1
	r.gh.set(func(g *fakeGitHub) { g.cfg = cfg })
	for _, h := range r.hosts {
		h.set(func(f *fakeHost) {
			f.cfg = cfg
			if !slices.Contains(lagging, f.name) {
				f.section = sectionHash(r.rev)
			}
		})
	}
	r.install(r.settings())
}

// install installs s as Run does, then opens the sessions it serves.
func (r *rig) install(s *Settings) {
	r.p = r.reg.install(context.Background(), s)
	r.p.visibility(context.Background())
	r.prove()
	r.reg.sync()
}

// prove runs every host's pending proof.
func (r *rig) prove() {
	for i, h := range r.p.hosts {
		select {
		case <-h.want:
			r.p.proveHost(context.Background(), i)
		default:
		}
	}
}

// pass runs the ensures due, the visibility reads due, each host's pass
// in turn, the pool-wide step, the proofs the passes asked for, and the starts.
func (r *rig) pass() {
	if r.p.retryEnsure(context.Background()) {
		r.reg.sync()
	}
	r.p.visibility(context.Background())
	for i := range r.p.hosts {
		r.p.passHost(context.Background(), i)
	}
	r.p.step(context.Background())
	r.prove()
	r.starts()
}

// starts runs the start worker's starts in line.
func (r *rig) starts() {
	for _, j := range r.p.owed() {
		r.p.start(context.Background(), j)
	}
}

func (r *rig) book(scope, profile string) *book { return r.p.bookOf(scope, profile) }

// view answers host's view in the current snapshot.
func (r *rig) view(host string) HostView {
	r.t.Helper()
	for _, h := range r.reg.Snapshot().Hosts {
		if h.Host == host {
			return h
		}
	}
	r.t.Fatalf("the snapshot has no host %s", host)
	return HostView{}
}

// serving reports host serving in the current snapshot.
func (r *rig) serving(host string) bool { return r.view(host).Serving }

// poll runs one poll of b: its capacity, then an answer with events and
// stats.
func (r *rig) poll(b *book, st github.Stats, events ...github.Event) {
	r.reg.Capacity(b.ss, r.now)
	r.reg.Polled(b.ss, &github.Poll{Started: r.now, Ended: r.now, Stats: &st, Events: events, Idle: len(events) == 0})
}

// logBuf is a log sink safe for concurrent writers; a discarding one
// keeps nothing.
type logBuf struct {
	b       strings.Builder
	mu      sync.Mutex
	discard bool
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.discard {
		return len(p), nil
	}
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

var _ io.Writer = (*logBuf)(nil)

// fakeSessions is GitHub's message sessions as the rig's registry opens
// them. GitHub holds one session per scale set: a create while another
// holds it is refused until that one ends, and a close ends a moment
// after it is asked for.
type fakeSessions struct {
	t    testing.TB
	live []*fakeSession
	mu   sync.Mutex
}

// fakeSession is one session the registry opened; opened is whether the
// pool has its statistics yet, which only the model reads and writes.
type fakeSession struct {
	w       want
	done    chan struct{}
	end     sync.Once
	closing bool
	ended   bool
	opened  bool
}

// closeDelay is how long a session's close takes to land at GitHub.
const closeDelay = 100 * time.Microsecond

// spawn opens w's session as the registry's spawn does. A session under
// another credential still holding the scale set fails the test: the
// registry closes every session, and waits for each, before one opens
// under a new credential.
func (f *fakeSessions) spawn(w want) *session {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.live {
		if !o.ended && o.w.gh != w.gh && o.w.ss.Scope == w.ss.Scope && o.w.ss.ID == w.ss.ID {
			f.t.Fatalf("%v opened under a new credential while the old credential's session still held it", w.ss)
		}
	}
	fs := &fakeSession{w: w, done: make(chan struct{})}
	f.live = append(f.live, fs)
	return &session{want: w, done: fs.done, cancel: func() {
		f.mu.Lock()
		fs.closing = true
		f.mu.Unlock()
		go func() {
			time.Sleep(closeDelay)
			f.finish(fs)
		}()
	}}
}

// finish ends fs: GitHub no longer holds its claim.
func (f *fakeSessions) finish(fs *fakeSession) {
	fs.end.Do(func() {
		f.mu.Lock()
		fs.ended = true
		f.live = slices.DeleteFunc(f.live, func(o *fakeSession) bool { return o == fs })
		f.mu.Unlock()
		close(fs.done)
	})
}

// endAll ends every session, as a process that stops does.
func (f *fakeSessions) endAll() {
	f.mu.Lock()
	live := slices.Clone(f.live)
	f.mu.Unlock()
	for _, fs := range live {
		f.finish(fs)
	}
}

// usable answers the session the registry holds open for ss, nil for
// none, once GitHub lets it poll: no other session still holds the scale
// set. A closing holder's close is waited out, as a create retried past it
// succeeds.
func (f *fakeSessions) usable(ss github.ScaleSet) *fakeSession {
	f.mu.Lock()
	var mine *fakeSession
	for _, o := range f.live {
		if !o.ended && !o.closing && o.w.ss == ss {
			mine = o
		}
	}
	var closing []*fakeSession
	for _, o := range f.live {
		if o == mine || o.ended || o.w.ss.Scope != ss.Scope || o.w.ss.ID != ss.ID {
			continue
		}
		if !o.closing {
			mine = nil
		}
		closing = append(closing, o)
	}
	f.mu.Unlock()
	if mine == nil {
		return nil
	}
	for _, o := range closing {
		<-o.done
	}
	return mine
}
