package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/github"
)

// startOne has ss assigned one job and starts its runner, returning the
// runner's name and registration ID.
func (fx *fixture) startOne(t *testing.T, ss github.ScaleSet) (string, int64) {
	t.Helper()
	fx.p.mu.Lock()
	s := fx.p.set(ss)
	n := s.asg.stats.Assigned + 1
	before := maps.Clone(s.runners)
	fx.p.mu.Unlock()
	fx.polled(ss, fx.at, &github.Stats{Assigned: n}, assigned(int64(n), "job"))
	if got := fx.startAll(t); got != 1 {
		t.Fatalf("started %d runners, want 1", got)
	}
	fx.p.mu.Lock()
	defer fx.p.mu.Unlock()
	for name, r := range s.runners {
		if before[name] == nil {
			return name, r.id
		}
	}
	t.Fatal("no new runner")
	return "", 0
}

// take has runner name of ss take a job, as GitHub's JobStarted says.
func (fx *fixture) take(ss github.ScaleSet, name string) {
	fx.polled(ss, fx.at, nil, github.Event{Kind: github.Started, Runner: name, RunID: 1, Name: "job"})
}

func (fx *fixture) reconcile(t *testing.T) {
	t.Helper()
	if listed, _ := fx.p.reconcile(t.Context()); !listed {
		t.Fatal("reconcile failed")
	}
}

func (fx *fixture) hasRecord(ss github.ScaleSet, name string) bool {
	fx.p.mu.Lock()
	defer fx.p.mu.Unlock()
	_, ok := fx.p.set(ss).runners[name]
	return ok
}

func exits(reason string) string {
	return `manade_runner_exits_total{profile="default",reason="` + reason + `",scope="acme/app"} 1`
}

func TestReconcileExits(t *testing.T) {
	for _, tc := range []struct {
		name, reason    string
		code            int
		oom, done, dead bool
	}{
		{name: "cleanAfterItsJob", reason: "completed", done: true},
		{name: "cleanWithNoJobCompleted", reason: "completed"},
		{name: "failed", reason: "failed", code: 1, done: true},
		{name: "oomKilled", reason: "oom", code: 137, oom: true, done: true},
		// Docker leaves a container dead when its removal failed midway.
		{name: "dead", reason: "failed", code: 1, done: true, dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			name, id := fx.startOne(t, appDefault)
			if tc.done {
				fx.polled(appDefault, fx.at, nil, github.Event{Kind: github.Completed, Runner: name, RunID: 1, Name: "job", Result: "succeeded"})
			}
			fx.dk.box(name).logs = []string{"line one", "line two"}
			fx.dk.exit(name, tc.code, tc.oom)
			if tc.dead {
				fx.dk.box(name).state = "dead"
			}
			fx.reconcile(t)
			if fx.dk.box(name) != nil {
				t.Error("the exited container is still there")
			}
			if fx.hasRecord(appDefault, name) {
				t.Error("the runner's record is still held")
			}
			if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
				t.Errorf("reserved = %d milli-CPUs, want 0", got)
			}
			if !strings.Contains(scrape(t, fx.m), exits(tc.reason)) {
				t.Errorf("no %s exit counted", tc.reason)
			}
			// A registration no JobCompleted named may linger offline.
			if want := !tc.done; slices.Contains(fx.gh.removed, id) != want {
				t.Errorf("registration removed = %v, want %v", !want, want)
			}
			unclean := tc.code != 0 || tc.oom
			if got := slices.Equal(fx.dk.logsAsked, []int{50}); got != unclean {
				t.Errorf("log tail asked = %v, want 50 lines only for an unclean exit", fx.dk.logsAsked)
			}
			if unclean && !fx.rec.AttrContains("A runner exited with an error", "tail", "line one\nline two") {
				t.Error("the WARN line does not carry the log tail")
			}
			if fx.rec.CountLevel(slog.LevelInfo, "Container exited") != 1 {
				t.Error("no exit line")
			}
		})
	}
}

func TestReconcileRetriesAFailedRemoval(t *testing.T) {
	fx := newFixture(t)
	name, _ := fx.startOne(t, appDefault)
	fx.dk.exit(name, 0, false)
	fx.dk.removeErr = map[string]error{"c-" + name: errFake}
	fx.reconcile(t)
	if !fx.hasRecord(appDefault, name) {
		t.Fatal("the record went with its container still there")
	}
	if !strings.Contains(scrape(t, fx.m), `manade_docker_errors_total{op="remove"} 1`) {
		t.Error("no remove error counted")
	}
	fx.dk.removeErr = nil
	fx.reconcile(t)
	if fx.hasRecord(appDefault, name) || fx.dk.box(name) != nil {
		t.Error("the next pass did not remove the exited runner")
	}
}

