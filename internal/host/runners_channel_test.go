package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/host/images"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/hostlink"
	"github.com/cplieger/manade/internal/jobspec"
)

// pipeListener is the hosts socket in memory: each dial hands the listener
// one end of a net.Pipe, which a synctest bubble counts as durably blocked.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "hosts.sock", Net: "unix"} }

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	host, cp := net.Pipe()
	select {
	case l.conns <- cp:
		return host, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// linked connects s to a control plane over the host channel until the
// test ends, answering the control plane's link to s.
func linked(t *testing.T, s *supervisor) *hostlink.Link {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(t.Context())
	ln := newPipeListener()
	srv := hostlink.NewServer(log)
	u := hostlink.NewUplink(&hostlink.UplinkOptions{
		Dial:    ln.dial,
		Hello:   func() hostlink.Hello { return hostlink.Hello{Name: "lan", Build: testBuild, Instance: s.instance} },
		Handler: hostapi.NewServer(&backend{s: s}, s.instance, log).Handler(),
		Log:     log,
	})
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(ctx, ln) })
	wg.Go(func() { u.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	synctest.Wait()
	l, ok := srv.Link("lan")
	if !ok {
		t.Fatalf("no link to the host, whose channel is %+v", u.State())
	}
	return l
}

// testBuild is the host's build in these tests, which picks the shipped
// images' tags.
const testBuild = "v1.0.0-dev.1"

// stream is a host's events stream as the control plane reads it over its
// link: the runner lines so far, and how the stream ended.
type stream struct {
	err   error
	runs  []hostapi.Runner
	ended bool
	mu    sync.Mutex
}

// follow reads l's events stream until the test ends.
func follow(t *testing.T, l *hostlink.Link) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	st := &stream{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := l.API.Events(ctx, func(ev *hostapi.Event) {
			if ev.Runner != nil {
				st.mu.Lock()
				st.runs = append(st.runs, *ev.Runner)
				st.mu.Unlock()
			}
		})
		st.mu.Lock()
		st.err, st.ended = err, true
		st.mu.Unlock()
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	synctest.Wait()
	return st
}

// of answers the lines for runner name, in order.
func (st *stream) of(name string) []hostapi.Runner {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(st.runs), func(r hostapi.Runner) bool { return r.Name != name })
}

// open answers nil while the stream is open, else how it ended.
func (st *stream) open() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.ended {
		return nil
	}
	return fmt.Errorf("the stream ended: %v", st.err)
}

func states(runs []hostapi.Runner) []hostapi.RunnerState {
	out := make([]hostapi.RunnerState, len(runs))
	for i, r := range runs {
		out[i] = r.State
	}
	return out
}

// serving gives s a runner set over d and img, run until the test ends, as
// the supervisor's build makes it over the host's daemon.
func serving(t *testing.T, s *supervisor, d runnerDaemon, img runnerImages) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s.runners = newRunners(ctx, &runnersConfig{
		Daemon: d, Images: img, Served: s.served.Load(), Log: s.log, Cgroups: newFakeCgroups(),
		DataFree: func() (uint64, error) { return 1 << 30, nil },
	})
	if err := s.runners.load(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.runners.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func listedOver(t *testing.T, l *hostlink.Link, name string) (hostapi.Runner, bool) {
	t.Helper()
	list, err := l.API.Runners(t.Context())
	if err != nil {
		t.Fatalf("GET /v1/runners = %v", err)
	}
	for _, r := range list.Runners {
		if r.Name == name {
			return r, true
		}
	}
	return hostapi.Runner{}, false
}

// A start the daemon answers late is never a failed start. Over the
// channel, the control plane's POST is answered before the daemon's start,
// its list and status answer while the start is held, the events stream
// stays open through it, and the runner ends running from one start with
// nothing removed.
func TestASlowStartOverTheChannelIsStarted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hold   time.Duration
		answer docker.Outcome
	}{
		{"answered past the control plane's call bounds", 2 * time.Minute, docker.Done},
		{"cut by the daemon client's own timeout, the container running", docker.CallTimeout, docker.Pending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
				d := newFakeRunnerDaemon()
				var held atomic.Bool
				d.starting = func() {
					if !held.CompareAndSwap(false, true) {
						return
					}
					time.Sleep(tc.hold)
					d.mu.Lock()
					c := d.ctrs["r1"]
					c.State.Status, c.State.StartedAt = docker.StateRunning, time.Now()
					d.mu.Unlock()
				}
				d.start = func(id string, _ int) (docker.Result, bool) {
					res := docker.Result{Outcome: tc.answer, ID: id}
					if tc.answer == docker.Pending {
						res.Err = context.DeadlineExceeded
					}
					return res, true
				}
				serving(t, s, d, &fakeImages{notReady: map[string]hostapi.ProfileStatus{}})
				l := linked(t, s)
				ev := follow(t, l)
				begin := time.Now()
				ans, err := l.API.CreateRunner(t.Context(), request("r1", "small"))
				if err != nil || ans.Runner.State != hostapi.StateCreating || time.Since(begin) != 0 {
					t.Errorf("POST = %+v, %v after %s; want creating at once, before the daemon's start", ans, err, time.Since(begin))
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if got, ok := listedOver(t, l, "r1"); !ok || got.State != hostapi.StateStarting {
					t.Errorf("listed %+v, %v while the start is held; want starting", got, ok)
				}
				if _, err := l.API.Status(t.Context()); err != nil {
					t.Errorf("GET /v1/status while the start is held = %v", err)
				}
				time.Sleep(tc.hold + pendingRetry)
				synctest.Wait()
				if got, ok := listedOver(t, l, "r1"); !ok || got.State != hostapi.StateRunning || got.Reason != "" || got.RemovedBy != "" {
					t.Errorf("listed %+v, %v once the start answered; want running", got, ok)
				}
				want := []hostapi.RunnerState{hostapi.StateCreating, hostapi.StateStarting, hostapi.StateRunning}
				if got := states(ev.of("r1")); !slices.Equal(got, want) {
					t.Errorf("events for r1 = %v, want %v, never failed", got, want)
				}
				calls := d.callList()
				starts := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "start ") })
				if len(starts) != 1 || slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "remove ") }) {
					t.Errorf("calls = %q, want one start and no removal", calls)
				}
				if err := ev.open(); err != nil {
					t.Errorf("through the held start, %v", err)
				}
				select {
				case <-l.Done():
					t.Error("the link closed during the held start")
				default:
				}
			})
		})
	}
}

