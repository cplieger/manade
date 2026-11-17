package history

import (
	"cmp"
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/store"
)

// Retention the settings do not decide (§5.3).
const (
	keepEvery      = time.Hour
	exitsFor       = 24 * time.Hour
	hostsFor       = 30 * 24 * time.Hour
	journalFor     = 400 * 24 * time.Hour
	journalMaxRows = 20000
)

// keepLease is how long the housekeeper may go without a finished run
// before it reads unhealthy (§12.3): two of its hourly runs.
const keepLease = 2 * keepEvery

// Housekeeper applies the history's retention.
type Housekeeper struct {
	st       *store.Store
	settings func() *config.Config
	log      *slog.Logger
	now      func() time.Time
	// since is when Run began or the last run finished, in Unix
	// nanoseconds, 0 before Run.
	since atomic.Int64
}

// NewHousekeeper answers a housekeeper of st under the settings settings
// answers at each run; log is slog.Default() when nil.
func NewHousekeeper(st *store.Store, settings func() *config.Config, log *slog.Logger) *Housekeeper {
	return &Housekeeper{st: st, settings: settings, log: cmp.Or(log, slog.Default()), now: time.Now}
}

// Run keeps the store at once and every hour until ctx ends.
func (k *Housekeeper) Run(ctx context.Context) {
	k.since.Store(k.now().UnixNano())
	scheduler.RunLoop(ctx, k.run, scheduler.LoopOptions{Interval: keepEvery, FireOnStart: true})
}

// Due answers when a finished run is due: keepLease after Run began or the
// last run finished, zero before Run.
func (k *Housekeeper) Due() time.Time {
	since := k.since.Load()
	if since == 0 {
		return time.Time{}
	}
	return time.Unix(0, since).Add(keepLease)
}

// Healthy reports false once Due has passed.
func (k *Housekeeper) Healthy() bool {
	due := k.Due()
	return due.IsZero() || !k.now().After(due)
}

func (k *Housekeeper) run(ctx context.Context) {
	k.log.Info("Housekeeping started")
	if err := k.keep(ctx, k.now()); err != nil {
		if ctx.Err() == nil {
			k.log.Warn("Housekeeping failed, so it runs again in an hour", "error", err)
		}
		return
	}
	k.since.Store(k.now().UnixNano())
	k.log.Info("Housekeeping finished")
}

// keep applies retention at now in one transaction.
func (k *Housekeeper) keep(ctx context.Context, now time.Time) error {
	cfg := k.settings()
	return k.st.UpdateHistory(ctx, func(h *store.HistoryTx) error { return retain(h, cfg, now) })
}

// retain deletes runs, evidence, kept exits, host events, probes and
// journal rows past their retention at now, and forgets the hosts cfg no
// longer holds.
func retain(h *store.HistoryTx, cfg *config.Config, now time.Time) error {
	if err := h.DeleteRunsBefore(now.Add(-cfg.History.Runs)); err != nil {
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
	if err := retainHosts(h, cfg, now); err != nil {
		return err
	}
	return h.TrimJournal(now.Add(-journalFor), journalMaxRows)
}

func retainHosts(h *store.HistoryTx, cfg *config.Config, now time.Time) error {
	if err := h.DeleteHostEventsBefore(now.Add(-hostsFor)); err != nil {
		return err
	}
	if err := h.DeleteProbesBefore(now.Add(-hostsFor)); err != nil {
		return err
	}
	var gone []string
	err := h.HostsSeen(func(host string) (bool, error) {
		if _, held := cfg.Host(host); !held {
			gone = append(gone, host)
		}
		return true, nil
	})
	for _, host := range gone {
		if err != nil {
			break
		}
		err = h.DeleteHostSeen(host)
	}
	return err
}
