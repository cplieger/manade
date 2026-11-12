package pool

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/jobspec"
)

// bootFixture is a pool over layout before boot: Ensure finds the four
// scale sets, the registry holds the base image run as runner, and the
// daemon's cache is empty.
func bootFixture(t *testing.T) *fixture {
	t.Helper()
	fx := newUnbooted(t, load(t, layout))
	fx.gh.sets = map[string][]github.ScaleSet{"acme/app": {appDefault, appLarge}, "acme/lib": {libDefault}, "acme": {orgDefault}}
	fx.dk.registry = map[string]docker.Image{baseRef: shippedImage("sha256:base1")}
	fx.dk.files = map[string]jobspec.Files{"sha256:base1": runnerFiles}
	return fx
}

// run starts Run and returns its stop, which ends ctx and answers Run's
// error once it returns.
func (fx *fixture) run(t *testing.T) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	finished := make(chan struct{})
	var err error
	go func() {
		defer close(finished)
		err = fx.p.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
		}
	})
	return func() error {
		cancel()
		select {
		case <-finished:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after its context ended")
			return nil
		}
	}
}

// eventually waits up to 5 seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// leftoverProbe is a probe container a crashed process left, created now.
func leftoverProbe(id string) *fakeBox {
	b := &fakeBox{state: "created"}
	b.ctr.ID = id
	b.ctr.Config.Labels = map[string]string{docker.LabelManaged: "true", docker.LabelProbe: "user", docker.LabelCreated: epoch.Format(time.RFC3339)}
	return b
}

func TestRunBootsServesAndShutsDown(t *testing.T) {
	fx := bootFixture(t)
	fx.gh.runners = map[int64]github.Runner{7: {Busy: true, Online: true}}
	fx.gh.stats = map[int]github.Stats{appDefault.ID: {Assigned: 5, Running: 1}}
	fx.dk.add(leftoverProbe("c-leftover"))
	fx.dk.add(orphan("c-old", "acme/app", "default", "manade-default-old", "running"))
	stop := fx.run(t)
	eventually(t, "the pool to be healthy", fx.p.Healthy)
	eventually(t, "the boot starts", func() bool { return fx.runners(appDefault)[idle] == 3 })

	calls := fx.calls.all()
	if calls[0] != "RemoveOwned c-leftover" {
		t.Errorf("first call = %q, want the leftover probe's removal before anything else", calls[0])
	}
	at := func(call string) int { return slices.Index(calls, call) }
	if at("EnsureNetwork manade-isolated") < 0 || at("EnsureNetwork manade-isolated") > at("CheckInstallations") {
		t.Errorf("calls = %v, want the job network ensured before GitHub is read", calls)
	}
	adopted := at("Runner 7")
	if adopted < at("Ensure acme") {
		t.Errorf("calls = %v, want adoption after every scale set is ensured", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "Listen ") && slices.Index(calls, c) < adopted {
			t.Errorf("calls = %v, want every session opened after adoption", calls)
		}
	}
	if got := fx.dk.pullsOf(); !slices.Equal(got, []string{baseRef}) {
		t.Errorf("pulls = %v, want one pull of the image both profiles use", got)
	}
	// The budget holds four default runners: the adopted one and three of
	// the four assignments it does not serve.
	if got := fx.runners(appDefault); got[busy] != 1 || got[idle] != 3 {
		t.Errorf("runners = %v, want 1 adopted busy and 3 started", got)
	}

	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	calls = fx.calls.all()
	for _, ss := range []github.ScaleSet{appDefault, appLarge, libDefault, orgDefault} {
		if !slices.Contains(calls, "Close "+ss.Scope+" "+ss.Profile) {
			t.Errorf("calls = %v, want %v's session closed", calls, ss.ScaleSet)
		}
	}
	if got := fx.runningOf(); !slices.Equal(got, []string{"c-old"}) {
		t.Errorf("containers left = %v, want only the busy runner's", got)
	}
	if got := len(fx.gh.removed); got != 3 {
		t.Errorf("registrations removed = %d, want the 3 idle runners'", got)
	}
	if got := fx.capacity(appDefault); got != 0 {
		t.Errorf("capacity after shutdown = %d, want 0", got)
	}
	if fx.p.Healthy() {
		t.Error("the pool reads healthy after shutdown")
	}
}