func TestReconcileSavesDiagnostics(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	fx.p.diag = root
	clean, _ := fx.startOne(t, appDefault)
	failed, _ := fx.startOne(t, appDefault)
	none, _ := fx.startOne(t, appDefault)
	big, _ := fx.startOne(t, appDefault)
	fx.dk.box(clean).diag = []byte("clean archive")
	fx.dk.box(failed).diag = []byte("failed archive")
	fx.dk.box(big).diag = bytes.Repeat([]byte("x"), 8<<20+1)
	fx.dk.exit(clean, 0, false)
	fx.dk.exit(failed, 1, false)
	fx.dk.exit(none, 0, false)
	fx.dk.exit(big, 0, false)
	fx.reconcile(t)
	for name, want := range map[string]string{clean: "clean archive", failed: "failed archive"} {
		if got, err := os.ReadFile(filepath.Join(dir, name+".tar")); err != nil || string(got) != want {
			t.Errorf("%s.tar = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, none+".tar")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a runner with no _diag left a file: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, big+".tar")); err != nil || info.Size() != 8<<20 {
		t.Errorf("the oversized archive was not cut at 8 MiB: %v", err)
	}
	if !strings.Contains(scrape(t, fx.m), `manade_docker_errors_total{op="archive"} 1`) {
		t.Error("the oversized archive counted no archive error")
	}

	t.Run("prunedAfterRetention", func(t *testing.T) {
		old := filepath.Join(dir, clean+".tar")
		stale := fx.at.Add(-fx.p.cfg.DiagRetention - time.Minute)
		if err := os.Chtimes(old, stale, stale); err != nil {
			t.Fatal(err)
		}
		fx.reconcile(t)
		if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("diagnostics past DIAG_RETENTION are kept: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, failed+".tar")); err != nil {
			t.Errorf("recent diagnostics went: %v", err)
		}
	})
}

func TestReconcileRemovesAtMaxLifetime(t *testing.T) {
	fx := newFixture(t)
	name, id := fx.startOne(t, appDefault)
	fx.take(appDefault, name)
	fx.advance(2 * time.Hour)
	fx.reconcile(t)
	if !fx.hasRecord(appDefault, name) {
		t.Fatal("removed at exactly max_lifetime")
	}
	fx.advance(time.Second)
	fx.reconcile(t)
	if fx.hasRecord(appDefault, name) || fx.dk.box(name) != nil {
		t.Fatal("a runner past max_lifetime is kept")
	}
	if !strings.Contains(scrape(t, fx.m), exits("lifetime")) {
		t.Error("no lifetime exit counted")
	}
	if !slices.Contains(fx.gh.removed, id) {
		t.Error("the registration of a runner cut mid-job is kept")
	}
	if n := fx.startAll(t); n != 0 {
		t.Errorf("starts = %d, want none while GitHub still counts the cut job", n)
	}
}

func TestReconcileRemovesIdleRunners(t *testing.T) {
	t.Run("registrationFirst", func(t *testing.T) {
		fx := newFixture(t)
		name, id := fx.startOne(t, appDefault)
		busyName, _ := fx.startOne(t, appDefault)
		fx.take(appDefault, busyName)
		fx.advance(10 * time.Minute)
		fx.reconcile(t)
		if !fx.hasRecord(appDefault, name) {
			t.Fatal("removed at exactly idle_timeout")
		}
		fx.advance(time.Second)
		fx.reconcile(t)
		want := []string{"RemoveRunner " + strconv.FormatInt(id, 10), "RemoveOwned c-" + name}
		if got := fx.calls.all(); !slices.Equal(got[len(got)-2:], want) {
			t.Errorf("calls = %v, want the registration removed before the container", got)
		}
		if fx.hasRecord(appDefault, name) {
			t.Error("the idle runner's record is kept")
		}
		if !fx.hasRecord(appDefault, busyName) {
			t.Error("a busy runner was removed as idle")
		}
		if !strings.Contains(scrape(t, fx.m), exits("idle")) {
			t.Error("no idle exit counted")
		}
		// The assignment it was started for is still GitHub's to hand out.
		if n := fx.startAll(t); n != 1 {
			t.Errorf("starts = %d, want 1 for the assignment no runner holds", n)
		}
	})

	t.Run("busyOnGitHub", func(t *testing.T) {
		fx := newFixture(t)
		name, id := fx.startOne(t, appDefault)
		fx.gh.removeErr = map[int64]error{id: github.ErrRunnerBusy}
		fx.advance(11 * time.Minute)
		fx.reconcile(t)
		if fx.dk.box(name) == nil || !fx.hasRecord(appDefault, name) {
			t.Fatal("a runner GitHub says is busy was removed")
		}
		if got := fx.runners(appDefault); got[busy] != 1 {
			t.Errorf("runners = %v, want it counted busy", got)
		}
	})

	t.Run("otherFailureRetried", func(t *testing.T) {
		fx := newFixture(t)
		name, id := fx.startOne(t, appDefault)
		fx.gh.removeErr = map[int64]error{id: errFake}
		fx.advance(11 * time.Minute)
		fx.reconcile(t)
		if fx.dk.box(name) == nil {
			t.Fatal("the container went while its registration stayed")
		}
		if got := fx.runners(appDefault); got[idle] != 1 {
			t.Errorf("runners = %v, want it still idle", got)
		}
		fx.gh.removeErr = nil
		fx.dk.removeErr = map[string]error{"c-" + name: errFake}
		fx.reconcile(t)
		if !fx.hasRecord(appDefault, name) {
			t.Fatal("the record went with its container still there")
		}
		fx.dk.removeErr = nil
		fx.reconcile(t)
		if fx.hasRecord(appDefault, name) || fx.dk.box(name) != nil {
			t.Error("the retried removal did not finish")
		}
	})
}

func TestReconcileClosesLostRecords(t *testing.T) {
	fx := newFixture(t)
	name, id := fx.startOne(t, appDefault)
	fx.dk.mu.Lock()
	delete(fx.dk.boxes, "c-"+name)
	fx.dk.mu.Unlock()
	fx.reconcile(t)
	if fx.hasRecord(appDefault, name) {
		t.Error("a record whose container vanished is kept")
	}
	if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
		t.Errorf("reserved = %d milli-CPUs, want 0", got)
	}
	if !strings.Contains(scrape(t, fx.m), exits("lost")) {
		t.Error("no lost exit counted")
	}
	if !slices.Contains(fx.gh.removed, id) {
		t.Error("the lost runner's registration is kept")
	}
	if n := fx.startAll(t); n != 0 {
		t.Errorf("starts = %d, want none while GitHub still counts its job", n)
	}
}

// listHook runs fn once, after ListContainers builds its answer and
// before it returns.
type listHook struct {
	*fakeDocker
	fn func()
}

func (d *listHook) ListContainers(ctx context.Context) ([]docker.Summary, error) {
	list, err := d.fakeDocker.ListContainers(ctx)
	if fn := d.fn; fn != nil {
		d.fn = nil
		fn()
	}
	return list, err
}

func TestReconcileKeepsARunnerStartedAfterTheList(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.p.dk = &listHook{fakeDocker: fx.dk, fn: func() {
		if n := fx.startAll(t); n != 1 {
			t.Fatalf("started %d runners, want 1", n)
		}
	}}
	fx.reconcile(t)
	if got := fx.runners(appDefault); got[idle] != 1 {
		t.Errorf("runners = %v, want the new runner kept", got)
	}
	if len(fx.gh.removed) != 0 {
		t.Errorf("registrations removed = %v, want none for a running runner", fx.gh.removed)
	}
	if strings.Contains(scrape(t, fx.m), exits("lost")) {
		t.Error("a runner the list could not see is counted lost")
	}
	if n := fx.startAll(t); n != 0 {
		t.Errorf("starts = %d, want no second runner for the assignment", n)
	}
}

func TestReconcileLeavesAStartInFlight(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	r := fx.p.owed()[0]
	fx.dk.mu.Lock()
	fx.dk.add(orphan("c-"+r.name, "acme/app", "default", r.name, "created"))
	fx.dk.mu.Unlock()
	fx.advance(3 * time.Hour)
	fx.reconcile(t)
	if fx.dk.box(r.name) == nil || !fx.hasRecord(appDefault, r.name) {
		t.Error("the pass touched a runner whose start is in flight")
	}
	if len(fx.calls.all()) != 0 {
		t.Errorf("calls = %v, want none", fx.calls.all())
	}
}

// orphan is a runner container no record holds, created at epoch with the
// default profile's limits, registered as runner 7.
func orphan(id, scope, profile, name, state string) *fakeBox {
	b := &fakeBox{state: state}
	b.ctr.ID, b.ctr.Image = id, "sha256:old"
	b.ctr.Config.Labels = map[string]string{
		docker.LabelManaged: "true", docker.LabelScope: scope, docker.LabelProfile: profile, docker.LabelRunner: name,
		docker.LabelRunnerID: "7", docker.LabelCreated: epoch.Format(time.RFC3339),
	}
	b.ctr.HostConfig.NanoCPUs, b.ctr.HostConfig.Memory = 2_000_000_000, 4<<30
	return b
}

func TestReconcileAdoptsOrphans(t *testing.T) {
	for _, tc := range []struct {
		runner                         *github.Runner
		readErr, removeErr, inspectErr error
		name                           string
		adopted, kept                  bool
		unregistered                   bool
		// unsettled is a runner that may hold a job left with no record.
		unsettled bool
	}{
		{name: "busy", runner: &github.Runner{Busy: true}, adopted: true, kept: true},
		{name: "idle", runner: &github.Runner{}, unregistered: true},
		{name: "registrationGone"},
		{name: "readFailed", readErr: errFake, kept: true, unsettled: true},
		{name: "inspectFailed", runner: &github.Runner{Busy: true}, inspectErr: errFake, kept: true, unsettled: true},
		{name: "idleUnregisterFailed", runner: &github.Runner{}, removeErr: errFake, kept: true, unsettled: true},
		// GitHub handed the idle orphan a job between the read and the removal.
		{name: "idleTookAJob", runner: &github.Runner{}, removeErr: github.ErrRunnerBusy, adopted: true, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			if tc.runner != nil {
				fx.gh.runners = map[int64]github.Runner{7: *tc.runner}
			}
			fx.gh.runnerErr = tc.readErr
			fx.gh.removeErr = map[int64]error{7: tc.removeErr}
			fx.dk.inspectErr = tc.inspectErr
			fx.dk.mu.Lock()
			fx.dk.add(orphan("c-old", "acme/app", "default", "manade-default-old", "running"))
			fx.dk.mu.Unlock()
			if listed, settled := fx.p.reconcile(t.Context()); !listed || settled == tc.unsettled {
				t.Errorf("reconcile = listed %v, settled %v; want settled %v", listed, settled, !tc.unsettled)
			}
			if got := fx.hasRecord(appDefault, "manade-default-old"); got != tc.adopted {
				t.Errorf("adopted = %v, want %v", got, tc.adopted)
			}
			if got := fx.dk.box("old") != nil; got != tc.kept {
				t.Errorf("container kept = %v, want %v", got, tc.kept)
			}
			if got := slices.Contains(fx.gh.removed, 7); got != tc.unregistered {
				t.Errorf("registration removed = %v, want %v", got, tc.unregistered)
			}
			want := int64(0)
			if tc.adopted {
				want = 2000
			}
			if got := fx.p.Snapshot().Reserved.MilliCPUs; got != want {
				t.Errorf("reserved = %d milli-CPUs, want %d from its inspected limits", got, want)
			}
		})
	}

	t.Run("adoptedRunnerKeepsItsAge", func(t *testing.T) {
		fx := newFixture(t)
		fx.gh.runners = map[int64]github.Runner{7: {Busy: true}}
		fx.dk.mu.Lock()
		fx.dk.add(orphan("c-old", "acme/app", "default", "manade-default-old", "running"))
		fx.dk.mu.Unlock()
		fx.advance(2*time.Hour + time.Second)
		fx.reconcile(t)
		fx.reconcile(t)
		if fx.dk.box("old") != nil {
			t.Error("an adopted runner created past max_lifetime ago is kept")
		}
	})

	t.Run("notTheConfigs", func(t *testing.T) {
		fx := newFixture(t)
		fx.dk.mu.Lock()
		fx.dk.add(orphan("c-gone", "acme/gone", "default", "manade-default-gone", "running"))
		probe := orphan("c-probe", "", "", "", "created")
		probe.ctr.Config.Labels = map[string]string{docker.LabelManaged: "true", docker.LabelProbe: "user"}
		fx.dk.add(probe)
		fx.dk.mu.Unlock()
		fx.reconcile(t)
		if fx.dk.box("gone") == nil || fx.dk.box("probe") == nil {
			t.Error("the pass removed a container of a scope the config does not name, or a probe")
		}
		if len(fx.calls.all()) != 0 {
			t.Errorf("calls = %v, want none", fx.calls.all())
		}
	})
}

