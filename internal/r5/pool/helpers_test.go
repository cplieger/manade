package pool

import (
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/github"
	"github.com/cplieger/manade/internal/r5/jobspec"
	"github.com/cplieger/manade/internal/r5/obs"
)

// layout is two repo scopes and an organization. default (2 CPUs) serves
// every scope, large (4 CPUs) only acme/app; the budget holds four default
// runners.
const layout = `capacity: {cpus: 8, memory: 32GiB}
queue: {head_reserve_after: 2m}
scopes:
  - repo: acme/app
  - repo: acme/lib
  - org: acme
profiles:
  default: {cpus: 2, memory: 4GiB}
  large: {cpus: 4, memory: 8GiB, scopes: [acme/app]}
`

var (
	appDefault = scaleSet("acme/app", "default", 1)
	appLarge   = scaleSet("acme/app", "large", 2)
	libDefault = scaleSet("acme/lib", "default", 3)
	orgDefault = scaleSet("acme", "default", 4)
)

func scaleSet(scope, profile string, id int) github.ScaleSet {
	return github.ScaleSet{Scope: scope, Profile: profile, ID: id}
}

// load validates file through config.Load.
func load(tb testing.TB, file string) *config.Config {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		tb.Fatal(err)
	}
	for k, v := range map[string]string{
		"CONFIG_FILE": path, "GITHUB_TOKEN": "test-token", "POOL_NAME": "manade", "DOCKER_SOCKET": "/var/run/docker.sock",
		"GITHUB_APP_CLIENT_ID": "", "GITHUB_APP_PRIVATE_KEY": "", "GITHUB_APP_PRIVATE_KEY_FILE": "", "GITHUB_TOKEN_FILE": "",
	} {
		tb.Setenv(k, v)
	}
	cfg, err := config.Load(context.Background())
	if err != nil {
		tb.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// calls is the order the fakes were called in, across both.
type calls struct {
	list []string
	mu   sync.Mutex
}

func (c *calls) add(call string) {
	c.mu.Lock()
	c.list = append(c.list, call)
	c.mu.Unlock()
}

func (c *calls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.list)
}

// fakeGitHub registers runners with rising IDs. jitGate, when set, holds
// each JIT until it is closed. A runner absent from runners reads as not
// found; a scope absent from private reads as private.
type fakeGitHub struct {
	jitErr, runnerErr, recheckErr error
	ensureErr, installErr         error
	jitGate                       chan struct{}
	// hang, when set, holds each listener past its context until closed.
	hang  chan struct{}
	calls *calls
	// sets is what Ensure finds per scope, and stats each scale set's
	// initial statistics by its ID.
	sets        map[string][]github.ScaleSet
	stats       map[int]github.Stats
	removeErr   map[int64]error
	runners     map[int64]github.Runner
	private     map[string]error
	public      map[string]bool
	jits, reads []string
	removed     []int64
	nextID      int64
	rechecks    int
	mu          sync.Mutex
}

