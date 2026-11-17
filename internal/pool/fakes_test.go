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
	cfg.PoolName = "manade"
	for i := range cfg.Profiles {
		cfg.Profiles[i].MaxLifetime, cfg.Profiles[i].IdleTimeout = 2*time.Hour, idleFor
	}
	for _, key := range scopes {
		owner, repo, _ := strings.Cut(key, "/")
		cfg.Scopes = append(cfg.Scopes, config.Scope{Key: key, Owner: owner, Repo: repo, Kind: config.ScopeRepo})
	}
	return cfg
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
	l := hostapi.RunnerList{InstanceID: f.instance()}
	for _, name := range slices.Sorted(maps.Keys(f.runs)) {
		l.Runners = append(l.Runners, *f.runs[name])
	}
	return l, nil
}

func (f *fakeHost) DeleteRunner(_ context.Context, name string, _ bool) ([]byte, error) {
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
	return nil, nil
}

func (*fakeHost) Logs(context.Context, string, int) ([]byte, error) { return []byte("the tail"), nil }

func (f *fakeHost) Probe(ctx context.Context, _ time.Duration) (hostapi.ProbeReport, error) {
	f.mu.Lock()
	f.probes++
	probe, down := f.probe, f.down
	f.mu.Unlock()
	switch {
	case down:
		return hostapi.ProbeReport{}, errLink
	case probe != nil:
		return probe(ctx)
	}
	return hostapi.ProbeReport{Proven: true}, nil
}

func (*fakeHost) Events(ctx context.Context, _ func(*hostapi.Event)) error {
	<-ctx.Done()
	return ctx.Err()
}

// exit ends runner name's container with code.
func (f *fakeHost) exit(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.runs[name]; r != nil && r.State == hostapi.StateRunning {
		r.State, r.ExitCode = hostapi.StateExited, &code
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
	next    int64
	mu      sync.Mutex
	// jitFail refuses each JIT config; ensureFail, each ensure.
	jitFail, ensureFail bool
}

func newFakeGitHub(cfg *config.Config) *fakeGitHub {
	return &fakeGitHub{cfg: cfg, regs: map[int64]bool{}, busy: map[int64]bool{}, public: map[string]bool{}}
}

func (g *fakeGitHub) Ensure(_ context.Context, _ *config.Config, sc *config.Scope) ([]github.ScaleSet, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ensureFail {
		return nil, errors.New("fake: GitHub is unreachable")
	}
	var out []github.ScaleSet
	for i := range g.cfg.Profiles {
		if pr := &g.cfg.Profiles[i]; pr.Serves(sc.Key) {
			out = append(out, github.ScaleSet{Scope: sc.Key, Profile: pr.Name, ID: i + 1})
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
	starts  map[string]int
	exits   map[ExitReason]int
	serving map[string]bool
	proofs  map[string]int
	// passes holds each Pass call's value; generations, each Generation's.
	passes      []bool
	generations []uint64
	mu          sync.Mutex
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{starts: map[string]int{}, exits: map[ExitReason]int{}, serving: map[string]bool{}, proofs: map[string]int{}}
}

func (*fakeMetrics) ScopeServing(string, bool) {}

func (m *fakeMetrics) RunnerStarted(_ string, _ github.ScaleSet, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.starts[fmt.Sprint(ok)]++
}

func (m *fakeMetrics) RunnerExited(_ string, _ github.ScaleSet, reason ExitReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exits[reason]++
}

func (*fakeMetrics) JobAssigned(string, time.Duration)            {}
func (*fakeMetrics) JobFinished(string, JobResult, time.Duration) {}
func (*fakeMetrics) HostStatus(string, *hostapi.Status)           {}

func (m *fakeMetrics) HostServing(host string, serving bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serving[host] = serving
}

func (m *fakeMetrics) HostProved(host string, _ time.Time, _ *hostapi.ProbeReport, _ bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proofs[host]++
}

func (*fakeMetrics) HostLinkError(string) {}

func (m *fakeMetrics) Pass(reconciling bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passes = append(m.passes, reconciling)
}

func (m *fakeMetrics) Generation(rev uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generations = append(m.generations, rev)
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

// fakeRecords is the store's scale-set record, which outlives a restart.
type fakeRecords struct {
	sets map[setKey]github.ScaleSet
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
	hosts   []*fakeHost
	rev     uint64
}

func newRig(t testing.TB, cfg *config.Config) *rig { return buildRig(t, cfg, &logBuf{}) }

// quietRig is newRig with its log discarded.
func quietRig(t testing.TB, cfg *config.Config) *rig { return buildRig(t, cfg, &logBuf{discard: true}) }

func buildRig(t testing.TB, cfg *config.Config, log *logBuf) *rig {
	gh := newFakeGitHub(cfg)
	r := &rig{t: t, now: epoch, cfg: cfg, gh: gh, side: sideOf(gh), log: log, rev: 1, settler: &fakeSettler{}, records: &fakeRecords{sets: map[setKey]github.ScaleSet{}}}
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
// a new registry boots its first generation, reads every scope's
// visibility and proves every host.
func (r *rig) restart() {
	r.m = newFakeMetrics()
	r.reg = NewRegistry(r.settings(), &Deps{
		Links: r.links, Metrics: r.m, Settle: r.settler, Records: r.records,
		Log: slog.New(slog.NewTextHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	r.reg.now = func() time.Time { return r.now }
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

func (r *rig) install(s *Settings) {
	r.p = r.reg.install(context.Background(), s)
	r.p.visibility(context.Background())
	r.prove()
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
	r.p.retryEnsure(context.Background())
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
