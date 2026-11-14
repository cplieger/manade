package host

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/images"
	"github.com/cplieger/manade/internal/host/probe"
	"github.com/cplieger/manade/internal/hostapi"
)

func TestHealthFollowsTheGuardsCheck(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "manade-host.healthy")
	st := health.NewStatus(health.WithMarker(marker), health.WithLogger(slog.New(slog.DiscardHandler)))
	var pingErr error
	s := &supervisor{opts: &Options{Health: st}, ping: func(context.Context) error { return pingErr }}
	exists := func() bool { _, err := os.Stat(marker); return err == nil }
	t.Run("unhealthy before the API serves", func(t *testing.T) {
		s.onChecked()
		if st.Healthy() || exists() {
			t.Error("healthy before the API serves")
		}
	})
	s.serving.Store(true)
	t.Run("healthy once a check passes with the API serving and the daemon answering", func(t *testing.T) {
		s.onChecked()
		if !st.Healthy() || !exists() {
			t.Error("unhealthy after a passing check")
		}
	})
	t.Run("unhealthy while the daemon does not answer", func(t *testing.T) {
		pingErr = errors.New("dockerd gone")
		s.onChecked()
		if st.Healthy() || exists() {
			t.Error("healthy with the daemon gone")
		}
	})
	t.Run("each passing check refreshes a marker the lease would call stale", func(t *testing.T) {
		pingErr = nil
		s.onChecked()
		old := time.Now().Add(-2 * HealthLease.Duration())
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}
		s.onChecked()
		st, err := os.Stat(marker)
		if err != nil || time.Since(st.ModTime()) > HealthLease.Duration() {
			t.Errorf("marker %v, %v: a check left it stale", st, err)
		}
	})
	t.Run("the lease is three checks and one slow check", func(t *testing.T) {
		if got := HealthLease.Duration(); got != 3*checkInterval+30*time.Second {
			t.Errorf("lease = %s", got)
		}
	})
}

func TestLatencies(t *testing.T) {
	l := newLatencies()
	l.observe("create", 40*time.Millisecond)
	l.observe("create", 3*time.Second)
	l.observe("start", 45*time.Second)
	l.observe("inspect", time.Second)
	got := l.snapshot()
	if len(got) != 2 || got[0].Call != "create" || got[1].Call != "start" {
		t.Fatalf("snapshot = %+v, want create and start only", got)
	}
	t.Run("cumulative buckets", func(t *testing.T) {
		c := got[0]
		// 0.05 holds the 40 ms call; 5 and up hold both.
		if c.Count != 2 || c.Buckets[0] != 1 || c.Buckets[5] != 1 || c.Buckets[6] != 2 || c.Buckets[len(c.Buckets)-1] != 2 {
			t.Errorf("create = %+v", c)
		}
		if s := got[1]; s.Count != 1 || s.Buckets[8] != 0 || s.Buckets[9] != 1 || s.Sum != 45 {
			t.Errorf("start = %+v", s)
		}
	})
	t.Run("a snapshot is a copy", func(t *testing.T) {
		got[0].Buckets[0] = 99
		if l.snapshot()[0].Buckets[0] != 1 {
			t.Error("the snapshot shares the histogram")
		}
	})
	if len(hostapi.LatencyBuckets) != len(got[0].Buckets) {
		t.Error("a bucket per bound")
	}
}

// reloadSupervisor is a supervisor of host lan, brought up on the fake
// kernel, whose SIGHUP reads cfg.
func reloadSupervisor(t *testing.T, cfg func() (*config.Config, error)) *supervisor {
	t.Helper()
	return reloadSupervisorOf(t, section(), cfg)
}

// reloadSupervisorOf is reloadSupervisor brought up with section h.
func reloadSupervisorOf(t *testing.T, h *config.Host, cfg func() (*config.Config, error)) *supervisor {
	t.Helper()
	f := newUpFixture(t)
	n, err := bringUp(t.Context(), h, f.platform(), guardHooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.stop() })
	log := slog.New(slog.DiscardHandler)
	s := &supervisor{log: log, net: n, changed: make(chan struct{}, 1), opts: &Options{Log: log, Reload: func(context.Context) (*config.Config, error) { return cfg() }}}
	s.prober = probe.NewRunner(&probe.Config{Log: log})
	sv := s.newServed(lanConfig(h, testProfile("small", 1)), h)
	s.served.Store(sv)
	s.images = images.New(&images.Config{Host: section(), Log: log, Profiles: sv.Profiles})
	s.runners = newRunners(t.Context(), &runnersConfig{Served: sv, Images: s.images, Log: log})
	return s
}