// A runner whose scale set the config dropped is drained like any orphan,
// and a busy one holds its reservation inside the budget until it exits.
func TestReconcileDrainsRunnersTheConfigDropped(t *testing.T) {
	for _, tc := range []struct {
		runner        *github.Runner
		readErr       error
		name, scope   string
		held, kept    bool
		unregistered  bool
		unsettled     bool
		noGitHubCalls bool
	}{
		{name: "removedProfileBusy", scope: "acme/app", runner: &github.Runner{Busy: true}, held: true, kept: true},
		{name: "removedProfileIdle", scope: "acme/app", runner: &github.Runner{}, unregistered: true},
		{name: "removedProfileOffline", scope: "acme/app", runner: &github.Runner{}, unregistered: true},
		{name: "removedProfileRegistrationGone", scope: "acme/app"},
		{name: "removedProfileReadFailed", scope: "acme/app", readErr: errFake, kept: true, unsettled: true},
		{name: "removedScope", scope: "acme/gone", held: true, kept: true, noGitHubCalls: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			if tc.runner != nil {
				fx.gh.runners = map[int64]github.Runner{7: *tc.runner}
			}
			fx.gh.runnerErr = tc.readErr
			fx.dk.mu.Lock()
			fx.dk.add(orphan("c-old", tc.scope, "retired", "manade-retired-old", "running"))
			fx.dk.mu.Unlock()
			for range 2 {
				if listed, settled := fx.p.reconcile(t.Context()); !listed || settled == tc.unsettled {
					t.Errorf("reconcile = listed %v, settled %v; want settled %v", listed, settled, !tc.unsettled)
				}
			}
			if got := fx.dk.box("old") != nil; got != tc.kept {
				t.Errorf("container kept = %v, want %v", got, tc.kept)
			}
			if got := slices.Contains(fx.gh.removed, 7); got != tc.unregistered {
				t.Errorf("registration removed = %v, want %v", got, tc.unregistered)
			}
			if tc.noGitHubCalls && len(fx.calls.all()) != 0 {
				t.Errorf("calls = %v, want none for a scope with no credentials", fx.calls.all())
			}
			want, slots := int64(0), 4
			if tc.held {
				want, slots = 2000, 3
			}
			if got := fx.p.Snapshot().Reserved.MilliCPUs; got != want {
				t.Errorf("reserved = %d milli-CPUs, want %d from its inspected limits", got, want)
			}
			fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 4, 0)})
			if got := fx.capacity(appDefault); got != slots {
				t.Errorf("capacity = %d, want %d default slots left in the budget", got, slots)
			}
			if n := fx.rec.CountLevel(slog.LevelWarn, "scope is no longer configured"); n != b2i(tc.scope == "acme/gone") {
				t.Errorf("WARN lines for an unconfigured scope = %d over two passes, want one only for it", n)
			}
		})
	}

	// held puts a busy runner of a profile the config dropped under a stray record.
	held := func(t *testing.T) *fixture {
		fx := newFixture(t)
		fx.gh.runners = map[int64]github.Runner{7: {Busy: true}}
		fx.dk.mu.Lock()
		fx.dk.add(orphan("c-old", "acme/app", "retired", "manade-retired-old", "running"))
		fx.dk.mu.Unlock()
		fx.reconcile(t)
		return fx
	}
	t.Run("anExitFreesItsReservation", func(t *testing.T) {
		fx := held(t)
		fx.dk.exit("old", 0, false)
		fx.reconcile(t)
		if fx.dk.box("old") != nil {
			t.Error("the exited container is kept")
		}
		if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
			t.Errorf("reserved = %d milli-CPUs after the exit, want 0", got)
		}
		if !slices.Contains(fx.gh.removed, 7) {
			t.Error("the exited runner's registration is kept, though no JobCompleted can name it")
		}
	})
	t.Run("itPassesTheDefaultLifetime", func(t *testing.T) {
		fx := held(t)
		fx.advance(config.DefaultMaxLifetime)
		fx.reconcile(t)
		if fx.dk.box("old") == nil {
			t.Fatal("removed at exactly max_lifetime")
		}
		fx.advance(time.Second)
		fx.reconcile(t)
		if fx.dk.box("old") != nil {
			t.Error("a runner past max_lifetime is kept")
		}
		if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
			t.Errorf("reserved = %d milli-CPUs after the removal, want 0", got)
		}
	})
	t.Run("itCountsAgainstItsProfilesMaxRunners", func(t *testing.T) {
		capped := strings.Replace(layout, "large: {cpus: 4,", "large: {max_runners: 1, cpus: 4,", 1)
		fx := newFixtureFrom(t, load(t, capped), appDefault, appLarge, libDefault, orgDefault)
		fx.gh.runners = map[int64]github.Runner{7: {Busy: true}}
		fx.dk.mu.Lock()
		fx.dk.add(orphan("c-old", "acme/lib", "large", "manade-large-old", "running"))
		fx.dk.mu.Unlock()
		fx.reconcile(t)
		fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("large", 1, "big", 0)}})
		if got := fx.capacity(appLarge); got != 0 {
			t.Errorf("large capacity = %d, want 0 while a large runner outside its scopes holds its one slot", got)
		}
		// A new session's assignments are backed only as far as the cap allows.
		fx.p.Opened(appLarge, github.Stats{Assigned: 1})
		<-fx.p.kick
		if n := fx.startAll(t); n != 0 {
			t.Errorf("starts = %d for an assignment past large's max_runners, want 0", n)
		}
	})
	t.Run("aVanishedContainerFreesItsReservation", func(t *testing.T) {
		fx := held(t)
		fx.dk.mu.Lock()
		delete(fx.dk.boxes, "c-old")
		fx.dk.mu.Unlock()
		fx.reconcile(t)
		if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
			t.Errorf("reserved = %d milli-CPUs after the container vanished, want 0", got)
		}
		if !slices.Contains(fx.gh.removed, 7) {
			t.Error("the vanished runner's registration is kept, though no JobCompleted can name it")
		}
	})
	// An exit before any pass took the runner, or a vanished container,
	// removes the registration too, where the scope still has a client.
	for _, tc := range []struct {
		name, scope string
		vanish      bool
	}{
		{name: "anExitBeforeAnyPassIsUnregistered", scope: "acme/app"},
		{name: "anExitBeforeAnyPassInAScopeThatLeftCallsNoGitHub", scope: "acme/gone"},
		{name: "aVanishedRunnerInAScopeThatLeftCallsNoGitHub", scope: "acme/gone", vanish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.gh.runners = map[int64]github.Runner{7: {Busy: true}}
			fx.dk.mu.Lock()
			fx.dk.add(orphan("c-old", tc.scope, "retired", "manade-retired-old", "running"))
			fx.dk.mu.Unlock()
			if tc.vanish {
				fx.reconcile(t)
				fx.dk.mu.Lock()
				delete(fx.dk.boxes, "c-old")
				fx.dk.mu.Unlock()
			} else {
				fx.dk.exit("old", 0, false)
			}
			fx.reconcile(t)
			if fx.dk.box("old") != nil {
				t.Error("the exited container is kept")
			}
			if got, want := slices.Contains(fx.gh.removed, 7), tc.scope == "acme/app"; got != want {
				t.Errorf("registration removed = %v, want %v", got, want)
			}
			runnerCalls := slices.DeleteFunc(fx.calls.all(), func(c string) bool {
				return !strings.HasPrefix(c, "Runner ") && !strings.HasPrefix(c, "RemoveRunner ")
			})
			if tc.scope == "acme/gone" && len(runnerCalls) != 0 {
				t.Errorf("GitHub calls = %v, want none for a scope with no credentials", runnerCalls)
			}
			if got := fx.p.Snapshot().Reserved.MilliCPUs; got != 0 {
				t.Errorf("reserved = %d milli-CPUs once it ended, want 0", got)
			}
		})
	}
}

