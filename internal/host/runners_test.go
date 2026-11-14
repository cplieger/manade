package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// fakeRunnerDaemon holds containers by name, as a daemon names them.
type fakeRunnerDaemon struct {
	ctrs     map[string]*docker.Container
	volumes  map[string]bool
	archives map[string][]byte
	// create and start, when set, answer a call instead of the default.
	create func(name string, n int) (docker.Result, bool)
	start  func(id string, n int) (docker.Result, bool)
	// listed, when set, runs after a list read the containers and before
	// it answers; inspected after an inspect of ref read its container;
	// starting before a start; removed after a removal of ref.
	listed    func()
	inspected func(ref string)
	starting  func()
	removed   func(ref string)
	// gate holds each create until it closes; with commitLate a create its
	// caller cut still commits once the gate opens, as a daemon under load
	// does, and InspectSettled waits for that.
	gate   chan struct{}
	events chan string
	// inspectFails is how many inspects fail next.
	inspectFails int
	calls        []string
	creates      int
	starts       int
	committing   int
	settling     int
	mu           sync.Mutex
	inspects     int
	commitLate   bool
}

func newFakeRunnerDaemon() *fakeRunnerDaemon {
	return &fakeRunnerDaemon{
		ctrs: map[string]*docker.Container{}, volumes: map[string]bool{}, archives: map[string][]byte{},
		events: make(chan string, 16),
	}
}

func (d *fakeRunnerDaemon) record(call string) {
	d.calls = append(d.calls, call)
}

func (d *fakeRunnerDaemon) Create(ctx context.Context, name string, req *docker.CreateRequest) docker.Result {
	d.mu.Lock()
	gate, late := d.gate, d.commitLate
	d.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			if late {
				d.mu.Lock()
				d.committing++
				d.mu.Unlock()
				go func() {
					<-gate
					d.commit(name, req)
					d.mu.Lock()
					d.committing--
					d.mu.Unlock()
				}()
			}
			return docker.Result{Outcome: docker.Pending, Err: ctx.Err()}
		}
	}
	return d.commit(name, req)
}

func (d *fakeRunnerDaemon) commit(name string, req *docker.CreateRequest) docker.Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.creates++
	d.record("create " + name)
	if d.create != nil {
		if res, ok := d.create(name, d.creates); ok {
			return res
		}
	}
	if c := d.ctrs[name]; c != nil {
		return docker.Result{Outcome: docker.Done, ID: c.ID}
	}
	c := &docker.Container{ID: "id-" + name}
	c.Config.Labels = maps.Clone(req.Labels)
	c.Config.Labels[docker.LabelManaged] = "true"
	c.State.Status = docker.StateCreated
	c.HostConfig.CpusetCpus, c.HostConfig.NanoCPUs, c.HostConfig.Memory = req.HostConfig.CpusetCpus, req.HostConfig.NanoCPUs, req.HostConfig.Memory
	d.ctrs[name] = c
	return docker.Result{Outcome: docker.Done, ID: c.ID}
}

func (d *fakeRunnerDaemon) Start(_ context.Context, id string) docker.Result {
	d.mu.Lock()
	starting := d.starting
	d.mu.Unlock()
	if starting != nil {
		starting()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	d.record("start " + id)
	if d.start != nil {
		if res, ok := d.start(id, d.starts); ok {
			return res
		}
	}
	c := d.byID(id)
	if c == nil {
		return docker.Result{Outcome: docker.Failed, ID: id, Err: errors.New("no such container")}
	}
	c.State.Status, c.State.StartedAt = docker.StateRunning, time.Now()
	return docker.Result{Outcome: docker.Done, ID: id}
}

func (d *fakeRunnerDaemon) byID(ref string) *docker.Container {
	if c := d.ctrs[ref]; c != nil {
		return c
	}
	for _, c := range d.ctrs {
		if c.ID == ref {
			return c
		}
	}
	return nil
}

func (d *fakeRunnerDaemon) InspectContainer(_ context.Context, ref string) (docker.Container, bool, error) {
	d.mu.Lock()
	d.inspects++
	d.record("inspect " + ref)
	if d.inspectFails > 0 {
		d.inspectFails--
		d.mu.Unlock()
		return docker.Container{}, false, errors.New("context deadline exceeded")
	}
	c := d.byID(ref)
	var ctr docker.Container
	if c != nil {
		ctr = *c
	}
	inspected := d.inspected
	d.mu.Unlock()
	if inspected != nil {
		inspected(ref)
	}
	return ctr, c != nil, nil
}

// InspectSettled inspects ref once no create cut by its caller is still
// to commit.
func (d *fakeRunnerDaemon) InspectSettled(ctx context.Context, ref string) (docker.Container, bool, error) {
	d.mu.Lock()
	d.settling++
	d.mu.Unlock()
	for {
		d.mu.Lock()
		n := d.committing
		d.mu.Unlock()
		if n == 0 {
			return d.InspectContainer(ctx, ref)
		}
		if !sleep(ctx, time.Millisecond) {
			return docker.Container{}, false, ctx.Err()
		}
	}
}

func (d *fakeRunnerDaemon) ListContainers(context.Context) ([]docker.Summary, error) {
	d.mu.Lock()
	var out []docker.Summary
	for _, c := range d.ctrs {
		out = append(out, docker.Summary{ID: c.ID, State: c.State.Status, Labels: maps.Clone(c.Config.Labels)})
	}
	listed := d.listed
	d.mu.Unlock()
	if listed != nil {
		listed()
	}
	return out, nil
}

func (d *fakeRunnerDaemon) StopContainer(_ context.Context, id string, _ time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record("stop " + id)
	if c := d.byID(id); c != nil && c.State.Status == docker.StateRunning {
		c.State.Status, c.State.ExitCode, c.State.FinishedAt = docker.StateExited, 143, time.Now()
	}
	return nil
}

func (d *fakeRunnerDaemon) RemoveOwned(_ context.Context, ref string) error {
	d.mu.Lock()
	d.record("remove " + ref)
	for name, c := range d.ctrs {
		if name == ref || c.ID == ref {
			delete(d.ctrs, name)
		}
	}
	removed := d.removed
	d.mu.Unlock()
	if removed != nil {
		removed(ref)
	}
	return nil
}

func (*fakeRunnerDaemon) Logs(_ context.Context, id string, n int) ([]string, error) {
	return []string{fmt.Sprintf("%s line 1", id), fmt.Sprintf("tail %d", n)}, nil
}

func (d *fakeRunnerDaemon) CopyArchive(_ context.Context, id, p string, w io.Writer, _ int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.archives[id+":"+p]
	if !ok {
		return false, nil
	}
	_, err := w.Write(b)
	return true, err
}

func (d *fakeRunnerDaemon) CreateVolume(_ context.Context, labels map[string]string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := "vol-" + labels[jobspec.LabelRunner]
	d.volumes[name] = true
	return name, nil
}

func (d *fakeRunnerDaemon) RemoveOwnedVolume(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.volumes, name)
	return nil
}

