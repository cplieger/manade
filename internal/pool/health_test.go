package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestHealthBeforeTheFirstGeneration(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(&Settings{Config: oneHost()}, &Deps{})
	if h := reg.Health(); h != (Health{Applied: true}) {
		t.Errorf("health %+v before Run installs a generation, want only Applied", h)
	}
}

func TestAGenerationIsBootedOnceItsBootPassRan(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	g := newGeneration(r.reg, r.settings())
	if g.health(r.now).Booted {
		t.Error("a new generation reads booted before its boot pass")
	}
	g.boot(t.Context())
	if !g.health(r.now).Booted {
		t.Error("a generation reads not booted after its boot pass")
	}
}

// The pass loop's lease is six reconciler intervals from its last pass.
func TestHealthPassingFollowsThePassLoop(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.pass()
	if h := r.reg.Health(); !h.Booted || !h.Passing {
		t.Fatalf("health %+v right after a pass, want Booted and Passing", h)
	}
	r.now = r.now.Add(6 * reconcileEvery)
	if h := r.reg.Health(); !h.Passing {
		t.Errorf("health %+v at the end of the lease, want Passing", h)
	}
	r.now = r.now.Add(time.Second)
	if h := r.reg.Health(); h.Passing || !h.Booted {
		t.Errorf("health %+v past the lease, want Booted and not Passing", h)
	}
	r.p.step(t.Context())
	if h := r.reg.Health(); !h.Passing {
		t.Errorf("health %+v after the next pass, want Passing", h)
	}
}

// A disconnected host is an upstream: its loop's lease counts only while
// it is connected.
func TestHealthReconcilingCountsOnlyConnectedHosts(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}, {name: "nas", cpus: 8, mem: 32}},
		[]profileSpec{{name: "small", hosts: []string{"default", "nas"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	r.pass()
	r.now = r.now.Add(ReconcileLease + time.Second)
	r.p.passHost(t.Context(), 0)
	if h := r.reg.Health(); h.Reconciling {
		t.Errorf("health %+v with nas connected past its lease, want not Reconciling", h)
	}
	r.hosts[1].set(func(f *fakeHost) { f.disconnected = true })
	if h := r.reg.Health(); !h.Reconciling {
		t.Errorf("health %+v with only the disconnected nas past its lease, want Reconciling", h)
	}
}

// An apply is held by its ensure past the apply lease, and the verdict
// comes back once the apply ends.
func TestHealthAppliedFollowsTheApply(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	held, release := make(chan struct{}), make(chan struct{})
	r.gh.set(func(g *fakeGitHub) {
		g.ensureHold = func() {
			held <- struct{}{}
			<-release
		}
	})
	next := cloneConfig(r.cfg)
	next.Scopes = append(next.Scopes, poolConfig(nil, nil, "acme/web").Scopes[0])
	next.Scopes[1].DefaultProfile = "small"
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.apply(next)
	}()
	<-held
	r.now = r.now.Add(applyLease)
	if h := r.reg.Health(); !h.Applied {
		t.Errorf("health %+v at the end of the apply lease, want Applied", h)
	}
	r.now = r.now.Add(time.Second)
	if h := r.reg.Health(); h.Applied {
		t.Errorf("health %+v with the apply held past its lease, want not Applied", h)
	}
	r.gh.set(func(g *fakeGitHub) { g.ensureHold = nil })
	close(release)
	<-done
	if h := r.reg.Health(); !h.Applied {
		t.Errorf("health %+v once the apply ended, want Applied", h)
	}
}

// A GitHub that never answers holds an install's ensures under one
// deadline, ensureLimit at a time, whatever the scope count: the
// generation boots with the unanswered scopes not served and the apply
// stays within its lease.
func TestEnsuresThatHangShareOneTimeout(t *testing.T) {
	old := ensureTimeout
	ensureTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ensureTimeout = old })
	r := quietRig(t, oneHost())
	next := cloneConfig(r.cfg)
	var added []string
	for i := range 12 {
		sc := poolConfig(nil, nil, fmt.Sprintf("acme/repo%d", i)).Scopes[0]
		sc.DefaultProfile = "small"
		next.Scopes = append(next.Scopes, sc)
		added = append(added, sc.Key)
	}
	r.gh.set(func(g *fakeGitHub) { g.ensureBlock = true })
	// One real ensureTimeout reads as one minute on the pool's clock.
	start := time.Now()
	scale := int64(time.Minute / ensureTimeout)
	r.reg.now = func() time.Time { return epoch.Add(time.Duration(scale * int64(time.Since(start)))) }
	done := make(chan struct{})
	notApplied := make(chan Health, 1)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
				if h := r.reg.Health(); !h.Applied {
					select {
					case notApplied <- h:
					default:
					}
				}
			}
		}
	}()
	r.apply(next)
	close(done)
	r.gh.mu.Lock()
	until, peak := slices.Clone(r.gh.blockedUntil), r.gh.blockedPeak
	r.gh.mu.Unlock()
	if len(until) != len(added) {
		t.Fatalf("%d ensures blocked, want one per added scope (%d)", len(until), len(added))
	}
	for i, dl := range until {
		if dl.IsZero() || !dl.Equal(until[0]) {
			t.Errorf("ensure %d ran under the deadline %v, want the round's one deadline %v", i, dl, until[0])
		}
	}
	if peak != ensureLimit {
		t.Errorf("%d ensures ran at once, want ensureLimit (%d)", peak, ensureLimit)
	}
	select {
	case h := <-notApplied:
		t.Errorf("health %+v during the install, want Applied throughout", h)
	default:
	}
	if r.reg.current() != r.p || !r.p.health(r.reg.now()).Booted {
		t.Error("the generation did not boot")
	}
	for _, key := range added {
		if err := r.p.scopes[key].ensureErr; !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("scope %s ensure error = %v, want the round's deadline", key, err)
		}
	}
}
