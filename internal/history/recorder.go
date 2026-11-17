// Package history is the history (§5.3): the Recorder writes each job's
// run row and each host's events and probes as the pool reports them, and
// the Housekeeper keeps their retention.
package history

import (
	"cmp"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

// queueSize bounds the messages waiting for their write; neither a poll
// nor a pass waits on the store.
const queueSize = 4096

// drainTimeout bounds the writes of the messages still queued at shutdown,
// and of the gaps after them.
const drainTimeout = 5 * time.Second

// queueLease is how long the queue may stay unemptied before the recorder
// reads unhealthy (§12.3).
const queueLease = 2 * time.Minute

// jobMsg is one job message waiting for its write.
type jobMsg struct {
	at     time.Time
	host   string
	waited pool.JobReason
	ss     github.ScaleSet
	e      github.Event
}

// Recorder writes the history. Job, State, Status and Proved queue their
// message for Run, which writes them in order; Exited and Listed write
// before they return.
type Recorder struct {
	// busySince is when the queue last turned non-empty, zero while it is
	// empty; qmu guards it and orders each send against the emptied
	// check.
	busySince time.Time
	st        *store.Store
	log       *slog.Logger
	now       func() time.Time
	q         chan func(context.Context)
	// seen holds what the history last recorded of each host this process
	// observed; only Run's writes touch it.
	seen    map[string]*store.HostSeen
	dropped atomic.Int64
	qmu     sync.Mutex
}

var _ pool.Runs = (*Recorder)(nil)

// NewRecorder answers a recorder writing to st; log is slog.Default() when
// nil.
func NewRecorder(st *store.Store, log *slog.Logger) *Recorder {
	return &Recorder{
		st: st, log: cmp.Or(log, slog.Default()), now: time.Now,
		q: make(chan func(context.Context), queueSize), seen: map[string]*store.HostSeen{},
	}
}

// Job queues e for its write. A message naming no runner request is no
// run's, and a full queue drops the message with a warning.
func (rec *Recorder) Job(ss github.ScaleSet, host string, e *github.Event, waited pool.JobReason) {
	if e.RequestID <= 0 {
		return
	}
	m := jobMsg{at: rec.now(), e: *e, ss: ss, host: host, waited: waited}
	rec.enqueue(func(ctx context.Context) { rec.write(ctx, &m) })
}

// enqueue queues w for Run; a full queue drops it with a warning.
func (rec *Recorder) enqueue(w func(context.Context)) {
	rec.qmu.Lock()
	defer rec.qmu.Unlock()
	select {
	case rec.q <- w:
		if rec.busySince.IsZero() {
			rec.busySince = rec.now()
		}
	default:
		if rec.dropped.Add(1) == 1 {
			rec.log.Warn("The history is behind, so messages are dropped from it until it catches up", "queued", queueSize)
		}
	}
}

// Healthy reports false once the queue has gone unemptied for longer than
// queueLease.
func (rec *Recorder) Healthy() bool {
	rec.qmu.Lock()
	since := rec.busySince
	rec.qmu.Unlock()
	return since.IsZero() || rec.now().Sub(since) <= queueLease
}

// Run writes the queued messages until ctx ends, then those still queued
// and a gap for each host it observed, and returns. Writes run under their
// own context, which ends drainTimeout after ctx: a write that ctx's end
// would cut is one the shutdown would lose.
func (rec *Recorder) Run(ctx context.Context) {
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer context.AfterFunc(ctx, func() { time.AfterFunc(drainTimeout, cancel) })()
	for {
		select {
		case w := <-rec.q:
			rec.written(wctx, w)
		case <-ctx.Done():
			rec.drain(wctx)
			return
		}
	}
}

func (rec *Recorder) drain(ctx context.Context) {
	for {
		select {
		case w := <-rec.q:
			rec.written(ctx, w)
		default:
			rec.stopped(ctx, rec.now())
			return
		}
	}
}

// written runs w, and marks the queue emptied and reports the catch-up
// once it is empty.
func (rec *Recorder) written(ctx context.Context, w func(context.Context)) {
	w(ctx)
	rec.qmu.Lock()
	empty := len(rec.q) == 0
	if empty {
		rec.busySince = time.Time{}
	}
	rec.qmu.Unlock()
	if !empty {
		return
	}
	if n := rec.dropped.Swap(0); n > 0 {
		rec.log.Info("The history caught up", "dropped", n)
	}
}

func (rec *Recorder) write(ctx context.Context, m *jobMsg) {
	err := rec.st.UpdateHistory(ctx, func(h *store.HistoryTx) error { return job(h, m) })
	if err != nil && ctx.Err() == nil {
		rec.log.Warn("Writing a job message to the run history failed", "scope", m.ss.Scope, "request_id", m.e.RequestID, "error", err)
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