func (d *fakeRunnerDaemon) Events(ctx context.Context, fn func(string)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-d.events:
			fn(id)
		}
	}
}

// exit ends container name with code, as the job's end would, and sends its
// die event.
func (d *fakeRunnerDaemon) exit(name string, code int, oom bool) {
	d.mu.Lock()
	c := d.ctrs[name]
	c.State.Status, c.State.ExitCode, c.State.OOMKilled, c.State.FinishedAt = docker.StateExited, code, oom, time.Now()
	id := c.ID
	d.mu.Unlock()
	d.events <- id
}

func (d *fakeRunnerDaemon) callList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// fakeImages serves every profile with one image, unless not ready; each
// tidy pass runs pass, then tidied, before Tidy returns, as the image
// manager's Tidied hook does after its pass.
type fakeImages struct {
	notReady map[string]hostapi.ProfileStatus
	pass     func()
	tidied   func()
	missing  []string
	mu       sync.Mutex
	tidies   int
}

func (f *fakeImages) ForStart(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, nr := f.notReady[p]
	return "sha256:img-" + p, !nr
}

func (f *fakeImages) Missing(p, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missing = append(f.missing, p+" "+id)
}

func (f *fakeImages) Tidy() {
	f.mu.Lock()
	f.tidies++
	pass, tidied := f.pass, f.tidied
	f.mu.Unlock()
	if pass != nil {
		pass()
	}
	if tidied != nil {
		tidied()
	}
}

func (f *fakeImages) Status() []hostapi.ProfileStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := []hostapi.ProfileStatus{}
	for _, name := range []string{"small", "large", "dind"} {
		p, nr := f.notReady[name]
		if !nr {
			p = hostapi.ProfileStatus{Name: name, Ready: true}
		}
		st = append(st, p)
	}
	return st
}

func testProfile(name string, cpus int64) config.Profile {
	return config.Profile{
		Name: name, RunnerImage: "base", Command: []string{"/home/runner/run.sh"}, Hosts: []string{"nas"},
		MilliCPUs: cpus * 1000, Memory: 512 << 20, PIDs: 1024,
	}
}

// testServed is host nas serving profiles small (1 CPU), large (2 CPUs),
// dind (the docker image) and elsewhere, which lists another host only.
func testServed(pinning bool) *served {
	other := testProfile("elsewhere", 1)
	other.Hosts = []string{"other"}
	dind := testProfile("dind", 1)
	dind.RunnerImage, dind.Command = config.ShippedDocker, []string{config.DockerWrapper}
	cfg := &config.Config{
		File: "/config/config.yaml", Self: &config.Self{Name: "nas", TLSDir: "/tls"},
		Profiles: []config.Profile{testProfile("small", 1), testProfile("large", 2), dind, other},
	}
	h := &config.Host{Name: "nas", Egress: config.Egress{Internet: true}}
	cfg.Hosts = []config.Host{*h}
	plat := &jobspec.Platform{Network: jobNetwork, CPUs: 4, Pinning: pinning, PIDs: true, Swap: true}
	return &served{Config: cfg, Host: "nas", Spec: jobspec.NewHost(cfg, h, plat)}
}

type runnersTest struct {
	r   *runnerSet
	d   *fakeRunnerDaemon
	img *fakeImages
	// free is the data volume's free bytes; failReads is how many reads of
	// them fail next.
	free      *atomic.Uint64
	failReads *atomic.Int32
}

// newRunnersTest runs a runnerSet until the test ends; cpus makes it a
// pinning host.
func newRunnersTest(t *testing.T, cpus []int, setup func(d *fakeRunnerDaemon)) *runnersTest {
	t.Helper()
	old, oldGap := pendingRetry, freeReadGap
	pendingRetry, freeReadGap = 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() { pendingRetry, freeReadGap = old, oldGap })
	d := newFakeRunnerDaemon()
	if setup != nil {
		setup(d)
	}
	img := &fakeImages{notReady: map[string]hostapi.ProfileStatus{}}
	free, failReads := &atomic.Uint64{}, &atomic.Int32{}
	free.Store(1 << 30)
	ctx, cancel := context.WithCancel(context.Background())
	r := newRunners(ctx, &runnersConfig{
		Daemon: d, Images: img, Served: testServed(cpus != nil), Log: slog.New(slog.DiscardHandler), CPUs: cpus,
		DataFree: func() (uint64, error) {
			if failReads.Add(-1) >= 0 {
				return 0, errors.New("statfs: input/output error")
			}
			failReads.Store(0)
			return free.Load(), nil
		},
	})
	img.tidied = r.tidied
	if err := r.load(t.Context()); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &runnersTest{r: r, d: d, img: img, free: free, failReads: failReads}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func request(name, profile string) *hostapi.RunnerRequest {
	return &hostapi.RunnerRequest{Profile: profile, RunnerName: name, Scope: "acme/app", JITConfig: "jit-secret", RunnerID: 7}
}

// listed answers runner name as a fresh list shows it.
func (rt *runnersTest) listed(t *testing.T, name string) (hostapi.Runner, bool) {
	t.Helper()
	list, err := rt.r.list(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range list {
		if run.Name == name {
			return run, true
		}
	}
	return hostapi.Runner{}, false
}

// waitDone waits for runner name's work to end.
func (rt *runnersTest) waitDone(t *testing.T, name string) {
	t.Helper()
	waitFor(t, name+"'s work ended", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		select {
		case <-rt.r.records[name].done:
			return true
		default:
			return false
		}
	})
}

func (rt *runnersTest) waitState(t *testing.T, name string, want hostapi.RunnerState) hostapi.Runner {
	t.Helper()
	var run hostapi.Runner
	waitFor(t, name+" "+string(want), func() bool {
		var ok bool
		run, ok = rt.listed(t, name)
		return ok && run.State == want
	})
	return run
}

