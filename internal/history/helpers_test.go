package history

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

var (
	// queued is when the test's job was queued; it is assigned 10 seconds
	// later, a runner takes it after 30 and it finishes after 5 minutes.
	queued = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ss     = github.ScaleSet{Scope: "acme", Profile: "small", ID: 3}
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), store.File), store.Options{Build: "test", Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

// recorder answers a recorder over a new store on a clock reading at.
func recorder(t *testing.T, at *time.Time) (*Recorder, *store.Store) {
	t.Helper()
	st := openStore(t)
	rec := NewRecorder(st, slog.New(slog.DiscardHandler))
	rec.now = func() time.Time { return *at }
	return rec, st
}

// message answers job message kind of request id: run 41's "build" job in
// acme/app's ci workflow, started by a push.
func message(kind github.EventKind, id int64, runner, result string) github.Event {
	e := github.Event{
		Kind: kind, RequestID: id, RunID: 41, Name: "build", Repository: "acme/app",
		WorkflowRef: "acme/app/.github/workflows/ci.yaml@refs/heads/main", Trigger: "push",
		Queued: queued, Assigned: queued.Add(10 * time.Second), Runner: runner, Result: result,
	}
	if kind != github.Assigned {
		e.RunnerAssigned = queued.Add(30 * time.Second)
	}
	if kind == github.Completed {
		e.Finished = queued.Add(5*time.Minute + 30*time.Second)
	}
	return e
}

// write writes job message e on host as the recorder's loop does.
func write(t *testing.T, rec *Recorder, host string, e github.Event) {
	t.Helper()
	rec.write(t.Context(), &jobMsg{at: rec.now(), e: e, ss: ss, host: host})
}

func exitOf(runner string, reason pool.ExitReason, code int, at time.Time) *pool.RunnerExit {
	return &pool.RunnerExit{At: at, Code: &code, Host: "nas", Runner: runner, Reason: reason}
}

// run answers request id's row.
func run(t *testing.T, st *store.Store, id uint64) (store.Run, bool) {
	t.Helper()
	var r store.Run
	var found bool
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) (err error) {
		r, found, err = h.Run(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r, found
}

// rollup answers the rollup of acme/app's small runs on host on the day
// starting at day.
func rollup(t *testing.T, st *store.Store, day time.Time, host string) (store.Rollup, bool) {
	t.Helper()
	var u store.Rollup
	var found bool
	k := store.RollupKey{Day: day, Scope: ss.Scope, Profile: ss.Profile, Host: host, Repository: "acme/app"}
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) (err error) {
		u, found, err = h.Rollup(&k)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return u, found
}

// blob answers run id's evidence.
func blob(t *testing.T, st *store.Store, id uint64) (store.Blob, bool) {
	t.Helper()
	var b store.Blob
	var found bool
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
		return h.Blobs(func(bid uint64, v *store.Blob) (bool, error) {
			if bid == id {
				b, found = *v, true
			}
			return !found, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return b, found
}

// ladder is §12.1's histogram edges in seconds: 0 s to 7 days.
var ladder = []float64{0, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 43200, 86400, 259200, 604800}

// bins answers the ladder's 17 intervals with one count in interval i.
func bins(i int) []int64 {
	b := make([]int64, len(ladder)-1)
	b[i]++
	return b
}

func settings(zone string, runs, evidence time.Duration) *config.Config {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		panic(err)
	}
	return &config.Config{TimeZone: loc, History: config.History{Runs: runs, Evidence: evidence}}
}

const day = 24 * time.Hour

func midnight(t time.Time, zone string) time.Time {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		panic(err)
	}
	return dayOf(t, loc)
}