func (g *fakeGitHub) JIT(ctx context.Context, _ github.ScaleSet, name string) (github.JIT, error) {
	if err := ctx.Err(); err != nil {
		return github.JIT{}, err
	}
	if g.jitGate != nil {
		select {
		case <-g.jitGate:
		case <-ctx.Done():
			return github.JIT{}, ctx.Err()
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jits = append(g.jits, name)
	if g.jitErr != nil {
		return github.JIT{}, g.jitErr
	}
	g.nextID++
	return github.JIT{Config: "test-jit-config", RunnerID: g.nextID}, nil
}

func (g *fakeGitHub) RemoveRunner(_ context.Context, _ string, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("RemoveRunner " + strconv.FormatInt(id, 10))
	if err := g.removeErr[id]; err != nil {
		return err
	}
	g.removed = append(g.removed, id)
	return nil
}

func (g *fakeGitHub) record(call string) {
	if g.calls != nil {
		g.calls.add(call)
	}
}

func (g *fakeGitHub) Ensure(_ context.Context, s *config.Scope) ([]github.ScaleSet, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("Ensure " + s.Key)
	return g.sets[s.Key], g.ensureErr
}

func (g *fakeGitHub) CheckInstallations(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("CheckInstallations")
	return g.installErr
}

// Listen opens ss's session with its initial statistics, beats once, and
// closes it once ctx ends, as the real listener's edges do.
func (g *fakeGitHub) Listen(ctx context.Context, ss github.ScaleSet, h github.Handler) {
	g.mu.Lock()
	st, hang := g.stats[ss.ID], g.hang
	g.record("Listen " + ss.Scope + " " + ss.Profile)
	g.mu.Unlock()
	h.Opened(ss, st)
	h.Beat(ss)
	<-ctx.Done()
	if hang != nil {
		<-hang
	}
	h.Closed(ss)
	g.mu.Lock()
	g.record("Close " + ss.Scope + " " + ss.Profile)
	g.mu.Unlock()
}

func (g *fakeGitHub) Recheck(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rechecks++
	return g.recheckErr
}

func (g *fakeGitHub) Private(_ context.Context, s *config.Scope) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads = append(g.reads, s.Key)
	g.record("Private " + s.Key)
	if err := g.private[s.Key]; err != nil {
		return false, err
	}
	return !g.public[s.Key], nil
}

func (g *fakeGitHub) Runner(_ context.Context, _ *config.Scope, id int64) (github.Runner, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("Runner " + strconv.FormatInt(id, 10))
	r, found := g.runners[id]
	return r, found, g.runnerErr
}

// fakeBox is one container the fake daemon holds.
type fakeBox struct {
	ctr  docker.Container
	logs []string
	// diag is its _diag archive, nil when it has none.
	diag  []byte
	state string
}

// fakeDocker holds containers and images in memory: a create answers
// "c-<name>" in state created, and a pull of a reference takes the image
// registry holds for it. It records what it is asked.
type fakeDocker struct {
	createErr, startErr, listErr, inspectErr error
	pullErr, imageInspectErr                 error
	// diskErr refuses a create that sets a size limit.
	networkErr, diskErr  error
	pullGate             chan struct{}
	removeErr            map[string]error
	registry, imgs       map[string]docker.Image
	tags                 map[string]string
	files                map[string]jobspec.Files
	calls                *calls
	now                  func() time.Time
	boxes                map[string]*fakeBox
	created              []*docker.CreateRequest
	started, removed     []string
	pulls, removedImages []string
	logsAsked            []int
	// readHook, when set, runs at each ReadFile before it answers.
	readHook func()
	// probeRemoveFails is how many removals of a probe container fail.
	probeRemoveFails int
	mu               sync.Mutex
}

// cache puts img in the daemon's cache under ref; d.mu is held or the
// fake is not yet shared.
func (d *fakeDocker) cache(ref string, img docker.Image) {
	if d.imgs == nil {
		d.imgs, d.tags = map[string]docker.Image{}, map[string]string{}
	}
	if old, ok := d.imgs[d.tags[ref]]; ok {
		old.RepoTags = slices.DeleteFunc(old.RepoTags, func(t string) bool { return t == ref })
		d.imgs[old.ID] = old
	}
	img.RepoTags = append(slices.Clone(img.RepoTags), ref)
	d.imgs[img.ID], d.tags[ref] = img, img.ID
}

func (d *fakeDocker) InspectImage(_ context.Context, ref string) (docker.Image, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.imageInspectErr != nil {
		return docker.Image{}, false, d.imageInspectErr
	}
	id, ok := d.tags[ref]
	if !ok {
		id = ref
	}
	img, found := d.imgs[id]
	return img, found, nil
}

func (d *fakeDocker) PullImage(ctx context.Context, ref string) error {
	d.mu.Lock()
	d.pulls = append(d.pulls, ref)
	gate := d.pullGate
	d.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	img, ok := d.registry[ref]
	if d.pullErr != nil || !ok {
		return cmp.Or(d.pullErr, errFake)
	}
	d.cache(ref, img)
	return nil
}

// RemoveOwnedImage refuses as the real client does: an image without the
// shipped-image label or with a tag is not owned, and one a container
// uses is a conflict.
func (d *fakeDocker) RemoveOwnedImage(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	img, found := d.imgs[id]
	switch {
	case !found:
		return nil
	case img.Config.Labels[docker.LabelRunnerImage] == "" || len(img.RepoTags) > 0:
		return docker.ErrNotOwned
	}
	for _, b := range d.boxes {
		if b.ctr.Image == id {
			return docker.ErrConflict
		}
	}
	delete(d.imgs, id)
	d.removedImages = append(d.removedImages, id)
	return nil
}

func (d *fakeDocker) ReadFile(_ context.Context, id, path string, _ int64) ([]byte, bool, error) {
	if d.readHook != nil {
		d.readHook()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.files[d.boxes[id].ctr.Image]
	data := map[string][]byte{jobspec.PasswdPath: f.Passwd, jobspec.GroupPath: f.Group}[path]
	return data, data != nil, nil
}

func (d *fakeDocker) EnsureNetwork(_ context.Context, spec docker.NetworkSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls != nil {
		d.calls.add("EnsureNetwork " + spec.Name)
	}
	return d.networkErr
}

func (d *fakeDocker) CreateContainer(_ context.Context, name string, req *docker.CreateRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createErr != nil {
		return "", d.createErr
	}
	if req.HostConfig.StorageOpt != nil && d.diskErr != nil {
		return "", d.diskErr
	}
	d.created = append(d.created, req)
	b := &fakeBox{state: "created"}
	b.ctr.ID, b.ctr.Image = "c-"+name, req.Image
	b.ctr.Config.Labels = maps.Clone(req.Labels)
	b.ctr.Config.Labels[docker.LabelManaged] = "true"
	b.ctr.Config.Labels[docker.LabelCreated] = d.now().UTC().Format(time.RFC3339)
	b.ctr.HostConfig.NanoCPUs, b.ctr.HostConfig.Memory = req.HostConfig.NanoCPUs, req.HostConfig.Memory
	d.add(b)
	return b.ctr.ID, nil
}

// add puts b in the daemon; d.mu is held.
func (d *fakeDocker) add(b *fakeBox) {
	if d.boxes == nil {
		d.boxes = map[string]*fakeBox{}
	}
	d.boxes[b.ctr.ID] = b
}

func (d *fakeDocker) StartContainer(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.startErr != nil {
		return d.startErr
	}
	d.started = append(d.started, id)
	if b := d.boxes[id]; b != nil {
		b.state = "running"
	}
	return nil
}

func (d *fakeDocker) RemoveOwned(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls != nil {
		d.calls.add("RemoveOwned " + id)
	}
	if err := d.removeErr[id]; err != nil {
		return err
	}
	if b := d.boxes[id]; b != nil && b.ctr.Config.Labels[docker.LabelProbe] != "" && d.probeRemoveFails > 0 {
		d.probeRemoveFails--
		return errFake
	}
	d.removed = append(d.removed, id)
	delete(d.boxes, id)
	return nil
}

func (d *fakeDocker) ListContainers(context.Context) ([]docker.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return nil, d.listErr
	}
	var out []docker.Summary
	for _, id := range slices.Sorted(maps.Keys(d.boxes)) {
		b := d.boxes[id]
		out = append(out, docker.Summary{ID: id, Labels: maps.Clone(b.ctr.Config.Labels), State: b.state})
	}
	return out, nil
}

func (d *fakeDocker) InspectContainer(_ context.Context, id string) (docker.Container, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.boxes[id]
	if d.inspectErr != nil || b == nil {
		return docker.Container{}, false, d.inspectErr
	}
	return b.ctr, true, nil
}

func (d *fakeDocker) Logs(_ context.Context, id string, n int) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logsAsked = append(d.logsAsked, n)
	return d.boxes[id].logs, nil
}

