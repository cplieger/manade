package pool

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/github"
	"github.com/cplieger/manade/internal/r5/obs"
)

// scrape answers m's metrics page.
func scrape(t *testing.T, m *obs.Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

func TestStartsExactlyTheUnservedAssignments(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 3})
	if n := fx.startAll(t); n != 3 {
		t.Errorf("starts = %d, want one per assignment", n)
	}
	if n := fx.startAll(t); n != 0 {
		t.Errorf("starts = %d, want none while every assignment has a runner", n)
	}
	// A runner is live before GitHub counts its job as running.
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 4, Running: 1})
	if n := fx.startAll(t); n != 1 {
		t.Errorf("starts = %d, want 1 for the new assignment", n)
	}
	if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 8000 {
		t.Errorf("reserved = %d milli-CPUs, want four runners' 8000", got)
	}
}

// GitHub's count drops only with a JobCompleted, which can come a poll
// after the runner's container exits.
func TestAnEndedRunnerCountsUntilGitHubsCountDrops(t *testing.T) {
	// ended starts one runner for one assignment, has it take the job, and
	// ends its container before any JobCompleted.
	ended := func(t *testing.T, took bool) (*fixture, string) {
		fx := newFixture(t)
		name, _ := fx.startOne(t, appDefault)
		if took {
			fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, RunID: 1, Name: "job"})
		}
		fx.dk.exit(name, 0, false)
		fx.reconcile(t)
		if fx.hasRecord(appDefault, name) {
			t.Fatal("the exited runner's record is kept")
		}
		return fx, name
	}

	t.Run("noStartBeforeTheJobCompleted", func(t *testing.T) {
		fx, name := ended(t, true)
		fx.advance(spentFor - time.Second)
		if n := fx.startAll(t); n != 0 {
			t.Errorf("starts = %d before the JobCompleted, want 0 for a job that ended", n)
		}
		fx.polled(appDefault, fx.at, &github.Stats{}, github.Event{Kind: github.Completed, Runner: name, RunID: 1, Name: "job", Result: "succeeded"})
		if n := fx.startAll(t); n != 0 {
			t.Errorf("starts = %d after the JobCompleted, want 0 with nothing assigned", n)
		}
	})

	t.Run("theJobCompletedFreesTheCount", func(t *testing.T) {
		fx, name := ended(t, true)
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1},
			github.Event{Kind: github.Completed, Runner: name, RunID: 1, Name: "job", Result: "succeeded"}, assigned(2, "next"))
		if n := fx.startAll(t); n != 1 {
			t.Errorf("starts = %d, want 1 for the next assignment", n)
		}
	})

	t.Run("aJobCompletedBeforeTheExitHoldsNothing", func(t *testing.T) {
		fx := newFixture(t)
		name, _ := fx.startOne(t, appDefault)
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1},
			github.Event{Kind: github.Completed, Runner: name, RunID: 1, Name: "job", Result: "succeeded"}, assigned(2, "next"))
		fx.dk.exit(name, 0, false)
		fx.reconcile(t)
		if n := fx.startAll(t); n != 1 {
			t.Errorf("starts = %d, want 1 for the next assignment once the finished runner is gone", n)
		}
	})

	t.Run("startedAgainAfterAPollWithNoMessage", func(t *testing.T) {
		fx, _ := ended(t, false)
		fx.advance(spentFor - time.Second)
		if n := fx.startAll(t); n != 0 {
			t.Errorf("starts = %d within a poll of the exit, want 0", n)
		}
		fx.advance(time.Second)
		if n := fx.startAll(t); n != 1 {
			t.Errorf("starts = %d a poll after an exit GitHub still counts, want 1", n)
		}
	})
}