func TestReconcileListCutShortByShutdownIsNoFailure(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	fx.dk.listErr = context.Canceled
	fx.p.dk = &listHook{fakeDocker: fx.dk, fn: cancel}
	fx.p.pass(ctx)
	if got := dockerErrors(t, fx, "list"); got != "" && got != "0" {
		t.Errorf("list errors = %s for a list shutdown cut short, want none", got)
	}
	if n := fx.rec.CountLevel(slog.LevelWarn, "Listing the pool's containers failed"); n != 0 {
		t.Errorf("WARN lines = %d for a list shutdown cut short, want none", n)
	}
}

// cancelHook cancels the pass inside the GitHub call it names and answers
// the cancellation, as a call shutdown cuts short does.
type cancelHook struct {
	*fakeGitHub
	cancel         context.CancelFunc
	runner, remove bool
}

func (g *cancelHook) Runner(ctx context.Context, s *config.Scope, id int64) (github.Runner, bool, error) {
	if g.runner {
		g.cancel()
		return github.Runner{}, false, ctx.Err()
	}
	return g.fakeGitHub.Runner(ctx, s, id)
}

func (g *cancelHook) RemoveRunner(ctx context.Context, scope string, id int64) error {
	if g.remove {
		g.cancel()
		return ctx.Err()
	}
	return g.fakeGitHub.RemoveRunner(ctx, scope, id)
}

