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
	create   func(name string, n int) (docker.Result, bool)
	start    func(id string, n int) (docker.Result, bool)
	gate     chan struct{}
	events   chan string
	calls    []string
	creates  int
	starts   int
	mu       sync.Mutex
	inspects int
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
	if d.gate != nil {
		select {
		case <-d.gate:
		case <-ctx.Done():
			return docker.Result{Outcome: docker.Pending, Err: ctx.Err()}
		}
	}
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
	defer d.mu.Unlock()
	d.inspects++
	c := d.byID(ref)
	if c == nil {
		return docker.Container{}, false, nil
	}
	return *c, true, nil
}

func (d *fakeRunnerDaemon) ListContainers(context.Context) ([]docker.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []docker.Summary
	for _, c := range d.ctrs {
		out = append(out, docker.Summary{ID: c.ID, State: c.State.Status, Labels: maps.Clone(c.Config.Labels)})
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
	defer d.mu.Unlock()
	d.record("remove " + ref)
	for name, c := range d.ctrs {
		if name == ref || c.ID == ref {
			delete(d.ctrs, name)
		}
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

// fakeImages serves every profile with one image, unless not ready.
type fakeImages struct {
	notReady map[string]hostapi.ProfileStatus
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
	defer f.mu.Unlock()
	f.tidies++
}

func (f *fakeImages) Status() []hostapi.ProfileStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Collect(maps.Values(f.notReady))
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
}

// newRunnersTest runs a runnerSet until the test ends; cpus makes it a
// pinning host.
func newRunnersTest(t *testing.T, cpus []int, setup func(d *fakeRunnerDaemon)) *runnersTest {
	t.Helper()
	old := pendingRetry
	pendingRetry = 10 * time.Millisecond
	t.Cleanup(func() { pendingRetry = old })
	d := newFakeRunnerDaemon()
	if setup != nil {
		setup(d)
	}
	img := &fakeImages{notReady: map[string]hostapi.ProfileStatus{}}
	r := newRunners(&runnersConfig{Daemon: d, Images: img, Served: testServed(cpus != nil), Log: slog.New(slog.DiscardHandler), CPUs: cpus})
	if err := r.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "Run started", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.base == ctx })
	return &runnersTest{r: r, d: d, img: img}
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
	rt.waitState(t, "r1", hostapi.StateRunning)
	calls := rt.d.callList()
	if !slices.Equal(calls, []string{"create r1", "create r1", "start id-r1", "start id-r1"}) {
		t.Errorf("calls = %q, want each pending call made again and nothing removed", calls)
	}
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
	t.Run("a full volume fails the start, holds starts until an exit and tidies", func(t *testing.T) {
		rt := newRunnersTest(t, nil, func(d *fakeRunnerDaemon) {
			d.start = func(id string, n int) (docker.Result, bool) {
				return docker.Result{Outcome: docker.Failed, ID: id, Err: errors.New("write /var/lib/docker/x: no space left on device")}, n == 2
			}
		})
		if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r1", hostapi.StateRunning)
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Fatal(err)
		}
		got := rt.waitState(t, "r2", hostapi.StateFailed)
		if !strings.Contains(got.Reason, "data volume is full ("+dataRoot+")") {
			t.Errorf("reason %q does not name the volume", got.Reason)
		}
		held, err := rt.r.accept(t.Context(), request("r3", "small"))
		if err != nil || held.State != hostapi.StateFailed || !strings.Contains(held.Reason, "starts are held") {
			t.Errorf("held = %+v, %v", held, err)
		}
		rt.img.mu.Lock()
		tidies := rt.img.tidies
		rt.img.mu.Unlock()
		if tidies != 1 {
			t.Errorf("%d tidy passes, want 1 at once", tidies)
		}
		rt.d.exit("r1", 0, false)
		waitFor(t, "the hold lifted", func() bool { rt.r.mu.Lock(); defer rt.r.mu.Unlock(); return rt.r.spaceHeld == "" })
		if _, err := rt.r.accept(t.Context(), request("r4", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r4", hostapi.StateRunning)
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
