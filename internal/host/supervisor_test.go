package host

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/go/health/v2"
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
