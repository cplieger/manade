package store

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

// failCommits has the stores the test opens fail every commit while the
// answered flag is set, and sets writeLease to lease.
func failCommits(t *testing.T, lease time.Duration) *atomic.Bool {
	t.Helper()
	var failing atomic.Bool
	oldFault, oldLease := commitFault, writeLease
	commitFault = func(context.Context) error {
		if failing.Load() {
			return errors.New("test: the disk is full")
		}
		return nil
	}
	writeLease = lease
	t.Cleanup(func() { commitFault, writeLease = oldFault, oldLease })
	return &failing
}

func TestWriteHealth(t *testing.T) {
	failing := failCommits(t, 0)
	s := mustOpen(t, testPath(t), "v1")
	defer mustClose(t, s)
	route := &Route{Scope: "acme/app", Variable: "MANADE_RUNNER", Value: "small"}
	if !s.Healthy() || s.WriteFailures() != 0 {
		t.Fatalf("Healthy %v, failures %d at open, want healthy with none", s.Healthy(), s.WriteFailures())
	}
	failing.Store(true)
	if err := s.RecordRoute(t.Context(), route); !errors.Is(err, bolttable.ErrCommit) {
		t.Fatalf("RecordRoute = %v, want a failed commit", err)
	}
	if s.Healthy() || s.WriteFailures() != 1 {
		t.Errorf("Healthy %v, failures %d after a failed write past the lease, want unhealthy with 1", s.Healthy(), s.WriteFailures())
	}
	if err := s.UpdateHistory(t.Context(), func(*HistoryTx) error { return errors.New("test: the write's own error") }); err == nil {
		t.Fatal("UpdateHistory with a failing function succeeded")
	}
	if s.Healthy() || s.WriteFailures() != 1 {
		t.Errorf("Healthy %v, failures %d after a write's own error, want them unchanged", s.Healthy(), s.WriteFailures())
	}
	failing.Store(false)
	if err := s.RecordRoute(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	if !s.Healthy() || s.WriteFailures() != 1 {
		t.Errorf("Healthy %v, failures %d after a commit, want healthy with the count kept", s.Healthy(), s.WriteFailures())
	}
}

// Every write counts: each failed commit adds one failure.
func TestEveryWriteIsRecorded(t *testing.T) {
	failing := failCommits(t, time.Hour)
	s := mustOpen(t, testPath(t), "v1")
	defer mustClose(t, s)
	set := ScaleSet{Scope: "acme/app", Profile: "small", ID: 1}
	if err := s.RecordScaleSets(t.Context(), []ScaleSet{set}); err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	for name, write := range map[string]func() error{
		"RecordRoute":     func() error { return s.RecordRoute(t.Context(), &Route{Scope: "acme/app"}) },
		"RecordScaleSets": func() error { return s.RecordScaleSets(t.Context(), []ScaleSet{set}) },
		"DropScaleSet":    func() error { return s.DropScaleSet(t.Context(), set, time.Now()) },
		"UpdateHistory": func() error {
			return s.UpdateHistory(t.Context(), func(h *HistoryTx) error { return h.DeleteRunsBefore(time.Now()) })
		},
	} {
		before := s.WriteFailures()
		if err := write(); !errors.Is(err, bolttable.ErrCommit) {
			t.Errorf("%s = %v, want a failed commit", name, err)
		}
		if got := s.WriteFailures(); got != before+1 {
			t.Errorf("failures after a failed %s = %d, want %d", name, got, before+1)
		}
	}
}

func TestAFailedWriteInsideTheLeaseIsHealthy(t *testing.T) {
	failing := failCommits(t, time.Hour)
	s := mustOpen(t, testPath(t), "v1")
	defer mustClose(t, s)
	failing.Store(true)
	if err := s.RecordScaleSets(t.Context(), []ScaleSet{{Scope: "acme/app", Profile: "small", ID: 1}}); err == nil {
		t.Fatal("RecordScaleSets succeeded with every commit failing")
	}
	if !s.Healthy() || s.WriteFailures() != 1 {
		t.Errorf("Healthy %v, failures %d inside the lease, want healthy with 1", s.Healthy(), s.WriteFailures())
	}
}

func TestSizeAndVolumeFree(t *testing.T) {
	path := testPath(t)
	s := mustOpen(t, path, "v1")
	defer mustClose(t, s)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Size(); err != nil || got != fi.Size() {
		t.Errorf("Size = %d, %v; want the file's %d", got, err, fi.Size())
	}
	if got, err := s.VolumeFree(); err != nil || got == 0 {
		t.Errorf("VolumeFree = %d, %v; want the test directory's free space", got, err)
	}
}