func TestCreateAnswersBeforeTheDaemon(t *testing.T) {
	gate := make(chan struct{})
	rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.gate = gate })
	run, err := rt.r.accept(t.Context(), request("r1", "small"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the answer is the creating record with its limits", func(t *testing.T) {
		if run.State != hostapi.StateCreating || run.Limits.MilliCPUs != 1000 || run.Limits.Memory != 512<<20 || run.RunnerID != 7 {
			t.Errorf("answer = %+v", run)
		}
	})
	t.Run("a create held past any client deadline lists creating", func(t *testing.T) {
		if got, _ := rt.listed(t, "r1"); got.State != hostapi.StateCreating {
			t.Errorf("listed %+v, want creating", got)
		}
	})
	t.Run("a repeated POST answers the record and starts nothing", func(t *testing.T) {
		again, err := rt.r.accept(t.Context(), request("r1", "small"))
		if err != nil || again.State != hostapi.StateCreating {
			t.Errorf("again = %+v, %v", again, err)
		}
	})
	close(gate)
	got := rt.waitState(t, "r1", hostapi.StateRunning)
	t.Run("then running, built from the checked image ID", func(t *testing.T) {
		if got.StartedAt.IsZero() {
			t.Errorf("running %+v has no start time", got)
		}
		rt.d.mu.Lock()
		img, creates := rt.d.ctrs["r1"].Config.Labels[jobspec.LabelProfile], rt.d.creates
		rt.d.mu.Unlock()
		if img != "small" || creates != 1 {
			t.Errorf("profile label %q, %d creates, want small and 1", img, creates)
		}
	})
	t.Run("the JIT config is never kept in the record", func(t *testing.T) {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		if jit := rt.r.records["r1"].rec.Request.JITConfig; jit != "" {
			t.Errorf("record holds %q", jit)
		}
	})
}

func TestCreateRefusals(t *testing.T) {
	rt := newRunnersTest(t, nil, nil)
	rt.img.notReady["large"] = hostapi.ProfileStatus{Name: "large", Reason: hostapi.ReasonImageMissing, Detail: "pulling again"}
	for _, tc := range []struct {
		name, profile string
		want          error
	}{
		{"an undefined profile", "nope", hostapi.ErrUnknownProfile},
		{"a profile for another host", "elsewhere", hostapi.ErrProfileRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rt.r.accept(t.Context(), request("x-"+tc.profile, tc.profile)); !errors.Is(err, tc.want) {
				t.Errorf("Create = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("a profile not ready answers its reason", func(t *testing.T) {
		_, err := rt.r.accept(t.Context(), request("x-large", "large"))
		nr, ok := errors.AsType[*hostapi.NotReadyError](err)
		if !ok || nr.Reason != hostapi.ReasonImageMissing || nr.Detail != "pulling again" {
			t.Errorf("Create = %v, want not ready with image_missing", err)
		}
	})
	t.Run("a refused request leaves no record", func(t *testing.T) {
		if _, ok := rt.listed(t, "x-large"); ok {
			t.Error("a refused runner is listed")
		}
	})
}

func TestPendingCallsAreRetriedNeverRemoved(t *testing.T) {
	rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
		d.create = func(_ string, n int) (docker.Result, bool) {
			return docker.Result{Outcome: docker.Pending, Err: errors.New("timed out")}, n == 1
		}
		d.start = func(id string, n int) (docker.Result, bool) {
			return docker.Result{Outcome: docker.Pending, ID: id, Err: errors.New("timed out")}, n == 1
		}
	})
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	rt.waitDone(t, "r1")
	calls := rt.d.callList()
	want := []string{"create r1", "create r1", "start id-r1", "inspect id-r1", "start id-r1", "inspect id-r1"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q: each pending call made again, a start only after an inspect found it created, nothing removed", calls, want)
	}
	rt.waitState(t, "r1", hostapi.StateRunning)
}