func lanConfig(h *config.Host, profiles ...config.Profile) *config.Config {
	for i := range profiles {
		profiles[i].Hosts = []string{h.Name}
	}
	return &config.Config{File: "/config/config.yaml", Self: &config.Self{Name: h.Name, TLSDir: "/tls"}, Hosts: []config.Host{*h}, Profiles: profiles}
}

func TestSupervisorReload(t *testing.T) {
	t.Run("a config that does not validate keeps everything and names why", func(t *testing.T) {
		s := reloadSupervisor(t, func() (*config.Config, error) { return nil, errors.New("profiles.small.memory: not a size") })
		before := s.served.Load()
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		if msg := s.configErr.Load(); msg == nil || !strings.Contains(*msg, "profiles.small.memory") || s.served.Load() != before {
			t.Errorf("configErr %v, served swapped %v", msg, s.served.Load() != before)
		}
	})
	t.Run("a config without the host's section keeps everything", func(t *testing.T) {
		other := section()
		other.Name = "other"
		s := reloadSupervisor(t, func() (*config.Config, error) { return lanConfig(other), nil })
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		if msg := s.configErr.Load(); msg == nil || !strings.Contains(*msg, "hosts.lan") {
			t.Errorf("configErr = %v", msg)
		}
	})
	t.Run("a section the running host cannot apply keeps everything", func(t *testing.T) {
		h := section()
		h.Egress.IPv6 = true
		s := reloadSupervisor(t, func() (*config.Config, error) { return lanConfig(h), nil })
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		if msg := s.configErr.Load(); msg == nil || !strings.Contains(*msg, "egress.ipv6") {
			t.Errorf("configErr = %v", msg)
		}
	})
	t.Run("a valid config swaps what the host serves and clears the kept reason", func(t *testing.T) {
		var bad bool
		s := reloadSupervisor(t, func() (*config.Config, error) {
			if bad {
				return nil, errors.New("broken")
			}
			return lanConfig(section("10.0.0.50/32"), testProfile("small", 1), testProfile("large", 2)), nil
		})
		bad = true
		_ = s.reload(t.Context())
		bad = false
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		sv := s.served.Load()
		if s.configErr.Load() != nil || len(sv.Profiles) != 2 || len(s.host().Egress.Allow) != 1 {
			t.Errorf("configErr %v, %d profiles, allow %v", s.configErr.Load(), len(sv.Profiles), s.host().Egress.Allow)
		}
		if st := s.images.Status(); len(st) != 2 {
			t.Errorf("images serve %d profiles, want both", len(st))
		}
	})
}

// changing runs s's posture hold and its change loop until the test ends.
func changing(t *testing.T, s *supervisor) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := s.net.hold(ctx); err != nil {
			t.Errorf("Hold = %v", err)
		}
	})
	wg.Go(func() { s.changes(ctx, cancel) })
	t.Cleanup(func() {
		cancel(nil)
		wg.Wait()
	})
}

// settled waits until no change is pending and the posture applies h's
// open and internet keys.
func settled(t *testing.T, s *supervisor, h *config.Host) {
	t.Helper()
	waitFor(t, "the change applied", func() bool {
		s.reloadMu.Lock()
		defer s.reloadMu.Unlock()
		return s.pending == nil && !s.net.postureChanged(h)
	})
}

func TestSupervisorReloadChangesThePosture(t *testing.T) {
	open := openSection()
	for name, tc := range map[string]struct{ from, to *config.Host }{
		"open to closed":        {open, section()},
		"internet on to off":    {section(), closedSection()},
		"internet off to on":    {closedSection(), section()},
		"closed to open":        {section(), open},
		"internet off and open": {closedSection(), open},
	} {
		t.Run(name, func(t *testing.T) {
			s := reloadSupervisorOf(t, tc.from, func() (*config.Config, error) { return lanConfig(tc.to, testProfile("small", 1)), nil })
			changing(t, s)
			before := egress.PolicyHash(s.host())
			if err := s.reload(t.Context()); err != nil {
				t.Fatal(err)
			}
			settled(t, s, tc.to)
			if msg := s.configErr.Load(); msg != nil {
				t.Fatalf("kept: %s", *msg)
			}
			if after := egress.PolicyHash(s.host()); after == before || after != egress.PolicyHash(tc.to) {
				t.Errorf("policy hash %s after the change, want the new section's %s", after, egress.PolicyHash(tc.to))
			}
			if (s.net.gitHubStatus() == nil) != tc.to.Egress.Internet {
				t.Errorf("learning GitHub's addresses %v, want %v", s.net.gitHubStatus() != nil, !tc.to.Egress.Internet)
			}
			if st := s.runners.profileStatus(); len(st) != 1 || !st[0].Ready && st[0].Reason != hostapi.ReasonPulling {
				t.Errorf("status %+v, want no egress hold once the change is applied", st)
			}
		})
	}
}

