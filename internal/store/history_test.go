package store

import (
	"slices"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func update(t *testing.T, s *Store, fn func(*HistoryTx) error) {
	t.Helper()
	if err := s.UpdateHistory(t.Context(), fn); err != nil {
		t.Fatalf("UpdateHistory: %v", err)
	}
}

func agedIDs(t *testing.T, s *Store, from time.Time) []uint64 {
	t.Helper()
	var ids []uint64
	update(t, s, func(h *HistoryTx) error {
		return h.Runs(from, func(r *Run) (bool, error) {
			ids = append(ids, r.ID)
			return true, nil
		})
	})
	return ids
}

// A run is found by the runner that took it, listed open on its host
// until it has an exit, and ordered by its end, or its first message while
// open.
func TestRunIndexes(t *testing.T) {
	s := mustOpen(t, testPath(t), "v1.0.0-dev.1")
	defer mustClose(t, s)
	update(t, s, func(h *HistoryTx) error {
		for _, r := range []Run{
			{ID: 1, SeenAt: t0, Runner: "r1", Host: "nas"},
			{ID: 2, SeenAt: t0.Add(-time.Hour), EndedAt: t0.Add(2 * time.Hour), Runner: "r2", Host: "nas", Exit: "completed"},
			{ID: 3, SeenAt: t0.Add(time.Minute)},
			{ID: 4, SeenAt: t0, Runner: "r4", Host: "box"},
		} {
			if err := h.PutRun(&r); err != nil {
				return err
			}
		}
		return nil
	})
	t.Run("byRunner", func(t *testing.T) {
		update(t, s, func(h *HistoryTx) error {
			r, found, err := h.RunOfRunner("r2")
			if !found || r.ID != 2 {
				t.Errorf("run of r2 = %+v, %v; want run 2", r, found)
			}
			if _, found, _ := h.RunOfRunner("r3"); found {
				t.Error("found a run of r3, which took none")
			}
			return err
		})
	})
	t.Run("openOnAHost", func(t *testing.T) {
		var got []uint64
		update(t, s, func(h *HistoryTx) error {
			return h.OpenRuns("nas", func(r *Run) (bool, error) {
				got = append(got, r.ID)
				return true, nil
			})
		})
		if !slices.Equal(got, []uint64{1}) {
			t.Errorf("open runs on nas = %v, want [1]: run 2 has an exit and run 4 is on box", got)
		}
	})
	t.Run("byAge", func(t *testing.T) {
		if got := agedIDs(t, s, t0); !slices.Equal(got, []uint64{1, 4, 3, 2}) {
			t.Errorf("runs from t0 = %v, want [1 4 3 2]: ended run 2 by its end", got)
		}
	})
}

// Deleting runs by age takes their evidence with them and leaves younger
// runs and their evidence.
func TestDeleteRunsBeforeTakesTheirEvidence(t *testing.T) {
	s := mustOpen(t, testPath(t), "v1.0.0-dev.1")
	defer mustClose(t, s)
	update(t, s, func(h *HistoryTx) error {
		for _, r := range []Run{{ID: 1, SeenAt: t0.Add(-48 * time.Hour), EndedAt: t0.Add(-25 * time.Hour)}, {ID: 2, SeenAt: t0.Add(-48 * time.Hour), EndedAt: t0}} {
			if err := h.PutRun(&r); err != nil {
				return err
			}
			if err := h.PutBlob(r.ID, &Blob{StoredAt: r.EndedAt, Log: "tail"}); err != nil {
				return err
			}
		}
		return h.DeleteRunsBefore(t0.Add(-24 * time.Hour))
	})
	if got := agedIDs(t, s, time.Time{}); !slices.Equal(got, []uint64{2}) {
		t.Errorf("runs = %v, want [2]", got)
	}
	var kept []uint64
	update(t, s, func(h *HistoryTx) error {
		return h.Blobs(func(id uint64, _ *Blob) (bool, error) {
			kept = append(kept, id)
			return true, nil
		})
	})
	if !slices.Equal(kept, []uint64{2}) {
		t.Errorf("evidence of runs %v, want [2]", kept)
	}
}

// The journal keeps rows younger than its cutoff, and at most its newest
// keep.
func TestTrimJournal(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string
		keep int64
	}{
		{name: "byAge", keep: 10, want: []string{"c", "d"}},
		{name: "byCount", keep: 1, want: []string{"d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustOpen(t, testPath(t), "v1.0.0-dev.1")
			defer mustClose(t, s)
			if err := s.live.Update(t.Context(), func(tx *bolttable.Tx) error {
				for i, subject := range []string{"a", "b", "c", "d"} {
					if err := write(tx, &entry{At: t0.Add(time.Duration(i) * time.Hour), Subject: subject, Action: actionScaleSetRecordDropped}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			update(t, s, func(h *HistoryTx) error { return h.TrimJournal(t0.Add(2*time.Hour), tc.keep) })
			var got []string
			for _, e := range journalRows(t, s) {
				got = append(got, e.Subject)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("journal = %v, want %v", got, tc.want)
			}
		})
	}
}