func TestRunEndsTheBootPhaseWhenBootFinishes(t *testing.T) {
	fx := bootFixture(t)
	gate := make(chan struct{})
	fx.dk.pullGate = gate
	stop := fx.run(t)
	eventually(t, "the first pull", func() bool { return len(fx.dk.pullsOf()) == 1 })
	if !strings.Contains(scrape(t, fx.m), "\nmanade_booting 1\n") {
		t.Error("manade_booting is not 1 while boot waits on a pull")
	}
	if n := fx.rec.CountExact("manade is starting"); n != 1 {
		t.Errorf("%d start records while boot waits on a pull, want 1", n)
	}
	if n := fx.rec.CountExact("manade finished starting"); n != 0 {
		t.Errorf("%d end lines while boot waits on a pull, want none", n)
	}
	close(gate)
	eventually(t, "manade_booting 0", func() bool { return strings.Contains(scrape(t, fx.m), "\nmanade_booting 0\n") })
	if n := fx.rec.CountExact("manade finished starting"); n != 1 {
		t.Errorf("%d end lines after boot, want 1", n)
	}
	if n := fx.rec.CountExact("manade is starting"); n != 1 {
		t.Errorf("%d start records after boot, want 1", n)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestBootRefuses(t *testing.T) {
	for _, tc := range []struct {
		arrange func(fx *fixture)
		want    string
		name    string
	}{
		{name: "publicScope", want: "acme/lib is public", arrange: func(fx *fixture) { fx.gh.public = map[string]bool{"acme/lib": true} }},
		{name: "rootImage", want: "profile default", arrange: func(fx *fixture) {
			fx.dk.files = map[string]jobspec.Files{"sha256:base1": rootFiles}
		}},
		{name: "installation", want: errFake.Error(), arrange: func(fx *fixture) { fx.gh.installErr = errFake }},
		{name: "scaleSet", want: errFake.Error(), arrange: func(fx *fixture) { fx.gh.ensureErr = errFake }},
		{name: "network", want: "job network manade-isolated", arrange: func(fx *fixture) { fx.dk.networkErr = errFake }},
		{name: "dockerList", want: "listing the pool's containers", arrange: func(fx *fixture) { fx.dk.listErr = errFake }},
		{name: "unadoptedRunner", want: errUnsettled.Error(), arrange: func(fx *fixture) {
			fx.gh.runnerErr = errFake
			fx.dk.add(orphan("c-old", "acme/app", "default", "manade-default-old", "running"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := bootFixture(t)
			tc.arrange(fx)
			// A boot that wrongly succeeds serves until the deadline.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err := fx.p.Run(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Run = %v, want an error naming %q", err, tc.want)
			}
			if slices.ContainsFunc(fx.calls.all(), func(c string) bool { return strings.HasPrefix(c, "Listen ") }) {
				t.Error("a session opened after boot failed")
			}
			if !strings.Contains(scrape(t, fx.m), "\nmanade_booting 1\n") || fx.rec.CountExact("manade finished starting") != 0 {
				t.Error("a failed boot recorded its end")
			}
			// The log rules count a loop of failed starts by these records.
			if n := fx.rec.CountExact("manade is starting"); n != 1 {
				t.Errorf("a failed boot wrote %d start records, want 1", n)
			}
		})
	}

	// sized is a pool with a disk profile, whose image is its own when
	// own is set, on a registry holding only the base image.
	sized := func(t *testing.T, own bool) *fixture {
		image := ""
		if own {
			image = ", image: " + ownRef
		}
		fx := newUnbooted(t, load(t, `capacity: {cpus: 8, memory: 32GiB, disk: 100GiB}
scopes:
  - repo: acme/app
profiles:
  default: {cpus: 2, memory: 4GiB}
  sized: {cpus: 2, memory: 4GiB, disk: 10GiB`+image+`}
`))
		fx.dk.registry = map[string]docker.Image{baseRef: shippedImage("sha256:base1")}
		fx.dk.files = map[string]jobspec.Files{"sha256:base1": runnerFiles}
		return fx
	}
	t.Run("diskRefusedOnAnotherProfilesImage", func(t *testing.T) {
		fx := sized(t, true)
		fx.dk.diskErr = errFake
		if err := fx.p.boot(t.Context()); err == nil || !strings.Contains(err.Error(), "profile sized's disk limit") {
			t.Errorf("boot = %v, want the disk limit probed on the image in service and refused", err)
		}
	})
	t.Run("diskProbeSkippedWithNoImage", func(t *testing.T) {
		fx := sized(t, false)
		fx.dk.registry = nil
		fx.dk.diskErr = errFake
		if err := fx.p.boot(t.Context()); err != nil {
			t.Errorf("boot = %v, want the probe skipped with no image to create it from", err)
		}
		for _, req := range fx.dk.created {
			if req.HostConfig.StorageOpt != nil {
				t.Errorf("a disk probe was created from %q", req.Image)
			}
		}
	})
	t.Run("diskRefused", func(t *testing.T) {
		fx := sized(t, false)
		fx.dk.diskErr = errFake
		if err := fx.p.boot(t.Context()); err == nil || !strings.Contains(err.Error(), "profile sized's disk limit") {
			t.Errorf("boot = %v, want the disk limit refused naming profile sized", err)
		}
		fx.dk.diskErr = nil
		if err := fx.p.boot(t.Context()); err != nil {
			t.Errorf("boot = %v with the limit accepted, want nil", err)
		}
		if got := fx.runningOf(); len(got) != 0 {
			t.Errorf("containers left = %v, want the probes removed", got)
		}
	})
}

func TestBootServesThroughOutsideFailures(t *testing.T) {
	t.Run("visibilityReadFailed", func(t *testing.T) {
		fx := bootFixture(t)
		fx.gh.private = map[string]error{"acme/lib": errFake}
		if err := fx.p.boot(t.Context()); err != nil {
			t.Fatalf("boot = %v, want nil", err)
		}
		if fx.p.scopes["acme/lib"].private() || !fx.p.scopes["acme/app"].private() {
			t.Error("want acme/lib unfunded and acme/app proven private")
		}
		fx.gh.reads = nil
		fx.p.proofs(t.Context())
		if len(fx.gh.reads) != 0 {
			t.Errorf("reads right after boot = %v, want none: boot's reads start the 10-minute pass", fx.gh.reads)
		}
	})
	t.Run("pullFailed", func(t *testing.T) {
		fx := bootFixture(t)
		fx.dk.registry = nil
		if err := fx.p.boot(t.Context()); err != nil {
			t.Fatalf("boot = %v, want nil", err)
		}
		if got := fx.serving("default"); got != "" {
			t.Errorf("default serves %q, want its image missing", got)
		}
	})
}

func TestRunClosesAPublicScopesSessions(t *testing.T) {
	fx := bootFixture(t)
	stop := fx.run(t)
	eventually(t, "the pool to be healthy", fx.p.Healthy)
	fx.p.mu.Lock()
	fx.p.prove(fx.p.scopes["acme/lib"], false, nil, fx.at)
	fx.p.mu.Unlock()
	eventually(t, "acme/lib's session to close", func() bool { return slices.Contains(fx.calls.all(), "Close acme/lib default") })
	if !fx.p.Healthy() {
		t.Error("a public scope's closed listener makes the pool unhealthy")
	}
	if slices.ContainsFunc(fx.calls.all(), func(c string) bool { return strings.HasPrefix(c, "Close acme/app") }) {
		t.Error("another scope's session closed")
	}
	_ = stop()
}

func TestShutdownGivesUpAtItsDeadline(t *testing.T) {
	defer func(d time.Duration) { shutdownTimeout = d }(shutdownTimeout)
	shutdownTimeout = 50 * time.Millisecond
	fx := bootFixture(t)
	fx.gh.hang = make(chan struct{})
	t.Cleanup(func() { close(fx.gh.hang) })
	stop := fx.run(t)
	eventually(t, "the pool to be healthy", fx.p.Healthy)
	began := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("Run returned %v after its context ended, want about the deadline", waited)
	}
	if fx.rec.CountLevel(slog.LevelWarn, "The shutdown gave up") != 1 {
		t.Error("no line says the shutdown gave up")
	}
}

func TestHealth(t *testing.T) {
	// healthy is a booted pool whose listeners beat and whose reconciler
	// passed, now.
	healthy := func(t *testing.T) *fixture {
		fx := newFixture(t)
		for _, ss := range []github.ScaleSet{appDefault, appLarge, libDefault, orgDefault} {
			fx.p.Beat(ss)
		}
		if !fx.p.pass(t.Context()) {
			t.Fatal("pass failed")
		}
		if !fx.p.Healthy() {
			t.Fatalf("Health = %+v, want healthy", fx.p.Health())
		}
		return fx
	}
	t.Run("listenerLease", func(t *testing.T) {
		fx := healthy(t)
		fx.advance(75 * time.Second)
		fx.p.Beat(appDefault)
		fx.p.Beat(appLarge)
		fx.p.Beat(orgDefault)
		if !fx.p.Health().Listening {
			t.Error("a listener 75 seconds past its beat reads wedged")
		}
		fx.advance(time.Second)
		fx.p.Beat(appDefault)
		fx.p.Beat(appLarge)
		fx.p.Beat(orgDefault)
		if h := fx.p.Health(); h.Listening || fx.p.Healthy() {
			t.Errorf("Health = %+v with acme/lib 76 seconds past its beat, want not listening", h)
		}
	})
	t.Run("reconcilerLease", func(t *testing.T) {
		fx := healthy(t)
		fx.advance(3 * time.Minute)
		for _, ss := range []github.ScaleSet{appDefault, appLarge, libDefault, orgDefault} {
			fx.p.Beat(ss)
		}
		if !fx.p.Health().Reconciling {
			t.Error("a pass 3 minutes ago reads wedged")
		}
		fx.advance(time.Second)
		for _, ss := range []github.ScaleSet{appDefault, appLarge, libDefault, orgDefault} {
			fx.p.Beat(ss)
		}
		if h := fx.p.Health(); h.Reconciling || fx.p.Healthy() {
			t.Errorf("Health = %+v past 3 minutes since the last pass, want not reconciling", h)
		}
	})
	t.Run("startingUntilEverySessionOpened", func(t *testing.T) {
		fx := healthy(t)
		ss := scaleSet("acme/lib", "large", 9)
		fx.p.addSet(ss)
		fx.p.Beat(ss)
		if h := fx.p.Health(); h.Started || fx.p.Healthy() {
			t.Errorf("Health = %+v with a session never opened, want starting and unhealthy", h)
		}
		fx.p.Opened(ss, github.Stats{})
		fx.p.Closed(ss)
		if !fx.p.Health().Started {
			t.Error("a session that opened once and closed reads as starting")
		}
		fx.p.booting = true
		if fx.p.Health().Started {
			t.Error("a pool still booting reads started")
		}
	})
	t.Run("publicScopeLeftOut", func(t *testing.T) {
		fx := healthy(t)
		ss := scaleSet("acme/lib", "large", 9)
		fx.p.addSet(ss)
		fx.p.mu.Lock()
		fx.p.prove(fx.p.scopes["acme/lib"], false, nil, fx.at)
		fx.p.mu.Unlock()
		fx.advance(time.Minute)
		if h := fx.p.Health(); !h.Started || !h.Listening {
			t.Errorf("Health = %+v, want a public scope's listeners left out", h)
		}
	})
	t.Run("stopping", func(t *testing.T) {
		fx := healthy(t)
		fx.p.stopping = true
		if fx.p.Healthy() {
			t.Error("a stopping pool reads healthy")
		}
	})
	t.Run("failedListKeepsTheLease", func(t *testing.T) {
		fx := healthy(t)
		fx.advance(3*time.Minute + time.Second)
		fx.dk.listErr = errFake
		if fx.p.pass(t.Context()) || fx.p.Health().Reconciling {
			t.Error("a failed list refreshed the reconciler's lease")
		}
	})
}

// runningOf lists the daemon's containers.
func (fx *fixture) runningOf() []string {
	fx.dk.mu.Lock()
	defer fx.dk.mu.Unlock()
	var out []string
	for id := range fx.dk.boxes {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// pullsOf lists the daemon's pulls.
func (d *fakeDocker) pullsOf() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.pulls)
}