func (d *fakeDocker) CopyArchive(_ context.Context, id, _ string, w io.Writer, limit int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.boxes[id]
	if b == nil || b.diag == nil {
		return false, nil
	}
	if int64(len(b.diag)) > limit {
		_, _ = w.Write(b.diag[:limit])
		return true, docker.ErrTooLarge
	}
	_, err := w.Write(b.diag)
	return true, err
}

func (*fakeDocker) Events(ctx context.Context, _ func(string)) error {
	<-ctx.Done()
	return ctx.Err()
}

// exit ends runner name's container with code, killed by the kernel's OOM
// killer when oom is set.
func (d *fakeDocker) exit(name string, code int, oom bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.boxes["c-"+name]
	b.state = "exited"
	b.ctr.State.ExitCode, b.ctr.State.OOMKilled = code, oom
}

// box finds runner name's container, nil when it is gone.
func (d *fakeDocker) box(name string) *fakeBox {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.boxes["c-"+name]
}

var errFake = errors.New("fake failure")

// fixture is a pool over layout, its four scale sets added with an open
// session, every scope proven private and every image in service, at
// epoch on a settable clock.
type fixture struct {
	p     *Pool
	gh    *fakeGitHub
	dk    *fakeDocker
	calls *calls
	m     *obs.Metrics
	rec   *capture.Recorder
	at    time.Time
}

func newFixture(tb testing.TB) *fixture {
	tb.Helper()
	return newFixtureFrom(tb, load(tb, layout), appDefault, appLarge, libDefault, orgDefault)
}

