package host

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/images"
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

func TestAPIPort(t *testing.T) {
	for _, tc := range []struct {
		addr config.Address
		want uint16
		ok   bool
	}{
		{config.Address{Socket: "/run/manade/hosts/nas/api.sock"}, 0, true},
		{config.Address{HostPort: "0.0.0.0:7464"}, 7464, true},
		{config.Address{HostPort: "[::]:7464"}, 7464, true},
		{config.Address{HostPort: "nas:http"}, 0, false},
	} {
		got, err := apiPort(tc.addr)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("apiPort(%+v) = %d, %v", tc.addr, got, err)
		}
	}
}

// reloadSupervisor is a supervisor of host lan, brought up on the fake
// kernel, whose SIGHUP reads cfg.
func reloadSupervisor(t *testing.T, cfg func() (*config.Config, error)) *supervisor {
	t.Helper()
	f := newUpFixture(t)
	n, err := bringUp(t.Context(), section(), f.platform(), guardHooks{})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	s := &supervisor{log: log, net: n, opts: &Options{Log: log, Reload: func(context.Context) (*config.Config, error) { return cfg() }}}
	sv := s.newServed(lanConfig(section(), testProfile("small", 1)), section())
	s.served.Store(sv)
	s.runners = newRunners(&runnersConfig{Served: sv, Log: log})
	s.images = images.New(&images.Config{Host: section(), Log: log, Profiles: sv.Profiles})
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