func TestReconcileGitHubCallsCutShortByShutdownLogNothing(t *testing.T) {
	for _, tc := range []struct {
		setup          func(*testing.T, *fixture)
		name           string
		runner, remove bool
	}{
		{name: "retiringAnIdleRunner", remove: true, setup: func(t *testing.T, fx *fixture) {
			fx.startOne(t, appDefault)
			fx.advance(10*time.Minute + time.Second)
		}},
		{name: "readingAnOrphan", runner: true, setup: func(_ *testing.T, fx *fixture) {
			fx.dk.mu.Lock()
			fx.dk.add(orphan("c-old", "acme/app", "default", "manade-default-old", "running"))
			fx.dk.mu.Unlock()
		}},
		{name: "deregisteringAnIdleOrphan", remove: true, setup: func(_ *testing.T, fx *fixture) {
			fx.gh.runners = map[int64]github.Runner{7: {}}
			fx.dk.mu.Lock()
			fx.dk.add(orphan("c-old", "acme/app", "default", "manade-default-old", "running"))
			fx.dk.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			tc.setup(t, fx)
			ctx, cancel := context.WithCancel(t.Context())
			hook := &cancelHook{fakeGitHub: fx.gh, cancel: cancel, runner: tc.runner, remove: tc.remove}
			fx.p.gh, fx.p.rest = hook, hook
			fx.p.reconcile(ctx)
			if ctx.Err() == nil {
				t.Fatal("the pass made no call the hook cuts short")
			}
			if n := fx.rec.CountLevel(slog.LevelWarn, ""); n != 0 {
				t.Errorf("WARN lines = %v for a call shutdown cut short, want none", fx.rec.Messages())
			}
		})
	}
}

