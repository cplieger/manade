// Package history is the run history (§5.3): the Recorder writes each
// job's run row and its rollups as the pool reports them, and the
// Housekeeper keeps the store's retention and the zone the rollups are cut
// in.
package history

import (
	"cmp"
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

// queueSize bounds the job messages waiting for their write; a poll never
// waits on the store.
const queueSize = 4096

// drainTimeout bounds the writes of the messages still queued at shutdown.
const drainTimeout = 5 * time.Second

// jobMsg is one job message waiting for its write.
type jobMsg struct {
	at   time.Time
	host string
	ss   github.ScaleSet
	e    github.Event
}

// Recorder writes the run history. Job queues its message for Run, which
// writes them in order; Exited and Listed write before they return.
type Recorder struct {
	st      *store.Store
	log     *slog.Logger
	now     func() time.Time
	q       chan jobMsg
	dropped atomic.Int64
}

var _ pool.Runs = (*Recorder)(nil)

// NewRecorder answers a recorder writing to st; log is slog.Default() when
// nil.
func NewRecorder(st *store.Store, log *slog.Logger) *Recorder {
	return &Recorder{st: st, log: cmp.Or(log, slog.Default()), now: time.Now, q: make(chan jobMsg, queueSize)}
}

// Job queues e for its write. A message naming no runner request is no
// run's, and a full queue drops the message with a warning.
func (rec *Recorder) Job(ss github.ScaleSet, host string, e *github.Event) {
	if e.RequestID <= 0 {
		return
	}
	select {
	case rec.q <- jobMsg{at: rec.now(), e: *e, ss: ss, host: host}:
	default:
		if rec.dropped.Add(1) == 1 {
			rec.log.Warn("The run history is behind, so job messages are dropped from it until it catches up", "queued", queueSize)
		}
	}
}

// Run writes the queued job messages until ctx ends, then those still
// queued, and returns.
func (rec *Recorder) Run(ctx context.Context) {
	for {
		select {
		case m := <-rec.q:
			rec.write(ctx, &m)
		case <-ctx.Done():
			rec.drain(ctx)
			return
		}
	}
}

func (rec *Recorder) drain(ctx context.Context) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	for {
		select {
		case m := <-rec.q:
			rec.write(dctx, &m)
		default:
			return
		}
	}
}

func (rec *Recorder) write(ctx context.Context, m *jobMsg) {
	err := rec.st.UpdateHistory(ctx, func(h *store.HistoryTx) error { return job(h, m.ss, m.host, &m.e, m.at) })
	if err != nil && ctx.Err() == nil {
		rec.log.Warn("Writing a job message to the run history failed", "scope", m.ss.Scope, "request_id", m.e.RequestID, "error", err)
	}
	if len(rec.q) == 0 {
		if n := rec.dropped.Swap(0); n > 0 {
			rec.log.Info("The run history caught up", "dropped", n)
		}
	}
}

// Exited writes x to the run of its runner.
func (rec *Recorder) Exited(ctx context.Context, x *pool.RunnerExit) {
	if err := rec.st.UpdateHistory(ctx, func(h *store.HistoryTx) error { return exited(h, x) }); err != nil && ctx.Err() == nil {
		rec.log.Warn("Writing a runner's exit to the run history failed", "host", x.Host, "runner", x.Runner, "error", err)
	}
}

// Listed closes as lost every open run of host whose runner runners does
// not hold.
func (rec *Recorder) Listed(ctx context.Context, host string, runners []string) {
	at := rec.now()
	if err := rec.st.UpdateHistory(ctx, func(h *store.HistoryTx) error { return listed(h, host, runners, at) }); err != nil && ctx.Err() == nil {
		rec.log.Warn("Closing the runs a host no longer holds failed", "host", host, "error", err)
	}
}