// A start whose fate is unknown that ran a runner which then exited is not
// issued again: the daemon would run the exited container a second time.
func TestAStartThatRanDuringItsPendingIsNotIssuedAgain(t *testing.T) {
	rt := newRunnersTest(t, []int{0, 1}, func(d *fakeRunnerDaemon) {
		d.start = func(id string, n int) (docker.Result, bool) {
			c := d.byID(id)
			c.State.Status, c.State.ExitCode = docker.StateExited, 3
			c.State.StartedAt, c.State.FinishedAt = time.Now(), time.Now()
			return docker.Result{Outcome: docker.Pending, ID: id, Err: errors.New("timed out")}, n == 1
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := rt.r.subscribe(ctx)
	if _, err := rt.r.accept(t.Context(), request("r1", "large")); err != nil {
		t.Fatal(err)
	}
	var states []hostapi.RunnerState
	var last hostapi.Runner
	for last.State != hostapi.StateExited {
		select {
		case last = <-ch:
			states = append(states, last.State)
		case <-time.After(5 * time.Second):
			t.Fatalf("states %v, no exit", states)
		}
	}
	rt.waitDone(t, "r1")
	t.Run("the runner lists exited with its first exit code, never running again", func(t *testing.T) {
		if slices.Contains(states, hostapi.StateRunning) || last.ExitCode == nil || *last.ExitCode != 3 {
			t.Errorf("states %v, last %+v, want exited 3 and never running", states, last)
		}
	})
	t.Run("the start is issued once", func(t *testing.T) {
		rt.d.mu.Lock()
		defer rt.d.mu.Unlock()
		if rt.d.starts != 1 {
			t.Errorf("%d starts, want 1", rt.d.starts)
		}
	})
	t.Run("its CPUs are free for the next runner", func(t *testing.T) {
		run, err := rt.r.accept(t.Context(), request("r2", "large"))
		if err != nil || run.Limits.CPUSet != "0,1" {
			t.Errorf("r2 = %+v, %v, want r1's CPUs 0,1", run, err)
		}
	})
}

// A start whose fate is unknown whose container then vanished fails, and
// is never issued again.
func TestAPendingStartWhoseContainerIsGoneFails(t *testing.T) {
	rt := newRunnersTest(t, []int{0, 1}, func(d *fakeRunnerDaemon) {
		d.start = func(id string, n int) (docker.Result, bool) {
			for name, c := range d.ctrs {
				if c.ID == id {
					delete(d.ctrs, name)
				}
			}
			return docker.Result{Outcome: docker.Pending, ID: id, Err: errors.New("timed out")}, n == 1
		}
	})
	var once atomic.Bool
	rt.d.mu.Lock()
	rt.d.starting = func() {
		if once.CompareAndSwap(false, true) {
			rt.r.sync(t.Context())
		}
	}
	rt.d.mu.Unlock()
	if _, err := rt.r.accept(t.Context(), request("r1", "large")); err != nil {
		t.Fatal(err)
	}
	rt.waitDone(t, "r1")
	t.Run("the runner fails, naming the container gone", func(t *testing.T) {
		rt.r.mu.Lock()
		got, _ := rt.r.listed("r1")
		rt.r.mu.Unlock()
		if got.State != hostapi.StateFailed || !strings.Contains(got.Reason, "the container is gone") {
			t.Errorf("r1 = %+v, want failed, the container gone", got)
		}
	})
	t.Run("the start is never issued again", func(t *testing.T) {
		rt.d.mu.Lock()
		defer rt.d.mu.Unlock()
		if rt.d.starts != 1 {
			t.Errorf("%d starts, want 1", rt.d.starts)
		}
	})
	t.Run("its CPUs, held by the list's view, are free with no list after", func(t *testing.T) {
		run, err := rt.r.accept(t.Context(), request("r2", "large"))
		if err != nil || run.State == hostapi.StateFailed || run.Limits.CPUSet != "0,1" {
			t.Errorf("r2 = %+v, %v, want r1's CPUs 0,1", run, err)
		}
	})
}

// An inspect that fails after a start whose fate is unknown leaves the
// runner pending, read again until it answers.
func TestAFailedReadAfterAPendingStartKeepsItPending(t *testing.T) {
	rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
		d.start = func(id string, n int) (docker.Result, bool) {
			return docker.Result{Outcome: docker.Pending, ID: id, Err: errors.New("timed out")}, n == 1
		}
		d.inspectFails = 1
	})
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	rt.waitDone(t, "r1")
	calls := rt.d.callList()
	want := []string{"create r1", "start id-r1", "inspect id-r1", "inspect id-r1", "start id-r1", "inspect id-r1"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	rt.waitState(t, "r1", hostapi.StateRunning)
}

func TestFailedStarts(t *testing.T) {
	t.Run("a refused start fails, naming the call, and removes its container", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
			d.start = func(id string, _ int) (docker.Result, bool) {
				return docker.Result{Outcome: docker.Failed, ID: id, Err: errors.New("OCI runtime create failed")}, true
			}
		})
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		got := rt.waitState(t, "r1", hostapi.StateFailed)
		if !strings.Contains(got.Reason, "the start failed: OCI runtime create failed") || got.RemovedBy != removedByFailed {
			t.Errorf("failed = %+v", got)
		}
		if !slices.Contains(rt.d.callList(), "remove id-r1") {
			t.Errorf("calls = %q, want the container removed", rt.d.callList())
		}
	})
	t.Run("a list during the removal shows the runner failed, never exited", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
			d.start = func(id string, _ int) (docker.Result, bool) {
				return docker.Result{Outcome: docker.Failed, ID: id, Err: errors.New("OCI runtime create failed")}, true
			}
			d.removed = func(string) {
				close(entered)
				<-release
			}
		})
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		<-entered
		got, ok := rt.listed(t, "r1")
		close(release)
		if !ok || got.State != hostapi.StateFailed {
			t.Errorf("listed %+v during the removal, want failed", got)
		}
	})
	t.Run("a failed start a list saw frees its CPUs and lists no orphan with no list after", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once atomic.Bool
		rt := newRunnersTest(t, []int{0, 1}, func(d *fakeRunnerDaemon) {
			d.start = func(id string, n int) (docker.Result, bool) {
				return docker.Result{Outcome: docker.Failed, ID: id, Err: errors.New("OCI runtime create failed")}, n == 1
			}
			d.starting = func() {
				if once.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
		})
		if _, err := rt.r.accept(t.Context(), request("r1", "large")); err != nil {
			t.Fatal(err)
		}
		<-entered
		rt.r.sync(t.Context())
		rt.r.mu.Lock()
		seen := rt.r.ctrs["r1"] != nil
		rt.r.mu.Unlock()
		close(release)
		if !seen {
			t.Fatal("the list during the start did not see r1's container")
		}
		rt.waitDone(t, "r1")
		rt.r.mu.Lock()
		got, _ := rt.r.listed("r1")
		rt.r.mu.Unlock()
		if got.State != hostapi.StateFailed || len(got.Orphans) != 0 {
			t.Errorf("r1 = %+v, want failed with no orphan", got)
		}
		run, err := rt.r.accept(t.Context(), request("r2", "large"))
		if err != nil || run.State == hostapi.StateFailed || run.Limits.CPUSet != "0,1" {
			t.Errorf("r2 = %+v, %v, want r1's CPUs 0,1", run, err)
		}
	})
	t.Run("a missing image takes it out of service", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
			d.create = func(string, int) (docker.Result, bool) {
				return docker.Result{Outcome: docker.Failed, Err: fmt.Errorf("%w: sha256:img-small", docker.ErrImageMissing)}, true
			}
		})
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateFailed)
		rt.img.mu.Lock()
		defer rt.img.mu.Unlock()
		if !slices.Equal(rt.img.missing, []string{"small sha256:img-small"}) {
			t.Errorf("Missing calls = %q", rt.img.missing)
		}
	})
}

// noSpaceOnce refuses the start numbered n for want of space.
func noSpaceOnce(n int) func(id string, i int) (docker.Result, bool) {
	return func(id string, i int) (docker.Result, bool) {
		return docker.Result{Outcome: docker.Failed, ID: id, Err: errors.New("write /var/lib/docker/x: no space left on device")}, i == n
	}
}

func (rt *runnersTest) held() bool {
	rt.r.mu.Lock()
	defer rt.r.mu.Unlock()
	return rt.r.space != nil
}

// freesOnTidy makes each tidy pass free n bytes of the data volume.
func (rt *runnersTest) freesOnTidy(n uint64) {
	rt.img.mu.Lock()
	defer rt.img.mu.Unlock()
	rt.img.pass = func() { rt.free.Add(n) }
}

