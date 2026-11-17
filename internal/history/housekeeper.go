package history

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/store"
)

// Retention the settings do not decide (§5.3).
const (
	keepEvery      = time.Hour
	exitsFor       = 24 * time.Hour
	rollupsFor     = 400 * 24 * time.Hour
	journalFor     = 400 * 24 * time.Hour
	journalMaxRows = 20000
)

// Housekeeper applies the store's retention and cuts the rollups again
// when the pool's time zone changed.
type Housekeeper struct {
	st       *store.Store
	settings func() *config.Config
	log      *slog.Logger
	now      func() time.Time
}

// NewHousekeeper answers a housekeeper of st under the settings settings
// answers at each run; log is slog.Default() when nil.
func NewHousekeeper(st *store.Store, settings func() *config.Config, log *slog.Logger) *Housekeeper {
	return &Housekeeper{st: st, settings: settings, log: cmp.Or(log, slog.Default()), now: time.Now}
}

// Run keeps the store at once and every hour until ctx ends.
func (k *Housekeeper) Run(ctx context.Context) {
	scheduler.RunLoop(ctx, k.run, scheduler.LoopOptions{Interval: keepEvery, FireOnStart: true})
}

func (k *Housekeeper) run(ctx context.Context) {
	k.log.Info("Housekeeping started")
	if err := k.keep(ctx, k.now()); err != nil {
		if ctx.Err() == nil {
			k.log.Warn("Housekeeping failed, so it runs again in an hour", "error", err)
		}
		return
	}
	k.log.Info("Housekeeping finished")
}

// keep applies retention and the zone at now in one transaction.
func (k *Housekeeper) keep(ctx context.Context, now time.Time) error {
	cfg := k.settings()
	return k.st.UpdateHistory(ctx, func(h *store.HistoryTx) error {
		cutoff := now.Add(-cfg.History.Runs).Truncate(time.Second)
		if err := retain(h, cfg, cutoff, now); err != nil {
			return err
		}
		recut, err := cutAgain(h, cfg.TimeZone, cutoff, now)
		if recut {
			k.log.Info("The rollups were cut again in the pool's time zone", "zone", cfg.TimeZone.String(), "from", cutoff.UTC().Format(time.RFC3339))
		}
		return err
	})
}

// retain deletes runs ending before cutoff, and evidence, kept exits,
// rollups and journal rows past their own retention at now.
func retain(h *store.HistoryTx, cfg *config.Config, cutoff, now time.Time) error {
	if err := h.DeleteRunsBefore(cutoff); err != nil {
		return err
	}
	var old []uint64
	err := h.Blobs(func(id uint64, b *store.Blob) (bool, error) {
		if now.Sub(b.StoredAt) > cfg.History.Evidence {
			old = append(old, id)
		}
		return true, nil
	})
	for _, id := range old {
		if err != nil {
			return err
		}
		err = h.DeleteBlob(id)
	}
	if err != nil {
		return err
	}
	var stale []string
	err = h.Exits(func(x *store.Exit) (bool, error) {
		if now.Sub(x.At) > exitsFor {
			stale = append(stale, x.Runner)
		}
		return true, nil
	})
	for _, name := range stale {
		if err != nil {
			return err
		}
		err = h.DeleteExit(name)
	}
	if err != nil {
		return err
	}
	if err := h.DeleteRollups(time.Time{}, now.Add(-rollupsFor)); err != nil {
		return err
	}
	return h.TrimJournal(now.Add(-journalFor), journalMaxRows)
}

// cutAgain cuts the rollups again in loc when they are cut in another
// zone: each rollup whose day starts at or after cutoff is rebuilt from the
// runs counted in it, and an older one keeps its zone. It reports whether
// it cut.
func cutAgain(h *store.HistoryTx, loc *time.Location, cutoff, now time.Time) (bool, error) {
	cur, err := cutZone(h)
	if err != nil || cur.String() == loc.String() {
		return false, err
	}
	if err := h.DeleteRollups(cutoff, time.Time{}); err != nil {
		return false, err
	}
	var again []store.Run
	if err := h.Runs(cutoff, func(r *store.Run) (bool, error) {
		if r.Counted != 0 && !r.Day.Before(cutoff) {
			again = append(again, *r)
		}
		return true, nil
	}); err != nil {
		return false, err
	}
	for i := range again {
		r := &again[i]
		place(r, loc)
		if err := rollIn(h, r, r.Counted); err != nil {
			return false, err
		}
		if err := h.PutRun(r); err != nil {
			return false, err
		}
	}
	return true, h.PutCut(&store.Cut{At: now, Zone: loc.String()})
}