// The host's own removals are never a runner's error exit. Every line
// the control plane reads over the channel for a runner the host removed
// names who removed it: a DELETE's stop, whose exit carries a code, and a
// failed start's cleanup, which never lists exited.
func TestTheHostsRemovalsOverTheChannelNameWhoRemoved(t *testing.T) {
	t.Run("a DELETE of a running runner", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
			d := newFakeRunnerDaemon()
			serving(t, s, d, &fakeImages{notReady: map[string]hostapi.ProfileStatus{}})
			l := linked(t, s)
			ev := follow(t, l)
			if _, err := l.API.CreateRunner(t.Context(), request("r1", "small")); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			// The daemon reports the stop's exit as a die event, which the
			// host reads before the removal ends.
			d.mu.Lock()
			d.removing = func(context.Context, string) error {
				d.events <- "id-r1"
				for range 100 {
					if slices.ContainsFunc(ev.of("r1"), func(r hostapi.Runner) bool { return r.ExitCode != nil }) {
						break
					}
					time.Sleep(time.Second)
				}
				return nil
			}
			d.mu.Unlock()
			if _, err := l.API.DeleteRunner(t.Context(), "r1", false); err != nil {
				t.Fatalf("DELETE = %v", err)
			}
			synctest.Wait()
			lines := ev.of("r1")
			exits := slices.DeleteFunc(slices.Clone(lines), func(r hostapi.Runner) bool { return r.ExitCode == nil })
			if len(exits) == 0 {
				t.Errorf("events for r1 = %+v, want the stop's exit", lines)
			}
			for _, r := range exits {
				if r.RemovedBy != removedByDelete {
					t.Errorf("exit line %+v, want removed_by %q", r, removedByDelete)
				}
			}
			if got, ok := listedOver(t, l, "r1"); ok {
				t.Errorf("listed %+v after its DELETE", got)
			}
		})
	})
	t.Run("a failed start's cleanup", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
			d := newFakeRunnerDaemon()
			d.start = func(id string, _ int) (docker.Result, bool) {
				return docker.Result{Outcome: docker.Failed, ID: id, Err: errors.New("OCI runtime create failed")}, true
			}
			serving(t, s, d, &fakeImages{notReady: map[string]hostapi.ProfileStatus{}})
			l := linked(t, s)
			ev := follow(t, l)
			if _, err := l.API.CreateRunner(t.Context(), request("r1", "small")); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			lines := ev.of("r1")
			if len(lines) == 0 {
				t.Fatal("no events for r1")
			}
			if slices.ContainsFunc(lines, func(r hostapi.Runner) bool { return r.State == hostapi.StateExited || r.ExitCode != nil }) {
				t.Errorf("events for r1 = %+v, want none exited", lines)
			}
			if last := lines[len(lines)-1]; last.State != hostapi.StateFailed || last.RemovedBy != removedByFailed || !strings.Contains(last.Reason, "OCI runtime create failed") {
				t.Errorf("last event for r1 = %+v, want failed for the start, removed by %q", last, removedByFailed)
			}
			if !slices.Contains(d.callList(), "remove id-r1") {
				t.Errorf("calls = %q, want its container removed", d.callList())
			}
		})
	})
}

