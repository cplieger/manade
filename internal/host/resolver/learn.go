package resolver

import (
	"context"
	"log/slog"
	"maps"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/manade/internal/egress"
)

// learnPad is added to each record's TTL: a job that resolved a name keeps
// its address that long past the answer's own life.
const learnPad = 10 * time.Minute

// refusedLogEvery bounds the WARN a full set logs.
const refusedLogEvery = time.Minute

// learnTimeout bounds a learned address's kernel write.
const learnTimeout = 2 * time.Second

// SetWriter adds addresses to the kernel's learned sets, renewing the
// timeout of any already there.
type SetWriter func(ctx context.Context, ls []egress.Learned) error

// Counts are a Learner's figures for status and metrics.
type Counts struct {
	Held    int
	Refused uint64
}

// Learner holds the addresses learned for GitHub's names with their
// expiries, as the kernel's set holds them, at most egress.LearnedMax, each
// one egress.Learnable passes. Its methods are safe for concurrent use.
type Learner struct {
	write SetWriter
	log   *slog.Logger
	held  map[netip.Addr]time.Time
	// queued is the batch the next write takes; while it is set, draining
	// is too, so one drain covers every queued batch.
	queued *batch
	// load is held from a learned address's record to its kernel write,
	// and by Loading's holder from a rule set's generation to its load, so
	// an address is in the loaded set or written after it.
	load    loadLock
	warned  time.Time
	refused atomic.Uint64
	mu      sync.Mutex
	// retired drops every batch a drain takes from now on unwritten.
	retired  bool
	draining bool
}

// batch is the records of every add made while a drain waits on the load
// lock or writes the batch before it.
type batch struct {
	done chan struct{}
	recs []Record
}

// loadLock is a mutex whose wait a context can end, so a drain stops
// waiting out a rule load once its forwarder stops.
type loadLock chan struct{}

func (k loadLock) Lock() { k <- struct{}{} }

func (k loadLock) Unlock() { <-k }

func (k loadLock) lock(ctx context.Context) error {
	select {
	case k <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewLearner answers a Learner writing through write.
func NewLearner(write SetWriter, log *slog.Logger) *Learner {
	return &Learner{write: write, log: log, held: map[netip.Addr]time.Time{}, load: make(loadLock, 1)}
}

// Retire waits for a kernel write in flight and makes the learner write
// nothing more, for a rule set it learned into that is being replaced: a
// batch queued meanwhile is dropped, its written channel closed.
func (l *Learner) Retire() {
	l.load.Lock()
	defer l.load.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retired = true
}

// Loading is held across a rule set's generation from Held and its load.
func (l *Learner) Loading() sync.Locker { return l.load }

// Learn queues recs' allowed addresses for the next kernel write. It answers
// a channel closed once that write is done or failed (logged), or at once
// when none is allowed, and the drain the caller runs under its context when
// none is in flight, else nil. The drain records each address for its TTL
// plus learnPad under the load lock and writes each batch once within
// learnTimeout; adds made meanwhile join the next batch, so a rule load waits
// behind at most one write. A new address past the bound is refused,
// counted and logged at most once a minute; a held one is renewed.
func (l *Learner) Learn(recs []Record) (written <-chan struct{}, drain func(context.Context)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keep []Record
	for _, r := range recs {
		if egress.Learnable(r.Addr) {
			keep = append(keep, r)
		}
	}
	if len(keep) == 0 {
		return nothingToWrite, nil
	}
	if l.queued == nil {
		l.queued = &batch{done: make(chan struct{})}
	}
	l.queued.recs = append(l.queued.recs, keep...)
	if !l.draining {
		l.draining = true
		drain = l.drain
	}
	return l.queued.done, drain
}

// nothingToWrite is the written channel of an add with no allowed address.
var nothingToWrite = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// drain writes the queued batches in turn until none is left; when ctx
// ends before the drain holds the load lock, the batch is failed
// unrecorded.
func (l *Learner) drain(ctx context.Context) {
	for {
		if err := l.load.lock(ctx); err != nil {
			l.mu.Lock()
			b := l.take()
			l.draining = false
			l.mu.Unlock()
			l.log.Warn("Learned GitHub addresses were not added: the resolver stopped before they were written", "addresses", len(b.recs), "err", err)
			close(b.done)
			return
		}
		l.mu.Lock()
		b := l.take()
		var ls []egress.Learned
		if !l.retired {
			ls = l.record(b.recs, time.Now())
		}
		l.mu.Unlock()
		err := l.writeBounded(ctx, ls)
		l.load.Unlock()
		if err != nil {
			l.log.Warn("Adding learned GitHub addresses to the rules failed; jobs' connections to them are dropped", "addresses", len(ls), "err", err)
		}
		close(b.done)
		l.mu.Lock()
		if l.queued == nil {
			l.draining = false
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
	}
}

// take answers the queued batch and empties the queue; l.mu is held.
func (l *Learner) take() *batch {
	b := l.queued
	l.queued = nil
	return b
}

// record holds each address of recs, which Learn filtered, answering what
// to write; l.mu is held.
func (l *Learner) record(recs []Record, now time.Time) []egress.Learned {
	var ls []egress.Learned
	for _, r := range recs {
		a := r.Addr.Unmap()
		if _, ok := l.held[a]; !ok && l.full(now) {
			l.refuse(a, now)
			continue
		}
		left := r.TTL + learnPad
		l.held[a] = now.Add(left)
		ls = append(ls, egress.Learned{Addr: a, Left: left})
	}
	return ls
}

func (l *Learner) writeBounded(ctx context.Context, ls []egress.Learned) error {
	if len(ls) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, learnTimeout)
	defer cancel()
	return l.write(ctx, ls)
}

// full reports whether egress.LearnedMax live addresses are held, dropping
// expired ones first. l.mu is held.
func (l *Learner) full(now time.Time) bool {
	if len(l.held) < egress.LearnedMax {
		return false
	}
	l.prune(now)
	return len(l.held) >= egress.LearnedMax
}

func (l *Learner) prune(now time.Time) {
	maps.DeleteFunc(l.held, func(_ netip.Addr, exp time.Time) bool { return !exp.After(now) })
}

// refuse counts a refused add; l.mu is held.
func (l *Learner) refuse(a netip.Addr, now time.Time) {
	n := l.refused.Add(1)
	if now.Sub(l.warned) < refusedLogEvery {
		return
	}
	l.warned = now
	l.log.Warn("The learned GitHub address set is full, so jobs' connections to this address are dropped until entries expire",
		"addr", a, "max", egress.LearnedMax, "refused_total", n)
}

// Held answers each live address with its time left.
func (l *Learner) Held() []egress.Learned {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.prune(now)
	out := make([]egress.Learned, 0, len(l.held))
	for a, exp := range l.held {
		out = append(out, egress.Learned{Addr: a, Left: exp.Sub(now)})
	}
	return out
}

// Counts answers the live addresses held and the adds refused.
func (l *Learner) Counts() Counts {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(time.Now())
	return Counts{Held: len(l.held), Refused: l.refused.Load()}
}