func TestStartRegistersThenCreatesFromTheCheckedImage(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.startAll(t)
	if len(fx.gh.jits) != 1 || len(fx.dk.created) != 1 || len(fx.dk.started) != 1 {
		t.Fatalf("jits, creates, starts = %d, %d, %d; want one each", len(fx.gh.jits), len(fx.dk.created), len(fx.dk.started))
	}
	name, req := fx.gh.jits[0], fx.dk.created[0]
	if !strings.HasPrefix(name, "manade-default-") || len(name) != len("manade-default-")+12 {
		t.Errorf("runner name = %q, want the pool, the profile and 12 hex digits", name)
	}
	if req.Image != "sha256:default" {
		t.Errorf("image = %q, want the checked image ID", req.Image)
	}
	if !slices.Equal(req.Env, []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=test-jit-config"}) {
		t.Errorf("env = %q, want the JIT config alone", req.Env)
	}
	if req.Labels[docker.LabelRunnerID] != "1" || req.Labels[docker.LabelRunner] != name {
		t.Errorf("labels = %v, want the runner's name and registration ID", req.Labels)
	}
	if fx.dk.started[0] != "c-"+name {
		t.Errorf("started %q, want the created container", fx.dk.started[0])
	}
	if got := fx.runners(appDefault); got[idle] != 1 {
		t.Errorf("runners = %v, want one idle", got)
	}
	if !strings.Contains(scrape(t, fx.m), `manade_runner_starts_total{profile="default",result="ok",scope="acme/app"} 1`) {
		t.Error("the start is not counted as ok")
	}
	for _, r := range fx.rec.Records() {
		if strings.Contains(r.Message, "test-jit-config") || strings.Contains(fmtAttrs(r), "test-jit-config") {
			t.Errorf("a log line carries the JIT config: %q", r.Message)
		}
	}
	if n := fx.rec.CountLevel(slog.LevelInfo, "Runner started"); n != 1 {
		t.Errorf("start lines = %d, want 1", n)
	}
}

func fmtAttrs(r slog.Record) string {
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(a.String())
		return true
	})
	return b.String()
}

func TestAFailedStartCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name                string
		jit, create, start  error
		unregistered        []int64
		removed             []string
		dockerErrorFragment string
	}{
		{name: "jit", jit: errFake},
		{name: "create", create: errFake, unregistered: []int64{1}, dockerErrorFragment: `op="create"} 1`},
		{name: "start", start: errFake, unregistered: []int64{1}, removed: []string{"c-"}, dockerErrorFragment: `op="start"} 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.gh.jitErr, fx.dk.createErr, fx.dk.startErr = tc.jit, tc.create, tc.start
			fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
			fx.startAll(t)
			if !slices.Equal(fx.gh.removed, tc.unregistered) {
				t.Errorf("registrations removed = %v, want %v", fx.gh.removed, tc.unregistered)
			}
			if len(fx.dk.removed) != len(tc.removed) || (len(tc.removed) == 1 && !strings.HasPrefix(fx.dk.removed[0], tc.removed[0])) {
				t.Errorf("containers removed = %v, want %v", fx.dk.removed, tc.removed)
			}
			if got := fx.runners(appDefault); len(got) != 0 {
				t.Errorf("runners = %v, want the record dropped", got)
			}
			if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
				t.Errorf("reserved = %d, want the reservation freed", got)
			}
			page := scrape(t, fx.m)
			if !strings.Contains(page, `manade_runner_starts_total{profile="default",result="error",scope="acme/app"} 1`) {
				t.Error("the start is not counted as an error")
			}
			if tc.dockerErrorFragment != "" && !strings.Contains(page, tc.dockerErrorFragment) {
				t.Errorf("no Docker error %s", tc.dockerErrorFragment)
			}
			if n := fx.startAll(t); n != 0 {
				t.Errorf("starts = %d, want none until the retry", n)
			}
			fx.advance(startRetry)
			if n := fx.startAll(t); n != 1 {
				t.Errorf("starts after %v = %d, want the retry", startRetry, n)
			}
		})
	}
}

// countingGitHub tracks how many JIT calls run at once.
type countingGitHub struct {
	*fakeGitHub
	mu       sync.Mutex
	now, top int
}

func (g *countingGitHub) JIT(ctx context.Context, ss github.ScaleSet, name string) (github.JIT, error) {
	g.mu.Lock()
	g.now++
	g.top = max(g.top, g.now)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.now--
		g.mu.Unlock()
	}()
	return g.fakeGitHub.JIT(ctx, ss, name)
}

func TestScaleReturnsAtOnceForABatch(t *testing.T) {
	big := strings.Replace(layout, "capacity: {cpus: 8, memory: 32GiB}", "capacity: {cpus: 128, memory: 256GiB}", 1)
	fx := newFixtureFrom(t, load(t, big), appDefault, appLarge, libDefault, orgDefault)
	gate := make(chan struct{})
	gh := &countingGitHub{fakeGitHub: &fakeGitHub{jitGate: gate}}
	fx.p.gh = gh
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { fx.p.runStarts(ctx); close(done) }()
	events := make([]github.Event, 50)
	for i := range events {
		events[i] = assigned(int64(i), "job")
	}
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 50}, events...)
	gh.fakeGitHub.mu.Lock()
	finished := len(gh.jits)
	gh.fakeGitHub.mu.Unlock()
	if finished != 0 {
		t.Errorf("JITs finished before Polled returned = %d, want 0", finished)
	}
	// With every JIT held, the worker must stop at its limit.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		gh.mu.Lock()
		n := gh.now
		gh.mu.Unlock()
		if n >= startsAtOnce || time.Now().After(deadline) {
			break
		}
	}
	for until := time.Now().Add(200 * time.Millisecond); time.Now().Before(until); time.Sleep(5 * time.Millisecond) {
		gh.mu.Lock()
		n := gh.now
		gh.mu.Unlock()
		if n > startsAtOnce {
			t.Fatalf("starts at once = %d, want at most %d", n, startsAtOnce)
		}
	}
	close(gate)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		fx.dk.mu.Lock()
		n := len(fx.dk.started)
		fx.dk.mu.Unlock()
		if n == 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("started %d of 50 runners", n)
		}
	}
	cancel()
	<-done
	if gh.top > startsAtOnce {
		t.Errorf("starts at once = %d, want at most %d", gh.top, startsAtOnce)
	}
}

func TestShutdownDropsUnstartedRunners(t *testing.T) {
	// More starts than run at once, so some wait for a turn at shutdown.
	big := strings.Replace(layout, "capacity: {cpus: 8, memory: 32GiB}", "capacity: {cpus: 64, memory: 128GiB}", 1)
	fx := newFixtureFrom(t, load(t, big), appDefault, appLarge, libDefault, orgDefault)
	fx.gh.jitGate = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { fx.p.runStarts(ctx); close(done) }()
	n := 2 * startsAtOnce
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: n})
	for deadline := time.Now().Add(10 * time.Second); fx.runners(appDefault)[starting] != n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the worker never took the starts")
		}
	}
	cancel()
	<-done
	if got := fx.runners(appDefault); len(got) != 0 {
		t.Errorf("runners = %v, want every unstarted record dropped", got)
	}
	if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
		t.Errorf("reserved = %d, want every reservation freed", got)
	}
}

func TestNoStartWithoutAnImageOrAtShutdown(t *testing.T) {
	t.Run("imageMissing", func(t *testing.T) {
		fx := newFixture(t)
		fx.p.setImage(fx.p.byName["default"], "")
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
		if n := fx.startAll(t); n != 0 {
			t.Errorf("starts = %d, want none without a checked image", n)
		}
	})
	t.Run("stopping", func(t *testing.T) {
		fx := newFixture(t)
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
		fx.p.stopping = true
		if n := fx.startAll(t); n != 0 {
			t.Errorf("starts = %d, want none at shutdown", n)
		}
	})
}

// startHook runs fn inside StartContainer, before the start returns.
type startHook struct {
	*fakeDocker
	fn func()
}

func (d *startHook) StartContainer(ctx context.Context, id string) error {
	d.fn()
	return d.fakeDocker.StartContainer(ctx, id)
}

func TestAJobStartedDuringTheStartStaysBusy(t *testing.T) {
	fx := newFixture(t)
	fx.p.dk = &startHook{fakeDocker: fx.dk, fn: func() {
		fx.gh.mu.Lock()
		name := fx.gh.jits[len(fx.gh.jits)-1]
		fx.gh.mu.Unlock()
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, RunID: 1, Name: "lint"})
	}}
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.startAll(t)
	if got := fx.runners(appDefault); got[busy] != 1 {
		t.Errorf("runners = %v, want the runner busy", got)
	}
}

func TestAStartCutByShutdownIsNoFailure(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, r := range fx.p.owed() {
		fx.p.start(ctx, r)
	}
	if strings.Contains(scrape(t, fx.m), `result="error",scope="acme/app"} 1`) {
		t.Error("a start cut by shutdown is counted as an error")
	}
	if n := fx.rec.CountLevel(slog.LevelWarn, "so the start is retried"); n != 0 {
		t.Errorf("retry WARN lines = %d, want none at shutdown", n)
	}
	if got := fx.runners(appDefault); len(got) != 0 {
		t.Errorf("runners = %v, want the record dropped", got)
	}
}