// imageDaemon is fakeRunnerDaemon with an image cache and a registry behind
// it, as one host daemon serves the image manager and the runners: a create
// from an image the cache no longer holds is refused with No such image.
type imageDaemon struct {
	*fakeRunnerDaemon
	images   map[string]docker.Image
	tags     map[string]string
	registry map[string]docker.Image
	// built is the image each runner's container was created from.
	built    map[string]string
	holdPull chan struct{}
	pulls    []time.Time
	imu      sync.Mutex
}

var _ images.Daemon = (*imageDaemon)(nil)

func newImageDaemon() *imageDaemon {
	return &imageDaemon{
		fakeRunnerDaemon: newFakeRunnerDaemon(),
		images:           map[string]docker.Image{}, tags: map[string]string{}, registry: map[string]docker.Image{}, built: map[string]string{},
	}
}

// publish makes the registry serve image id, the shipped base image, under
// ref at digest n.
func (d *imageDaemon) publish(ref, id string, n int) {
	img := docker.Image{ID: id, RepoDigests: []string{jobspec.RunnerRepo + "@" + fmt.Sprintf("sha256:%064x", n)}}
	img.Config.User = "runner"
	img.Config.Labels = map[string]string{docker.LabelRunnerImage: "base"}
	d.imu.Lock()
	defer d.imu.Unlock()
	d.registry[ref] = img
}

// forget drops image id and its tags, as a prune inside the host would.
func (d *imageDaemon) forget(id string) {
	d.imu.Lock()
	defer d.imu.Unlock()
	delete(d.images, id)
	maps.DeleteFunc(d.tags, func(_, v string) bool { return v == id })
}

func (d *imageDaemon) pullTimes() []time.Time {
	d.imu.Lock()
	defer d.imu.Unlock()
	return slices.Clone(d.pulls)
}

func (d *imageDaemon) builtFrom(name string) string {
	d.imu.Lock()
	defer d.imu.Unlock()
	return d.built[name]
}

func (d *imageDaemon) Create(ctx context.Context, name string, req *docker.CreateRequest) docker.Result {
	d.imu.Lock()
	_, held := d.images[req.Image]
	if held {
		d.built[name] = req.Image
	}
	d.imu.Unlock()
	if !held {
		return docker.Result{Outcome: docker.Failed, Err: fmt.Errorf("%w: %s", docker.ErrImageMissing, req.Image)}
	}
	return d.fakeRunnerDaemon.Create(ctx, name, req)
}

func (d *imageDaemon) InspectImage(_ context.Context, ref string) (docker.Image, bool, error) {
	d.imu.Lock()
	defer d.imu.Unlock()
	id := ref
	if t, ok := d.tags[ref]; ok {
		id = t
	}
	img, ok := d.images[id]
	return img, ok, nil
}

func (d *imageDaemon) PullImage(ctx context.Context, ref, _ string) error {
	d.imu.Lock()
	d.pulls = append(d.pulls, time.Now())
	hold := d.holdPull
	d.imu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.imu.Lock()
	defer d.imu.Unlock()
	img, ok := d.registry[ref]
	if !ok {
		return errors.New("manifest unknown")
	}
	d.images[img.ID], d.tags[ref] = img, img.ID
	return nil
}

func (d *imageDaemon) Digest(_ context.Context, ref, _ string) (string, error) {
	d.imu.Lock()
	defer d.imu.Unlock()
	_, dg, _ := strings.Cut(d.registry[ref].RepoDigests[0], "@")
	return dg, nil
}

func (*imageDaemon) LoadImage(context.Context, io.Reader) error {
	return errors.New("the fake loads no image")
}

func (d *imageDaemon) ListImages(context.Context) ([]docker.ImageSummary, error) {
	d.imu.Lock()
	defer d.imu.Unlock()
	var out []docker.ImageSummary
	for _, id := range slices.Sorted(maps.Keys(d.images)) {
		out = append(out, docker.ImageSummary{ID: id, Labels: d.images[id].Config.Labels, RepoDigests: d.images[id].RepoDigests})
	}
	return out, nil
}

// RemoveImage untags a tag, or removes an image by its ID with its tags.
func (d *imageDaemon) RemoveImage(_ context.Context, ref string) error {
	d.imu.Lock()
	_, isTag := d.tags[ref]
	delete(d.tags, ref)
	d.imu.Unlock()
	if !isTag {
		d.forget(ref)
	}
	return nil
}