// A SIGHUP that changes open or internet starts nothing while a runner
// runs, then rebuilds the egress once it has ended.
func TestSupervisorReloadWaitsForTheRunners(t *testing.T) {
	s := reloadSupervisorOf(t, openSection(), func() (*config.Config, error) { return lanConfig(section(), testProfile("small", 1)), nil })
	changing(t, s)
	s.runners.mu.Lock()
	s.runners.ctrs["r1"] = &hostapi.Container{Name: "r1", State: docker.StateRunning}
	s.runners.mu.Unlock()
	if err := s.reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	st := s.runners.profileStatus()
	if len(st) != 1 || st[0].Ready || st[0].Reason != hostapi.ReasonEgressChangePending || !strings.Contains(st[0].Detail, "r1") {
		t.Errorf("status %+v, want small not ready while r1 runs", st)
	}
	if _, err := s.runners.accept(t.Context(), request("r2", "small")); err == nil {
		t.Error("a POST was taken while the change waits")
	}
	time.Sleep(50 * time.Millisecond)
	if !s.host().Egress.Open {
		t.Fatal("the egress changed with r1 running")
	}
	s.runners.mu.Lock()
	s.runners.ctrs["r1"].State = docker.StateExited
	s.runners.changed("r1")
	s.runners.mu.Unlock()
	settled(t, s, section())
	if s.host().Egress.Open || s.net.section().Egress.Open {
		t.Error("the host is still open once r1 ended")
	}
}

func TestSupervisorReloadAfterAPendingChange(t *testing.T) {
	t.Run("a section back on the running posture drops the change", func(t *testing.T) {
		next := lanConfig(section(), testProfile("small", 1))
		s := reloadSupervisorOf(t, openSection(), func() (*config.Config, error) { return next, nil })
		s.runners.mu.Lock()
		s.runners.ctrs["r1"] = &hostapi.Container{Name: "r1", State: docker.StateRunning}
		s.runners.mu.Unlock()
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		next = lanConfig(openSection(), testProfile("small", 1))
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s.pending != nil || s.runners.egressIdle() != nil {
			t.Error("the dropped change still holds starts")
		}
	})
	t.Run("a change woken by a dropped hold waits for the new hold's runners", func(t *testing.T) {
		next := lanConfig(section(), testProfile("small", 1))
		s := reloadSupervisorOf(t, openSection(), func() (*config.Config, error) { return next, nil })
		s.runners.mu.Lock()
		s.runners.ctrs["r1"] = &hostapi.Container{Name: "r1", State: docker.StateRunning}
		s.runners.mu.Unlock()
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		old := s.runners.egressIdle()
		next = lanConfig(openSection(), testProfile("small", 1))
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		next = lanConfig(section(), testProfile("small", 1))
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !closed(old) {
			t.Fatal("setup: the dropped hold's channel is open")
		}
		if err := s.applyChange(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !s.net.section().Egress.Open || s.pending == nil {
			t.Error("the change was applied with r1 still running under the new hold")
		}
	})
	t.Run("a change that fails holds starts and names why", func(t *testing.T) {
		s := reloadSupervisorOf(t, section(), func() (*config.Config, error) { return lanConfig(closedSection(), testProfile("small", 1)), nil })
		s.net.seams.FetchDomains = func(context.Context) ([]byte, error) { return nil, errors.New("403 rate limit exceeded") }
		changing(t, s)
		if err := s.reload(t.Context()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the change failed", func() bool { return s.configErr.Load() != nil })
		if msg := *s.configErr.Load(); !strings.Contains(msg, "rate limit") {
			t.Errorf("configErr = %q, want the failure", msg)
		}
		st := s.runners.profileStatus()
		if len(st) != 1 || st[0].Reason != hostapi.ReasonEgressChangeFailed || !strings.Contains(st[0].Detail, "rate limit") {
			t.Errorf("status %+v, want small held naming the failure", st)
		}
		if !s.host().Egress.Internet || !s.net.section().Egress.Internet {
			t.Error("a failed change left the new section served")
		}
	})
}