func TestAFullVolume(t *testing.T) {
	t.Run("fails the start naming the volume, holds starts as not ready and tidies", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(2) })
		rt.freesOnTidy(1 << 20)
		rt.img.notReady["large"] = hostapi.ProfileStatus{Name: "large", Reason: hostapi.ReasonPulling, Detail: "no image of base has passed its pull and check yet"}
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateRunning)
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Fatal(err)
		}
		got := rt.waitState(t, "r2", hostapi.StateFailed)
		rt.waitDone(t, "r2")
		if !strings.Contains(got.Reason, "data volume is full ("+dataRoot+")") {
			t.Errorf("reason %q does not name the volume", got.Reason)
		}
		_, err := rt.r.accept(t.Context(), request("r3", "small"))
		if nr, ok := errors.AsType[*hostapi.NotReadyError](err); !ok || nr.Reason != hostapi.ReasonDataFull || !strings.Contains(nr.Detail, dataRoot) {
			t.Errorf("held POST = %v, want not ready naming the volume", err)
		}
		if _, ok := rt.listed(t, "r3"); ok {
			t.Error("a POST refused while held left a record")
		}
		for _, p := range rt.r.profileStatus() {
			if p.Ready || p.Reason != hostapi.ReasonDataFull || !strings.Contains(p.Detail, dataRoot) {
				t.Errorf("status %+v, want every profile not ready for the full volume", p)
			}
			for _, arm := range []string{"a runner exits", "container is removed", "more free space", "no runner running"} {
				if !strings.Contains(p.Detail, arm) {
					t.Errorf("status detail %q does not name the lift %q", p.Detail, arm)
				}
			}
			if p.Name == "large" && !strings.Contains(p.Detail, "pulling: no image of base") {
				t.Errorf("large's detail %q hides its own reason", p.Detail)
			}
		}
		rt.img.mu.Lock()
		tidies := rt.img.tidies
		rt.img.mu.Unlock()
		if tidies != 1 {
			t.Errorf("%d tidy passes, want 1 at once", tidies)
		}
		if !rt.held() {
			t.Error("the tidy pass, which freed space, lifted the hold while a runner runs")
		}
		rt.d.exit("r1", 0, false)
		waitFor(t, "the hold lifted", func() bool { return !rt.held() })
		if _, err := rt.r.accept(t.Context(), request("r4", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r4", hostapi.StateRunning)
	})
	t.Run("an exit seen only by the list lifts the hold", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(2) })
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateRunning)
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r2", hostapi.StateFailed)
		rt.d.mu.Lock()
		c := rt.d.ctrs["r1"]
		c.State.Status, c.State.FinishedAt = docker.StateExited, time.Now()
		rt.d.mu.Unlock()
		rt.r.sync(t.Context())
		if rt.held() {
			t.Error("the list read r1's exit and kept the hold")
		}
	})
	t.Run("with no runner running, the hold lifts only once a pass frees the margin past the free space after the removal", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(1) })
		rt.d.mu.Lock()
		rt.d.removed = func(string) { rt.free.Add(64 << 10) }
		rt.d.mu.Unlock()
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateFailed)
		rt.waitDone(t, "r1")
		rt.r.tidied()
		rt.r.sync(t.Context())
		if !rt.held() {
			t.Fatal("the refused start's removal, a pass and a list that freed nothing lifted the hold")
		}
		rt.freesOnTidy(1<<30 - 32<<10)
		rt.img.Tidy()
		if !rt.held() {
			t.Fatal("a pass that freed less than 1 GiB past the reading after the removal lifted the hold")
		}
		if st := rt.r.profileStatus(); st[0].Reason != hostapi.ReasonDataFull {
			t.Errorf("status %+v, want data_full to stand", st[0])
		}
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err == nil {
			t.Error("a POST was taken while the full volume's hold stands")
		}
		rt.freesOnTidy(32 << 10)
		rt.img.Tidy()
		if rt.held() {
			t.Fatal("passes that freed 1 GiB in all kept the hold with no runner running")
		}
		if _, err := rt.r.accept(t.Context(), request("r3", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r3", hostapi.StateRunning)
	})
	t.Run("a list while the refused start's container is removed keeps the hold", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
			d.start = noSpaceOnce(1)
			d.removed = func(string) {
				close(entered)
				<-release
			}
		})
		rt.free.Store(4 << 30)
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		<-entered
		rt.r.sync(t.Context())
		held := rt.held()
		close(release)
		if !held {
			t.Error("a list before the hold's baseline was read lifted it")
		}
	})
	t.Run("a list that finds the volume grown lifts the hold", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(1) })
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateFailed)
		rt.waitDone(t, "r1")
		rt.free.Add(10 << 30)
		rt.r.sync(t.Context())
		if rt.held() {
			t.Error("a list that found more free space kept the hold with no runner running")
		}
	})
	t.Run("a served profile's disk above 1 GiB raises the margin", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(1) })
		sv := testServed(false)
		sv.Config.Profiles[1].Disk = 4 << 30
		sv.Profiles = []*config.Profile{&sv.Config.Profiles[0], &sv.Config.Profiles[1]}
		rt.r.setServed(sv)
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateFailed)
		rt.waitDone(t, "r1")
		rt.free.Add(4<<30 - 1)
		rt.r.sync(t.Context())
		if !rt.held() {
			t.Fatal("a list that found less than the largest profile's 4 GiB disk lifted the hold")
		}
		rt.free.Add(1)
		rt.r.sync(t.Context())
		if rt.held() {
			t.Error("a list that found 4 GiB more kept the hold")
		}
	})
	t.Run("a first read that fails is tried again before the pass, which then keeps the hold if it frees nothing", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(1) })
		rt.failReads.Store(1)
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateFailed)
		rt.waitDone(t, "r1")
		if !rt.held() {
			t.Error("a pass that freed nothing lifted the hold, as if no read had measured it")
		}
	})
	t.Run("a first read that fails is tried again before the pass, whose freed space lifts the hold", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(1) })
		rt.failReads.Store(1)
		rt.freesOnTidy(2 << 30)
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateFailed)
		rt.waitDone(t, "r1")
		if rt.held() {
			t.Error("the pass freed 2 GiB past the retried read and the hold stands")
		}
	})
	t.Run("a hold no read could measure lifts at the pass's end with no runner running", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(1) })
		rt.failReads.Store(int32(freeReads))
		var heldAtPass atomic.Bool
		rt.img.mu.Lock()
		rt.img.pass = func() {
			rt.r.sync(t.Context())
			heldAtPass.Store(rt.held())
		}
		rt.img.mu.Unlock()
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateFailed)
		rt.waitDone(t, "r1")
		if !heldAtPass.Load() {
			t.Error("the hold was not standing when the tidy pass ran, after a list")
		}
		if rt.held() {
			t.Error("the hold with no reading stands after the pass ended with no runner running")
		}
	})
	t.Run("an exit from before the hold does not lift it", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(3) })
		for _, name := range []string{"r1", "r2"} {
			if _, err := rt.r.accept(t.Context(), request(name, "small")); err != nil {
				t.Fatal(err)
			}
			rt.waitState(t, name, hostapi.StateRunning)
		}
		rt.d.exit("r1", 0, false)
		rt.waitState(t, "r1", hostapi.StateExited)
		if _, err := rt.r.accept(t.Context(), request("r3", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r3", hostapi.StateFailed)
		rt.waitDone(t, "r3")
		rt.r.sync(t.Context())
		if !rt.held() {
			t.Error("r1's exit, older than the hold, lifted it")
		}
	})
	t.Run("a deleted runner's removal lifts the hold", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.start = noSpaceOnce(2) })
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateRunning)
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r2", hostapi.StateFailed)
		if _, err := rt.r.remove(t.Context(), "r1", false); err != nil {
			t.Fatal(err)
		}
		if rt.held() {
			t.Error("removing a runner's container kept the hold")
		}
	})
}

