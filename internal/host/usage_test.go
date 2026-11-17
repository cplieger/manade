package host

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/hostapi"
)

// fakeCgroups is each runner's cgroup by name: the working set a sample
// reads, the figures its end reads, and the removals asked for.
type fakeCgroups struct {
	ws      map[string]int64
	ended   map[string]platform.RunnerEnd
	present map[string]bool
	// busy refuses each removal of the names it holds.
	busy    map[string]bool
	reads   map[string]int
	removed []string
	mu      sync.Mutex
}

var _ usageCgroups = (*fakeCgroups)(nil)

func newFakeCgroups() *fakeCgroups {
	return &fakeCgroups{
		ws: map[string]int64{}, ended: map[string]platform.RunnerEnd{}, present: map[string]bool{},
		busy: map[string]bool{}, reads: map[string]int{},
	}
}

func (f *fakeCgroups) WorkingSet(name string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws, ok := f.ws[name]
	if ok {
		f.reads[name]++
	}
	return ws, ok, nil
}

func (f *fakeCgroups) Ended(name string) (platform.RunnerEnd, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.ended[name]
	return e, ok, nil
}

func (f *fakeCgroups) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[name] {
		return errors.New("rmdir: device or resource busy")
	}
	f.removed = append(f.removed, name)
	delete(f.present, name)
	return nil
}

func (f *fakeCgroups) Names() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.present {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

// set gives runner name's cgroup working set ws and answers how many reads
// of it the sampler has made.
func (f *fakeCgroups) set(name string, ws int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ws[name], f.present[name] = ws, true
	return f.reads[name]
}

func (f *fakeCgroups) readsOf(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[name]
}

func (f *fakeCgroups) end(name string, e platform.RunnerEnd) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended[name], f.present[name] = e, true
	delete(f.ws, name)
}

func (f *fakeCgroups) removals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

// sampled runs runner name at each working set in turn, each read by the
// sampler at least once.
func (rt *runnersTest) sampled(t *testing.T, name string, ws ...int64) {
	t.Helper()
	for _, w := range ws {
		before := rt.cg.set(name, w)
		waitFor(t, "a sample of "+name, func() bool { return rt.cg.readsOf(name) > before })
	}
}

// An exited runner reports the CPU time its cgroup kept and the highest
// working set sampled while it ran; the kernel's watermark stands in only
// for an OOM kill or a runner never sampled, and a runner with no cgroup
// reports none.
func TestAnExitedRunnerReportsItsUsage(t *testing.T) {
	end := platform.RunnerEnd{CPU: 90 * time.Second, Watermark: 512 * mib}
	for _, tc := range []struct {
		name    string
		samples []int64
		cgroup  bool
		oom     bool
		peak    int64
	}{
		{name: "sampled", samples: []int64{100 * mib, 300 * mib, 200 * mib}, cgroup: true, peak: 300 * mib},
		{name: "oom", samples: []int64{140 * mib}, cgroup: true, oom: true, peak: 512 * mib},
		{name: "never sampled", cgroup: true, peak: 512 * mib},
		{name: "no cgroup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRunnersTest(t, nil, nil)
			if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
				t.Fatal(err)
			}
			rt.waitState(t, "r1", hostapi.StateRunning)
			rt.sampled(t, "r1", tc.samples...)
			if tc.cgroup {
				rt.cg.end("r1", end)
			}
			rt.d.exit("r1", 0, tc.oom)
			run := rt.waitState(t, "r1", hostapi.StateExited)
			if !tc.cgroup {
				if run.Usage != nil {
					t.Errorf("usage = %+v, want none for a runner with no cgroup", run.Usage)
				}
				return
			}
			if run.Usage == nil {
				t.Fatal("an exited runner with a cgroup reports no usage")
			}
			ran := run.ExitedAt.Sub(run.StartedAt)
			if want := (hostapi.Usage{CPU: 90 * time.Second, Ran: ran, PeakMemory: tc.peak}); *run.Usage != want {
				t.Errorf("usage = %+v, want %+v", *run.Usage, want)
			}
		})
	}
}

// The exit's line on the events stream carries the usage, so a client that
// only follows the stream has it.
func TestTheExitEventCarriesTheUsage(t *testing.T) {
	rt := newRunnersTest(t, nil, nil)
	ch := rt.r.subscribe(t.Context())
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	rt.waitState(t, "r1", hostapi.StateRunning)
	rt.sampled(t, "r1", 64*mib)
	rt.cg.end("r1", platform.RunnerEnd{CPU: time.Second, Watermark: 128 * mib})
	rt.d.exit("r1", 1, false)
	for {
		select {
		case run := <-ch:
			if run.State != hostapi.StateExited {
				continue
			}
			if run.Usage == nil || run.Usage.CPU != time.Second || run.Usage.PeakMemory != 64*mib {
				t.Errorf("the exit event's usage = %+v, want 1 s and 64 MiB", run.Usage)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("no exit event")
		}
	}
}

// A runner's cgroup goes with its DELETE, and the list's pass removes any
// other cgroup no record or container holds, warning once while one cannot
// be removed.
func TestRunnerCgroupsAreRemoved(t *testing.T) {
	var logs bytes.Buffer
	rt := newRunnersTest(t, nil, nil)
	rt.r.meter.log = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := rt.r.accept(t.Context(), request("r1", "small")); err != nil {
		t.Fatal(err)
	}
	rt.waitState(t, "r1", hostapi.StateRunning)
	rt.sampled(t, "r1", 64*mib)
	t.Run("at the runner's DELETE", func(t *testing.T) {
		if _, err := rt.r.remove(t.Context(), "r1", false); err != nil {
			t.Fatal(err)
		}
		if got := rt.cg.removals(); !slices.Equal(got, []string{"r1"}) {
			t.Errorf("removals = %q, want r1's", got)
		}
	})
	t.Run("a stray at the list's pass, a held one kept", func(t *testing.T) {
		if _, err := rt.r.accept(t.Context(), request("r2", "small")); err != nil {
			t.Fatal(err)
		}
		rt.waitState(t, "r2", hostapi.StateRunning)
		rt.cg.set("r2", mib)
		rt.cg.set("stray", mib)
		rt.cg.mu.Lock()
		rt.cg.busy["stuck"], rt.cg.present["stuck"] = true, true
		rt.cg.mu.Unlock()
		rt.r.sync(t.Context())
		rt.r.sync(t.Context())
		if got := rt.cg.removals(); !slices.Equal(got, []string{"r1", "stray"}) {
			t.Errorf("removals = %q, want r1's then the stray's, never r2's", got)
		}
		if n := strings.Count(logs.String(), "stuck"); n != 1 {
			t.Errorf("the cgroup that cannot be removed was warned of %d times, want once:\n%s", n, logs.String())
		}
	})
}
