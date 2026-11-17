package history

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

func housekeeper(st *store.Store, cfg **config.Config) *Housekeeper {
	return NewHousekeeper(st, func() *config.Config { return *cfg }, slog.New(slog.DiscardHandler))
}

// Each kind of row leaves at its own retention: runs with their evidence
// at history.runs, evidence at history.evidence and kept exits after a
// day.
func TestRetention(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rec, st := recorder(t, &now)
	failed := func(id int64, at time.Time) {
		e := message(github.Started, id, "f"+strconv.FormatInt(id, 10), "")
		write(t, rec, "nas", e)
		x := exitOf(e.Runner, pool.ExitFailed, 1, at)
		x.Log = []byte("tail")
		rec.Exited(t.Context(), x)
	}
	failed(1, now.Add(-8*day))
	failed(2, now.Add(-3*day))
	failed(3, now.Add(-time.Hour))
	rec.Exited(t.Context(), exitOf("stray-old", pool.ExitFailed, 1, now.Add(-25*time.Hour)))
	rec.Exited(t.Context(), exitOf("stray-new", pool.ExitFailed, 1, now.Add(-23*time.Hour)))
	cfg := settings(7*day, 2*day)
	if err := housekeeper(st, &cfg).keep(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	t.Run("runs", func(t *testing.T) {
		for id, want := range map[uint64]bool{1: false, 2: true, 3: true} {
			if _, found := run(t, st, id); found != want {
				t.Errorf("run %d kept = %v, want %v", id, found, want)
			}
		}
	})
	t.Run("evidence", func(t *testing.T) {
		for id, want := range map[uint64]bool{1: false, 2: false, 3: true} {
			if _, found := blob(t, st, id); found != want {
				t.Errorf("evidence of run %d kept = %v, want %v", id, found, want)
			}
		}
	})
	t.Run("exits", func(t *testing.T) {
		if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
			for name, want := range map[string]bool{"stray-old": false, "stray-new": true} {
				if _, found, err := h.Exit(name); err != nil || found != want {
					t.Errorf("exit of %s kept = %v (%v), want %v", name, found, err, want)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