func TestRestartListsCreatedContainersFailed(t *testing.T) {
	rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
		c := &docker.Container{ID: "id-old"}
		c.Config.Labels = map[string]string{docker.LabelManaged: "true", jobspec.LabelRunner: "old", jobspec.LabelProfile: "small", jobspec.LabelRunnerID: "9"}
		c.State.Status = docker.StateCreated
		d.ctrs["old"] = c
	})
	got, ok := rt.listed(t, "old")
	if !ok || got.State != hostapi.StateFailed || got.Reason != hostapi.ReasonHostRestarted || got.RunnerID != 9 {
		t.Errorf("listed %+v, %v, want failed %q", got, ok, hostapi.ReasonHostRestarted)
	}
}

func TestPinning(t *testing.T) {
	setup := func(d *fakeRunnerDaemon) {
		c := &docker.Container{ID: "id-kept"}
		c.Config.Labels = map[string]string{docker.LabelManaged: "true", jobspec.LabelRunner: "kept", jobspec.LabelProfile: "small"}
		c.State.Status, c.HostConfig.CpusetCpus = docker.StateRunning, "0"
		d.ctrs["kept"] = c
	}
	rt := newRunnersTest(t, []int{0, 1, 2, 3}, setup)
	t.Run("a supervisor restart keeps a running container's CPUs", func(t *testing.T) {
		run, err := rt.r.accept(t.Context(), request("a", "large"))
		if err != nil || run.Limits.CPUSet != "1,2" || run.Limits.MilliCPUs != 0 {
			t.Errorf("a = %+v, %v, want CPUs 1,2 beside kept's 0", run, err)
		}
	})
	t.Run("more than the free CPUs fails the runner", func(t *testing.T) {
		run, err := rt.r.accept(t.Context(), request("b", "large"))
		if err != nil || run.State != hostapi.StateFailed || !strings.Contains(run.Reason, "1 free CPUs") {
			t.Errorf("b = %+v, %v", run, err)
		}
	})
	rt.waitState(t, "a", hostapi.StateRunning)
	t.Run("an exited container frees its CPUs", func(t *testing.T) {
		rt.d.exit("kept", 0, false)
		rt.waitState(t, "kept", hostapi.StateExited)
		run, err := rt.r.accept(t.Context(), request("c", "large"))
		if err != nil || run.Limits.CPUSet != "0,3" {
			t.Errorf("c = %+v, %v, want CPUs 0,3", run, err)
		}
	})
	t.Run("the CPUs reach the container", func(t *testing.T) {
		rt.waitState(t, "c", hostapi.StateRunning)
		rt.d.mu.Lock()
		defer rt.d.mu.Unlock()
		if got := rt.d.ctrs["c"].HostConfig.CpusetCpus; got != "0,3" {
			t.Errorf("CpusetCpus = %q", got)
		}
	})
}

func TestDelete(t *testing.T) {
	rt := newRunnersTest(t, nil, nil)
	if _, err := rt.r.accept(t.Context(), request("r1", "dind")); err != nil {
		t.Fatal(err)
	}
	rt.waitState(t, "r1", hostapi.StateRunning)
	rt.d.mu.Lock()
	rt.d.archives["id-r1:"+defaultDiagDir] = []byte("tar")
	rt.d.mu.Unlock()
	diag, err := rt.r.remove(t.Context(), "r1", true)
	t.Run("copies _diag, stops and removes the container, drops the record and its volume", func(t *testing.T) {
		if err != nil || string(diag) != "tar" {
			t.Errorf("Delete = %q, %v", diag, err)
		}
		calls := rt.d.callList()
		if !slices.Contains(calls, "stop id-r1") || !slices.Contains(calls, "remove id-r1") {
			t.Errorf("calls = %q", calls)
		}
		if _, ok := rt.listed(t, "r1"); ok {
			t.Error("a deleted runner is still listed")
		}
		rt.d.mu.Lock()
		defer rt.d.mu.Unlock()
		if len(rt.d.volumes) != 0 {
			t.Errorf("volumes left: %v", rt.d.volumes)
		}
	})
	t.Run("a runner with no container is no error", func(t *testing.T) {
		if b, err := rt.r.remove(t.Context(), "gone", true); err != nil || b != nil {
			t.Errorf("Delete = %q, %v", b, err)
		}
	})
	t.Run("a pending runner's work is cut before the removal", func(t *testing.T) {
		gate := make(chan struct{})
		rt.d.mu.Lock()
		rt.d.gate = gate
		rt.d.mu.Unlock()
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.r.remove(t.Context(), "r2", false); err != nil {
			t.Fatal(err)
		}
		if _, ok := rt.listed(t, "r2"); ok {
			t.Error("a deleted pending runner is still listed")
		}
	})
}

