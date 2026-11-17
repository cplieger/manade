package pool

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

func TestAGenerationChangeKeepsABusyRunnerAndItsJob(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.assign(b, 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "a", RunID: 1})
	cfg := cloneConfig(r.cfg)
	cfg.Profiles[0].MilliCPUs = 1000
	r.apply(cfg)
	r.pass()
	rec, ok := r.record(r.small(), name)
	if !ok || rec.state != busy || !rec.stale {
		t.Fatalf("the busy runner after its profile was resized is %+v, %v; want kept busy and stale", rec, ok)
	}
	if r.gh.wasRemoved(rec.id) || len(r.hosts[0].running()) != 1 {
		t.Error("a generation change removed a runner on a job")
	}
	if got := r.m.generations; len(got) != 2 || got[1].rev != 2 || got[1].cfg != cfg {
		t.Errorf("generations recorded %v, want revisions 1 and 2 with their settings", got)
	}
}

// The idle runner covers an assignment GitHub has not started, so a
// retired one is replaced by a runner built for the new spec.
func TestAnIdleRunnerIsReplacedOnlyWhenItsProfileChanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		edit    func(cpu *int64, runners *int)
		name    string
		retired bool
	}{
		{name: "resized", edit: func(cpu *int64, _ *int) { *cpu = 1000 }, retired: true},
		{name: "maxRunnersOnly", edit: func(_ *int64, runners *int) { *runners = 5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, oneHost())
			r.assign(r.small(), 1, 0)
			r.pass()
			old := r.hosts[0].running()[0]
			cfg := cloneConfig(r.cfg)
			tc.edit(&cfg.Profiles[0].MilliCPUs, &cfg.Profiles[0].MaxRunners)
			r.apply(cfg)
			r.pass()
			got := r.hosts[0].running()
			if len(got) != 1 {
				t.Fatalf("running %v after the change, want one runner for the assignment", got)
			}
			if replaced := got[0] != old; replaced != tc.retired {
				t.Errorf("idle runner replaced %v after the change, want %v", replaced, tc.retired)
			}
		})
	}
}

func TestAHostServesOnlyTheSectionOfTheCurrentRevision(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	cfg := cloneConfig(r.cfg)
	cfg.HeadReserveAfter = time.Minute
	r.apply(cfg, "default")
	r.assign(r.small(), 1, 0)
	if r.hosts[0].posts != 0 {
		t.Fatal("a host still running the previous revision's section got a start")
	}
	r.hosts[0].set(func(f *fakeHost) { f.section = sectionHash(r.rev) })
	r.pass()
	if r.hosts[0].posts != 1 {
		t.Errorf("POSTs = %d once the host runs the new section, want 1", r.hosts[0].posts)
	}
}

func TestAFailedEnsureBootsTheGenerationAndIsRetried(t *testing.T) {
	t.Parallel()
	cfg := oneHost()
	r := &rig{t: t, now: epoch, cfg: cfg, gh: newFakeGitHub(cfg), log: &logBuf{}, rev: 1, settler: &fakeSettler{}, records: newRecords(), sessions: &fakeSessions{t: t}, runs: newFakeRuns(), hostLog: newFakeHostLog()}
	r.gh.ensureFail = true
	r.side = sideOf(r.gh)
	r.hosts = []*fakeHost{newFakeHost(cfg, "default", sectionHash(1))}
	r.links = &fakeLinks{hosts: r.hosts}
	r.restart()
	if r.small() != nil {
		t.Fatal("a scale set exists although its ensure failed")
	}
	if h := r.reg.Health(); !h.Booted {
		t.Errorf("health %+v with GitHub unreachable at boot, want the generation booted", h)
	}
	r.gh.set(func(g *fakeGitHub) { g.ensureFail = false })
	r.pass()
	if r.small() == nil {
		t.Fatal("the retry did not ensure the scale set")
	}
	r.assign(r.small(), 1, 0)
	if r.hosts[0].posts != 1 {
		t.Errorf("POSTs = %d once the ensure succeeded, want 1", r.hosts[0].posts)
	}
}

func TestADisconnectedHostIsNotServedAndKeepsItsRunners(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	name := r.hosts[0].running()[0]
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = true })
	r.pass()
	if _, ok := r.record(r.small(), name); !ok {
		t.Fatal("a disconnected host's runner lost its record")
	}
	if r.serving("default") {
		t.Error("a disconnected host serves")
	}
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = false })
	r.pass()
	if !r.serving("default") {
		t.Error("the host does not serve once it reconnects")
	}
}

// recordingGitHub is fakeGitHub whose Listen records each session it
// opens and closes.
type recordingGitHub struct {
	*fakeGitHub
	open   map[setKey]int
	opened int
	mu     sync.Mutex
}

func (g *recordingGitHub) Listen(ctx context.Context, _ *config.Scope, ss github.ScaleSet, _ string, _ github.Handler) {
	k := setKey{ss.Scope, ss.Profile}
	g.mu.Lock()
	g.open[k]++
	g.opened++
	g.mu.Unlock()
	<-ctx.Done()
	g.mu.Lock()
	g.open[k]--
	g.mu.Unlock()
}

func (g *recordingGitHub) sessions() (open, opened int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.open {
		open += n
	}
	return open, g.opened
}

func TestRunKeepsSessionsAcrossARevisionAndClosesThemAtShutdown(t *testing.T) {
	cfg := oneHost()
	gh := &recordingGitHub{fakeGitHub: newFakeGitHub(cfg), open: map[setKey]int{}}
	h := newFakeHost(cfg, "default", sectionHash(1))
	m := newFakeMetrics()
	side := &GitHub{ScaleSets: gh, REST: gh, Sweep: gh}
	reg := NewRegistry(&Settings{Config: cfg, GitHub: side, Revision: 1, Sections: map[string]string{"default": sectionHash(1)}}, &Deps{
		Links: &fakeLinks{hosts: []*fakeHost{h}}, Metrics: m, Runs: newFakeRuns(), Hosts: newFakeHostLog(), Settle: &fakeSettler{},
		Records: newRecords(), Routes: newRecords(), Log: slog.New(slog.DiscardHandler),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.Run(ctx)
	}()
	waitFor(t, "the first session", func() bool { open, _ := gh.sessions(); return open == 1 })
	next := cloneConfig(cfg)
	next.HeadReserveAfter = time.Minute
	reg.Apply(&Settings{Config: next, GitHub: side, Revision: 2, Sections: map[string]string{"default": sectionHash(2)}})
	waitFor(t, "revision 2 serving", func() bool { return reg.Snapshot().Revision == 2 })
	if open, opened := gh.sessions(); open != 1 || opened != 1 {
		t.Errorf("sessions open %d, opened %d after a revision that keeps the scale set; want the one session kept", open, opened)
	}
	cancel()
	<-done
	if open, _ := gh.sessions(); open != 0 {
		t.Errorf("sessions open %d after the shutdown, want 0", open)
	}
}

// waitFor polls cond until it holds, failing the test after 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