func TestReconcileListFailure(t *testing.T) {
	fx := newFixture(t)
	fx.dk.listErr = errFake
	fx.p.pass(t.Context())
	if !strings.Contains(scrape(t, fx.m), `manade_docker_errors_total{op="list"} 1`) {
		t.Error("no list error counted")
	}
	fx.p.mu.Lock()
	passed := fx.p.passed
	fx.p.mu.Unlock()
	if !passed.IsZero() {
		t.Errorf("a failed pass refreshed the lease to %v", passed)
	}
	fx.dk.listErr = nil
	fx.p.pass(t.Context())
	fx.p.mu.Lock()
	passed = fx.p.passed
	fx.p.mu.Unlock()
	if !passed.Equal(fx.at) {
		t.Errorf("lease = %v after a good pass, want %v", passed, fx.at)
	}
}

// diesOnce answers one die event, then holds the stream.
type diesOnce struct {
	*fakeDocker
	sent bool
}

func (d *diesOnce) Events(ctx context.Context, fn func(string)) error {
	if !d.sent {
		d.sent = true
		fn("c-any")
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestRunReconcilerPassesOnADieEvent(t *testing.T) {
	fx := newFixture(t)
	name, _ := fx.startOne(t, appDefault)
	fx.dk.exit(name, 0, false)
	fx.p.dk = &diesOnce{fakeDocker: fx.dk}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { fx.p.runReconciler(ctx); close(done) }()
	for deadline := time.Now().Add(10 * time.Second); fx.dk.box(name) != nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no pass followed the die event")
		}
	}
	cancel()
	<-done
}
