package resolver

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/manade/internal/egress"
)

// LearnPad is added to each record's TTL: a job that resolved a name keeps
// its address that long past the answer's own life.
const LearnPad = 10 * time.Minute

// refusedLogEvery bounds the WARN a full set logs.
const refusedLogEvery = time.Minute

// SetWriter adds addresses to the kernel's learned sets, renewing the
// timeout of any already there.
type SetWriter func(ctx context.Context, ls []egress.Learned) error

// Family indexes a per-family count.
type Family int

// The families; FamilyCount is how many there are.
const (
	IPv4 Family = iota
	IPv6
	FamilyCount
)

// Counts are a Learner's figures for status and metrics.
type Counts struct {
	Held    [FamilyCount]int
	Refused [FamilyCount]uint64
}

// Learner holds the addresses learned for GitHub's names with their
// expiries, as the kernel's sets hold them, at most egress.LearnedMax per
// family. Its methods are safe for concurrent use.
type Learner struct {
	write   SetWriter
	log     *slog.Logger
	allowed atomic.Pointer[func(netip.Addr) bool]
	held    map[netip.Addr]time.Time
	warned  time.Time
	refused [FamilyCount]atomic.Uint64
	// n counts held's addresses per family, expired ones until pruned.
	n [FamilyCount]int
	// load is held from a learned address's record to its kernel write,
	// and by Loading's holder from a rule set's generation to its load, so
	// an address is in the loaded set or written after it.
	load sync.Mutex
	mu   sync.Mutex
}

// NewLearner answers a Learner writing through write and learning only
// addresses allowed passes.
func NewLearner(write SetWriter, allowed func(netip.Addr) bool, log *slog.Logger) *Learner {
	l := &Learner{write: write, log: log, held: map[netip.Addr]time.Time{}}
	l.allowed.Store(&allowed)
	return l
}

// SetAllowed replaces the test an address must pass, for a changed policy.
// Addresses already held stay until their timeouts; a rule set generated
// from Held filters them again.
func (l *Learner) SetAllowed(allowed func(netip.Addr) bool) { l.allowed.Store(&allowed) }

// Loading is held across a rule set's generation from Held and its load.
func (l *Learner) Loading() sync.Locker { return &l.load }

// Learn records each allowed address of recs for its TTL plus LearnPad and
// writes them to the kernel before it returns, answering the write's
// error. A new address past a family's bound is refused, counted and
// logged at most once a minute; a held one is renewed whatever the count.
func (l *Learner) Learn(ctx context.Context, recs []Record) error {
	l.load.Lock()
	defer l.load.Unlock()
	now := time.Now()
	allowed := *l.allowed.Load()
	var ls []egress.Learned
	l.mu.Lock()
	for _, r := range recs {
		a := r.Addr.Unmap()
		if !allowed(a) {
			continue
		}
		if _, ok := l.held[a]; !ok && l.full(familyOf(a), now) {
			l.refuse(familyOf(a), a, now)
			continue
		}
		if _, ok := l.held[a]; !ok {
			l.n[familyOf(a)]++
		}
		left := r.TTL + LearnPad
		l.held[a] = now.Add(left)
		ls = append(ls, egress.Learned{Addr: a, Left: left})
	}
	l.mu.Unlock()
	if len(ls) == 0 {
		return nil
	}
	return l.write(ctx, ls)
}

// full reports whether family f holds egress.LearnedMax live addresses,
// dropping expired ones first. l.mu is held.
func (l *Learner) full(f Family, now time.Time) bool {
	if l.n[f] < egress.LearnedMax {
		return false
	}
	l.prune(now)
	return l.n[f] >= egress.LearnedMax
}

func (l *Learner) prune(now time.Time) {
	for a, exp := range l.held {
		if !exp.After(now) {
			delete(l.held, a)
			l.n[familyOf(a)]--
		}
	}
}

// refuse counts a refused add; l.mu is held.
func (l *Learner) refuse(f Family, a netip.Addr, now time.Time) {
	n := l.refused[f].Add(1)
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

// Counts answers the live addresses held and the adds refused, per family.
func (l *Learner) Counts() Counts {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(time.Now())
	c := Counts{Held: l.n}
	for f := range FamilyCount {
		c.Refused[f] = l.refused[f].Load()
	}
	return c
}

func familyOf(a netip.Addr) Family {
	if a.Is4() {
		return IPv4
	}
	return IPv6
}
