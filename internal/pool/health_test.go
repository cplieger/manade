package pool

import (
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