// newFixtureFrom is newFixture over cfg with the scale sets given.
func newFixtureFrom(tb testing.TB, cfg *config.Config, sets ...github.ScaleSet) *fixture {
	tb.Helper()
	fx := newUnbooted(tb, cfg)
	for _, ss := range sets {
		fx.p.addSet(ss)
		fx.p.Opened(ss, github.Stats{})
	}
	for i := range cfg.Profiles {
		fx.p.setImage(i, "sha256:"+cfg.Profiles[i].Name)
	}
	fx.proveAll()
	fx.p.booting = false
	<-fx.p.kick
	return fx
}

// newUnbooted is a pool over cfg as New leaves it, before boot, on the
// fakes, at epoch on a settable clock.
func newUnbooted(tb testing.TB, cfg *config.Config) *fixture {
	tb.Helper()
	log, rec := capture.New()
	m, err := obs.New(cfg, log)
	if err != nil {
		tb.Fatalf("obs.New: %v", err)
	}
	order := &calls{}
	fx := &fixture{gh: &fakeGitHub{calls: order}, dk: &fakeDocker{calls: order}, calls: order, m: m, rec: rec, at: epoch}
	fx.dk.now = func() time.Time { return fx.at }
	fx.p = New(cfg, &Deps{GitHub: fx.gh, REST: fx.gh, Sweep: fakeSweep{}, Docker: fx.dk, Host: jobspec.NewHost(cfg, 0), Metrics: m, Log: log})
	fx.p.now = func() time.Time { return fx.at }
	return fx
}

// fakeSweep reads nothing until ctx ends.
type fakeSweep struct{}

func (fakeSweep) Run(ctx context.Context, _ func() bool, _ func(time.Time, []github.Queue)) {
	<-ctx.Done()
}

// proveAll records a good visibility read of every scope now.
func (fx *fixture) proveAll() {
	fx.p.mu.Lock()
	defer fx.p.mu.Unlock()
	for _, sc := range fx.p.scopes {
		fx.p.prove(sc, true, nil, fx.at)
	}
}

func (fx *fixture) advance(d time.Duration) { fx.at = fx.at.Add(d) }

// capacity is ss's figure for a poll beginning now.
func (fx *fixture) capacity(ss github.ScaleSet) int { return fx.p.Capacity(ss, fx.at) }

// polled answers ss's poll begun at started, now, with the statistics and
// events given.
func (fx *fixture) polled(ss github.ScaleSet, started time.Time, st *github.Stats, events ...github.Event) {
	fx.p.Polled(ss, &github.Poll{Started: started, Ended: fx.at, Stats: st, Events: events, Idle: st == nil && len(events) == 0})
}

// sweep hands the pool one complete read of each scope given, ended now.
func (fx *fixture) sweep(reads map[string][]github.QueuedJob) {
	var qs []github.Queue
	for scope, jobs := range reads {
		qs = append(qs, github.Queue{Scope: scope, Jobs: jobs, Complete: true})
	}
	fx.p.queued(fx.at, qs)
}

// job is a queued job of profile, queued minute minutes after epoch.
func job(profile string, run int64, name string, minute int) github.QueuedJob {
	return github.QueuedJob{Profile: profile, RunID: run, Name: name, Created: epoch.Add(time.Duration(minute) * time.Minute)}
}

// jobs is n queued jobs of profile in run, a minute apart from minute.
func jobs(profile string, run int64, n, minute int) []github.QueuedJob {
	out := make([]github.QueuedJob, n)
	for i := range out {
		out[i] = job(profile, run, "job-"+strconv.Itoa(i), minute+i)
	}
	return out
}

// startAll runs every owed start synchronously and reports how many ran.
func (fx *fixture) startAll(tb testing.TB) int {
	tb.Helper()
	rs := fx.p.owed()
	for _, r := range rs {
		fx.p.start(tb.Context(), r)
	}
	return len(rs)
}

// runners counts ss's runner records by state.
func (fx *fixture) runners(ss github.ScaleSet) map[runnerState]int {
	fx.p.mu.Lock()
	defer fx.p.mu.Unlock()
	out := map[runnerState]int{}
	for _, r := range fx.p.set(ss).runners {
		out[r.state]++
	}
	return out
}

// counts finds ss's row in a snapshot.
func counts(snap obs.Snapshot, ss github.ScaleSet) obs.Counts {
	for _, c := range snap.Counts {
		if c.ScaleSet == ss.ScaleSet {
			return c
		}
	}
	return obs.Counts{}
}
