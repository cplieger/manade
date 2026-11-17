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

// finished writes request id's run, started at start on nas and
// succeeded 5 minutes later.
func finished(t *testing.T, rec *Recorder, id int64, start time.Time) {
	t.Helper()
	e := message(github.Started, id, "r"+strconv.FormatInt(id, 10), "")
	e.Queued, e.RunnerAssigned = start.Add(-30*time.Second), start
	write(t, rec, "nas", e)
	e.Kind, e.Result, e.Finished = github.Completed, "succeeded", start.Add(5*time.Minute)
	write(t, rec, "nas", e)
}

// Changing the zone cuts again every day the runs still cover, from those
// runs, and leaves the days before the run retention's cutoff in the zone
// they were cut in. Each later run counts in the new zone's days.
func TestTheZoneCutsAgainTheDaysTheRunsCover(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rec, st := recorder(t, &now)
	// The cutoff is 7 days back, Nov 13 at 12:00 UTC. 23:30 UTC on Nov 15
	// is 00:30 on Nov 16 in Paris. Run 2 ends after the cutoff on a UTC day
	// that starts before it; run 3 ends before it and leaves with its run.
	finished(t, rec, 1, time.Date(2026, 11, 15, 23, 30, 0, 0, time.UTC))
	finished(t, rec, 2, time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC))
	finished(t, rec, 3, time.Date(2026, 11, 12, 22, 0, 0, 0, time.UTC))
	cfg := settings("Europe/Paris", 7*day, 7*day)
	if err := housekeeper(st, &cfg).keep(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	t.Run("coveredDayInTheNewZone", func(t *testing.T) {
		if _, found := rollup(t, st, midnight(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), "UTC"), "nas"); found {
			t.Error("the UTC rollup of Nov 15 is still there")
		}
		u, found := rollup(t, st, midnight(time.Date(2026, 11, 16, 0, 30, 0, 0, time.UTC), "Europe/Paris"), "nas")
		if !found || u.Zone != "Europe/Paris" || u.Date != "2026-11-16" || u.Jobs != 1 {
			t.Errorf("Paris rollup of Nov 16 = %+v, %v; want run 1", u, found)
		}
		r, _ := run(t, st, 1)
		if r.DayZone != "Europe/Paris" || !r.Day.Equal(midnight(time.Date(2026, 11, 16, 0, 30, 0, 0, time.UTC), "Europe/Paris")) {
			t.Errorf("run 1 counts in %v (%s), want Paris's Nov 16", r.Day, r.DayZone)
		}
	})
	t.Run("dayBeforeTheCutoffKeepsItsZone", func(t *testing.T) {
		u, found := rollup(t, st, midnight(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), "UTC"), "nas")
		if !found || u.Zone != "UTC" || u.Jobs != 1 {
			t.Errorf("UTC rollup of Nov 12 = %+v, %v; want kept", u, found)
		}
	})
	t.Run("aDayStartedBeforeTheCutoffKeepsItsRuns", func(t *testing.T) {
		u, found := rollup(t, st, midnight(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), "UTC"), "nas")
		if !found || u.Zone != "UTC" || u.Jobs != 1 {
			t.Errorf("UTC rollup of Nov 13 = %+v, %v; want run 2 kept there", u, found)
		}
		if u, found := rollup(t, st, midnight(time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC), "Europe/Paris"), "nas"); found {
			t.Errorf("Paris rollup of Nov 13 = %+v, want none: run 2 counts once", u)
		}
	})
	t.Run("laterRunsCountInTheNewZone", func(t *testing.T) {
		finished(t, rec, 4, time.Date(2026, 11, 20, 23, 30, 0, 0, time.UTC))
		if u, found := rollup(t, st, midnight(time.Date(2026, 11, 21, 0, 30, 0, 0, time.UTC), "Europe/Paris"), "nas"); !found || u.Jobs != 1 {
			t.Errorf("Paris rollup of Nov 21 = %+v, %v; want run 4", u, found)
		}
	})
	t.Run("sameZoneCutsNothing", func(t *testing.T) {
		if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
			cut, err := cutAgain(h, cfg.TimeZone, now.Add(-7*day), now)
			if cut {
				t.Error("cut again in the zone it is cut in")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// Each kind of row leaves at its own retention: runs with their evidence
// at history.runs, evidence at history.evidence, kept exits after a day and
// rollups after 400 days.
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
	old := store.RollupKey{Day: now.Add(-401 * day).Truncate(day), Scope: "acme", Profile: "small", Repository: "acme/app"}
	kept := store.RollupKey{Day: now.Add(-399 * day).Truncate(day), Scope: "acme", Profile: "small", Repository: "acme/app"}
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
		if err := h.PutRollup(&old, &store.Rollup{Jobs: 1}); err != nil {
			return err
		}
		return h.PutRollup(&kept, &store.Rollup{Jobs: 1})
	}); err != nil {
		t.Fatal(err)
	}
	cfg := settings("UTC", 7*day, 2*day)
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
	t.Run("rollups", func(t *testing.T) {
		if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
			for k, want := range map[*store.RollupKey]bool{&old: false, &kept: true} {
				if _, found, err := h.Rollup(k); err != nil || found != want {
					t.Errorf("rollup of %v kept = %v (%v), want %v", k.Day, found, err, want)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