func TestEventsAndLogs(t *testing.T) {
	rt := newRunnersTest(t, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := rt.r.subscribe(ctx)
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	rt.waitState(t, "r1", hostapi.StateRunning)
	rt.d.exit("r1", 137, true)
	var states []hostapi.RunnerState
	var last hostapi.Runner
	for last.State != hostapi.StateExited {
		select {
		case last = <-ch:
			states = append(states, last.State)
		case <-time.After(5 * time.Second):
			t.Fatalf("states %v, no exit", states)
		}
	}
	t.Run("each state change is sent once, the exit with its code and OOM flag", func(t *testing.T) {
		want := []hostapi.RunnerState{hostapi.StateCreating, hostapi.StateStarting, hostapi.StateRunning, hostapi.StateExited}
		if !slices.Equal(states, want) || *last.ExitCode != 137 || !last.OOMKilled {
			t.Errorf("states %v, last %+v", states, last)
		}
	})
	t.Run("the subscription closes when its context ends", func(t *testing.T) {
		cancel()
		waitFor(t, "the channel closed", func() bool {
			select {
			case _, open := <-ch:
				return !open
			default:
				return false
			}
		})
	})
	t.Run("logs of an unknown runner", func(t *testing.T) {
		if _, err := rt.r.logs(t.Context(), "nope", 10); !errors.Is(err, hostapi.ErrUnknownRunner) {
			t.Errorf("Logs = %v", err)
		}
		if b, err := rt.r.logs(t.Context(), "r1", 10); err != nil || string(b) != "id-r1 line 1\ntail 10\n" {
			t.Errorf("Logs = %q, %v", b, err)
		}
	})
}

// A list read before a runner's start lands, and swapped in after it, must
// not take the running container back out of the view.
func TestARefreshKeepsWritesMadeDuringIt(t *testing.T) {
	read, release := make(chan struct{}), make(chan struct{})
	rt := newRunnersTest(t, nil, nil)
	rt.d.mu.Lock()
	rt.d.listed = func() {
		close(read)
		<-release
	}
	rt.d.mu.Unlock()
	refreshed := make(chan error, 1)
	go func() { refreshed <- rt.r.refresh(t.Context()) }()
	<-read
	rt.d.mu.Lock()
	rt.d.listed = nil
	rt.d.mu.Unlock()
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "r1 started", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		return rt.r.records["r1"].rec.State == hostapi.RecordStarted
	})
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	rt.r.mu.Lock()
	got, _ := rt.r.listed("r1")
	rt.r.mu.Unlock()
	if got.State != hostapi.StateRunning {
		t.Errorf("r1 = %+v after the stale list, want running", got)
	}
}

// A DELETE that cut a create the daemon then commits removes that
// container before it answers, so no failed runner outlives the DELETE.
func TestADeleteWaitsOutACutCreate(t *testing.T) {
	gate := make(chan struct{})
	rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) { d.gate, d.commitLate = gate, true })
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { _, err := rt.r.remove(t.Context(), "r1", false); deleted <- err }()
	var err error
	answered := false
	waitFor(t, "the DELETE waiting on the cut create, or answered", func() bool {
		select {
		case err = <-deleted:
			answered = true
			return true
		default:
		}
		rt.d.mu.Lock()
		defer rt.d.mu.Unlock()
		return rt.d.settling > 0
	})
	close(gate)
	if !answered {
		err = <-deleted
	}
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cut create committed", func() bool {
		rt.d.mu.Lock()
		defer rt.d.mu.Unlock()
		return rt.d.committing == 0
	})
	if got, ok := rt.listed(t, "r1"); ok {
		t.Errorf("listed %+v after its DELETE answered", got)
	}
}

// A POST served before run starts still builds under the host's run, so
// the host's stop ends it.
func TestWorkRunsUnderTheHostsRunFromTheStart(t *testing.T) {
	d := newFakeRunnerDaemon()
	d.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	r := newRunners(ctx, &runnersConfig{Daemon: d, Images: &fakeImages{}, Served: testServed(false), Log: slog.New(slog.DiscardHandler)})
	if _, err := r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the build ignored the host's stop")
	}
}

// A read made with the lock dropped never writes over a newer write for the
// same name: a start's inspect that read the running container before its
// die was handled leaves the exited view.
func TestAStartsInspectDoesNotHideAnExit(t *testing.T) {
	read, release := make(chan struct{}), make(chan struct{})
	rt := newRunnersTest(t, nil, nil)
	var first atomic.Bool
	rt.d.mu.Lock()
	rt.d.inspected = func(ref string) {
		if ref == "id-r1" && first.CompareAndSwap(false, true) {
			close(read)
			<-release
		}
	}
	rt.d.mu.Unlock()
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	<-read
	rt.d.exit("r1", 0, false)
	waitFor(t, "the die handled", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		c := rt.r.ctrs["r1"]
		return c != nil && c.State == docker.StateExited
	})
	close(release)
	waitFor(t, "r1 recorded started", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		return rt.r.records["r1"].rec.State == hostapi.RecordStarted
	})
	rt.r.mu.Lock()
	got, _ := rt.r.listed("r1")
	rt.r.mu.Unlock()
	if got.State != hostapi.StateExited {
		t.Errorf("r1 = %+v, want exited: the start's older read put its running view back", got)
	}
}

// A die's inspect still in flight when a DELETE removes the runner does not
// put the deleted runner back.
func TestAnExitsInspectDoesNotUndoADelete(t *testing.T) {
	read, release := make(chan struct{}), make(chan struct{})
	rt := newRunnersTest(t, nil, nil)
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	rt.waitState(t, "r1", hostapi.StateRunning)
	var first atomic.Bool
	rt.d.mu.Lock()
	rt.d.inspected = func(ref string) {
		if ref == "id-r1" && first.CompareAndSwap(false, true) {
			close(read)
			<-release
		}
	}
	rt.d.mu.Unlock()
	rt.d.exit("r1", 0, false)
	<-read
	if _, err := rt.r.remove(t.Context(), "r1", false); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, "the die's inspect done", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		return len(rt.r.reading) == 0
	})
	rt.r.mu.Lock()
	got, listed := rt.r.listed("r1")
	_, remembered := rt.r.wrote["r1"]
	rt.r.mu.Unlock()
	if listed {
		t.Errorf("r1 listed %+v after its DELETE answered", got)
	}
	if remembered {
		t.Error("the deleted runner's write is kept after the last read that could write it ended")
	}
}