// ReadFile answers a shipped image's user files: user runner, 1001:1001.
func (*imageDaemon) ReadFile(_ context.Context, _, p string, _ int64) ([]byte, bool, error) {
	switch p {
	case jobspec.PasswdPath:
		return []byte("root:x:0:0::/root:/bin/sh\nrunner:x:1001:1001::/home/runner:/bin/sh\n"), true, nil
	case jobspec.GroupPath:
		return []byte("root:x:0:\nrunner:x:1001:\n"), true, nil
	}
	return nil, false, nil
}

func (*imageDaemon) ListVolumes(context.Context) ([]docker.Volume, error) { return nil, nil }

func profileOver(t *testing.T, l *hostlink.Link, name string) hostapi.ProfileStatus {
	t.Helper()
	st, err := l.API.Status(t.Context())
	if err != nil {
		t.Fatalf("GET /v1/status = %v", err)
	}
	for _, p := range st.Profiles {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("status has no profile %s: %+v", name, st.Profiles)
	return hostapi.ProfileStatus{}
}

// A create the daemon refuses because the image in service is gone
// takes the image out of service. Over the channel, the runner fails naming
// it, status reports the profile not ready with image_missing while the
// image is pulled again at once, a start meanwhile is refused with that
// reason, and once the pull and check pass a runner starts from the new
// image.
func TestAVanishedImageOverTheChannelIsPulledAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		d := newImageDaemon()
		ref := jobspec.ImageRef(&config.Profile{RunnerImage: "base"}, testBuild)
		d.publish(ref, "sha256:a1", 1)
		m := images.New(&images.Config{
			Daemon: d, Host: s.host(), Log: s.log, Version: testBuild, StateDir: t.TempDir(), Profiles: s.served.Load().Profiles,
			Keep: images.Keep{Runner: func(string) bool { return true }, ProbeLeftover: func(string) bool { return false }},
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			m.Run(ctx)
			close(done)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
		s.images = m
		serving(t, s, d, m)
		l := linked(t, s)
		ev := follow(t, l)
		if p := profileOver(t, l, "small"); !p.Ready || p.ImageID != "sha256:a1" {
			t.Fatalf("profile small = %+v, want ready on sha256:a1", p)
		}
		d.forget("sha256:a1")
		d.publish(ref, "sha256:a2", 2)
		d.imu.Lock()
		d.holdPull = make(chan struct{})
		d.imu.Unlock()
		refusedAt := time.Now()
		if _, err := l.API.CreateRunner(t.Context(), request("r1", "small")); err != nil {
			t.Fatalf("POST r1 = %v", err)
		}
		synctest.Wait()
		lines := ev.of("r1")
		if len(lines) == 0 {
			t.Fatal("no events for r1")
		}
		if last := lines[len(lines)-1]; last.State != hostapi.StateFailed || !strings.Contains(last.Reason, "no such image") || !strings.Contains(last.Reason, "sha256:a1") {
			t.Errorf("last event for r1 = %+v, want failed for the missing sha256:a1", last)
		}
		if p := profileOver(t, l, "small"); p.Ready || p.Reason != hostapi.ReasonImageMissing || !strings.Contains(p.Failure, "sha256:a1") {
			t.Errorf("profile small = %+v while the pull is held, want not ready, image_missing, naming sha256:a1", p)
		}
		if _, err := l.API.CreateRunner(t.Context(), request("r2", "small")); !heldFor(err, hostapi.ReasonImageMissing) {
			t.Errorf("POST r2 while the pull is held = %v, want not ready, image_missing", err)
		}
		if pulls := d.pullTimes(); len(pulls) != 2 || !pulls[1].Equal(refusedAt) {
			t.Errorf("pulls at %v, want a second at once on the refusal, %v", pulls, refusedAt)
		}
		d.imu.Lock()
		close(d.holdPull)
		d.imu.Unlock()
		synctest.Wait()
		if p := profileOver(t, l, "small"); !p.Ready || p.ImageID != "sha256:a2" {
			t.Errorf("profile small = %+v after the pull, want ready on sha256:a2", p)
		}
		if _, err := l.API.CreateRunner(t.Context(), request("r3", "small")); err != nil {
			t.Fatalf("POST r3 = %v", err)
		}
		synctest.Wait()
		if got, ok := listedOver(t, l, "r3"); !ok || got.State != hostapi.StateRunning || d.builtFrom("r3") != "sha256:a2" {
			t.Errorf("r3 = %+v, %v, built from %q; want running from sha256:a2", got, ok, d.builtFrom("r3"))
		}
	})
}