// A failed create's CPUs are free as soon as its work ends, with no list
// between.
func TestAFailedCreateFreesItsCPUsAtOnce(t *testing.T) {
	rt := newRunnersTest(t, []int{0, 1}, func(d *fakeRunnerDaemon) {
		d.create = func(name string, _ int) (docker.Result, bool) {
			return docker.Result{Outcome: docker.Failed, Err: errors.New("invalid mount")}, name == "r1"
		}
	})
	if _, err := rt.r.accept(t.Context(), request("r1", "large")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "r1's work ended", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		select {
		case <-rt.r.records["r1"].done:
			return true
		default:
			return false
		}
	})
	run, err := rt.r.accept(t.Context(), request("r2", "large"))
	if err != nil || run.State == hostapi.StateFailed || run.Limits.CPUSet != "0,1" {
		t.Errorf("r2 = %+v, %v, want r1's CPUs 0,1", run, err)
	}
}

// openServed is testServed with host nas open.
func openServed() *served {
	sv := testServed(false)
	sv.Config.Hosts[0].Egress.Open = true
	return sv
}

func TestAReloadThatClosesTheHostDrainsIt(t *testing.T) {
	setup := func(t *testing.T) *runnersTest {
		t.Helper()
		rt := newRunnersTest(t, nil, nil)
		rt.r.setServed(openServed())
		for _, name := range []string{"done", "r1"} {
			if _, err := rt.r.accept(t.Context(), request(name, "small")); err != nil {
				t.Fatal(err)
			}
			rt.waitState(t, name, hostapi.StateRunning)
		}
		rt.d.exit("done", 0, false)
		rt.waitState(t, "done", hostapi.StateExited)
		rt.r.setServed(testServed(false))
		return rt
	}
	t.Run("holds starts until the runners started while open have ended", func(t *testing.T) {
		rt := setup(t)
		for _, p := range rt.r.profileStatus() {
			if p.Ready || p.Reason != hostapi.ReasonOpenDraining || !strings.Contains(p.Detail, "r1") || strings.Contains(p.Detail, "done") {
				t.Errorf("status %+v, want every profile not ready naming r1 alone", p)
			}
		}
		_, err := rt.r.accept(t.Context(), request("r2", "small"))
		if nr, ok := errors.AsType[*hostapi.NotReadyError](err); !ok || nr.Reason != hostapi.ReasonOpenDraining {
			t.Errorf("POST while draining = %v, want not ready", err)
		}
		rt.d.exit("r1", 0, false)
		waitFor(t, "the drain over", func() bool {
			rt.r.mu.Lock()
			defer rt.r.mu.Unlock()
			return len(rt.r.draining) == 0
		})
		if st := rt.r.profileStatus(); !st[0].Ready {
			t.Errorf("status %+v after the drain, want ready", st[0])
		}
		if _, err := rt.r.accept(t.Context(), request("r3", "small")); err != nil {
			t.Errorf("POST after the drain = %v", err)
		}
	})
	t.Run("a removal ends a runner's part in it", func(t *testing.T) {
		rt := setup(t)
		if _, err := rt.r.remove(t.Context(), "r1", false); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Errorf("POST after the removal = %v", err)
		}
	})
	t.Run("opening the host again ends it", func(t *testing.T) {
		rt := setup(t)
		rt.r.setServed(openServed())
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Errorf("POST once open again = %v", err)
		}
	})
	t.Run("a closed host's own runners hold nothing", func(t *testing.T) {
		rt := newRunnersTest(t, nil, nil)
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateRunning)
		rt.r.setServed(testServed(false))
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Errorf("POST after a reload that kept the host closed = %v", err)
		}
	})
}

// A start's read from before the die that lands after the die's inspect
// began, and before it wrote, still loses to the exited view.
func TestAnExitWinsOverAnOlderReadWrittenFirst(t *testing.T) {
	startRead, exitRead := make(chan struct{}), make(chan struct{})
	releaseStart, releaseExit := make(chan struct{}), make(chan struct{})
	rt := newRunnersTest(t, nil, nil)
	var calls atomic.Int32
	rt.d.mu.Lock()
	rt.d.inspected = func(ref string) {
		if ref != "id-r1" {
			return
		}
		switch calls.Add(1) {
		case 1:
			close(startRead)
			<-releaseStart
		case 2:
			close(exitRead)
			<-releaseExit
		}
	}
	rt.d.mu.Unlock()
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	<-startRead
	rt.d.exit("r1", 0, false)
	<-exitRead
	close(releaseStart)
	waitFor(t, "r1 recorded started", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		return rt.r.records["r1"].rec.State == hostapi.RecordStarted
	})
	close(releaseExit)
	waitFor(t, "the die's inspect done", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		return len(rt.r.reading) == 0
	})
	rt.r.mu.Lock()
	got, _ := rt.r.listed("r1")
	rt.r.mu.Unlock()
	if got.State != hostapi.StateExited {
		t.Errorf("r1 = %+v, want exited", got)
	}
}

// A list that read a runner's exit keeps it over a start's older read
// written while the list was out.
func TestAListsExitWinsOverAStartsOlderRead(t *testing.T) {
	startRead, releaseStart := make(chan struct{}), make(chan struct{})
	listRead, releaseList := make(chan struct{}), make(chan struct{})
	rt := newRunnersTest(t, nil, nil)
	var first atomic.Bool
	rt.d.mu.Lock()
	rt.d.inspected = func(ref string) {
		if ref == "id-r1" && first.CompareAndSwap(false, true) {
			close(startRead)
			<-releaseStart
		}
	}
	rt.d.mu.Unlock()
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	<-startRead
	rt.d.mu.Lock()
	c := rt.d.ctrs["r1"]
	c.State.Status, c.State.FinishedAt = docker.StateExited, time.Now()
	var listOnce atomic.Bool
	rt.d.listed = func() {
		if listOnce.CompareAndSwap(false, true) {
			close(listRead)
			<-releaseList
		}
	}
	rt.d.mu.Unlock()
	refreshed := make(chan error, 1)
	go func() { refreshed <- rt.r.refresh(t.Context()) }()
	<-listRead
	close(releaseStart)
	waitFor(t, "r1 recorded started", func() bool {
		rt.r.mu.Lock()
		defer rt.r.mu.Unlock()
		return rt.r.records["r1"].rec.State == hostapi.RecordStarted
	})
	close(releaseList)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	rt.r.mu.Lock()
	got, _ := rt.r.listed("r1")
	rt.r.mu.Unlock()
	if got.State != hostapi.StateExited {
		t.Errorf("r1 = %+v, want the list's exited view", got)
	}
}
