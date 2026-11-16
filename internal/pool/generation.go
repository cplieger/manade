package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

const (
	// ensureTimeout bounds one scope's scale-set ensure; ensureRetry
	// spaces the ensures of a scope whose last one failed.
	ensureTimeout = time.Minute
	ensureRetry   = time.Minute
)

// generation is the pool for one settings revision: its settings, its
// ledger and queue, its per-host loops and its start worker. Its records
// pass to the next generation, which boots through the same path.
type generation struct {
	// rechecked is when the last pass over every scope began.
	rechecked time.Time
	reg       *Registry
	d         *Deps
	cfg       *config.Config
	sections  map[string]string
	sets      map[*book]*set
	now       func() time.Time
	kick      chan struct{}
	verify    chan struct{}
	scopes    map[string]*scope
	l         *ledger
	log       *slog.Logger
	// stopLoops ends the loops at their next pass boundary; nil until start.
	stopLoops context.CancelFunc
	hosts     []*host
	wg        sync.WaitGroup
	rev       uint64
	mu        sync.Mutex
	// booting holds health at starting until the first generation's boot
	// pass has adopted every runner its hosts list.
	booting, stopping bool
}

func newGeneration(r *Registry, s *Settings) *generation {
	cfg := s.Config
	g := &generation{
		reg: r, d: &r.d, cfg: cfg, sections: s.Sections, rev: s.Revision, log: r.d.Log, now: r.now,
		l: newLedger(cfg), booting: true, kick: make(chan struct{}, 1), verify: make(chan struct{}, 1),
		scopes: map[string]*scope{}, sets: map[*book]*set{},
	}
	for i := range cfg.Hosts {
		g.hosts = append(g.hosts, &host{cfg: &cfg.Hosts[i], want: make(chan struct{}, 1), wake: make(chan struct{}, 1)})
	}
	for i := range cfg.Scopes {
		// A scope this generation adds is read at once.
		g.scopes[cfg.Scopes[i].Key] = &scope{cfg: &cfg.Scopes[i], unregister: map[string]int64{}, wanted: true}
	}
	return g
}

// scope is one scope's records.
type scope struct {
	// proof is when a read last proved the scope private; read is when its
	// last visibility read began, and failed when its reads began failing,
	// zero while they succeed. above is when a poll last reported capacity
	// above its assignments, which keeps the proof refreshed.
	proof, read, failed, above time.Time
	// ensureErr is why the scope's scale sets could not be ensured, nil
	// once they are; the ensure loop retries it.
	ensureErr error
	cfg       *config.Scope
	// unregister holds, by runner name, the registrations of ended
	// runners whose removal GitHub has not yet taken.
	unregister map[string]int64
	// jobs is the last complete read of the scope's queued jobs.
	jobs []github.QueuedJob
	// partial reports that the last read was not complete; wanted, that a
	// read is due at once.
	partial, public, wanted bool
}

// private reports that sc's last visibility read proved it private.
func (sc *scope) private() bool { return !sc.public && sc.failed.IsZero() && !sc.proof.IsZero() }

func (sc *scope) jobsOf(profile string) []github.QueuedJob {
	var out []github.QueuedJob
	for _, j := range sc.jobs {
		if j.Profile == profile {
			out = append(out, j)
		}
	}
	return out
}

// set is what the pool keeps of one scale set beside its book.
type set struct {
	scope *scope
	// beat is the listener's last Beat.
	beat time.Time
	// retryAt holds starts back after a failed JIT config.
	retryAt time.Time
	// opened reports a session opened; held, a session another process
	// holds.
	opened, held bool
}

// keepSets records old's scale sets whose scope and profile g's settings
// still serve: a scale set is GitHub's record of a profile's name in a
// scope, which no other setting changes, so it needs no ensure again.
func (g *generation) keepSets(old *generation) {
	old.mu.Lock()
	defer old.mu.Unlock()
	for _, b := range old.l.books {
		pr, ok := g.cfg.Profile(b.ss.Profile)
		if g.scopes[b.ss.Scope] != nil && ok && pr.Allowed(b.ss.Scope) {
			g.addSet(b.ss)
		}
	}
}

// ensure finds or creates the scale sets of every scope that lacks one of
// its profiles'. A scope whose ensure fails is not served, with its
// reason, and the ensure loop retries it: no failure stops a generation
// from booting.
func (g *generation) ensure(ctx context.Context) {
	for i := range g.cfg.Scopes {
		if sc := g.scopes[g.cfg.Scopes[i].Key]; g.lacksSets(sc) {
			g.ensureScope(ctx, sc)
		}
	}
}

// lacksSets reports a profile allowed in sc with no scale set recorded.
func (g *generation) lacksSets(sc *scope) bool {
	for i := range g.cfg.Profiles {
		if g.cfg.Profiles[i].Allowed(sc.cfg.Key) && g.bookOf(sc.cfg.Key, g.cfg.Profiles[i].Name) == nil {
			return true
		}
	}
	return false
}

// ensureScope ensures sc's scale sets and records them, or the failure.
func (g *generation) ensureScope(ctx context.Context, sc *scope) {
	ectx, cancel := context.WithTimeout(ctx, ensureTimeout)
	sets, err := g.d.GitHub.Ensure(ectx, sc.cfg)
	cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		if sc.ensureErr == nil && ctx.Err() == nil {
			g.log.Warn("Ensuring a scope's scale sets failed, so the scope is not served until a retry succeeds", "scope", sc.cfg.Key, "error", err)
		}
		sc.ensureErr = err
		return
	}
	sc.ensureErr = nil
	for _, ss := range sets {
		if g.bookOf(ss.Scope, ss.Profile) == nil {
			g.addSet(ss)
		}
	}
	g.gate()
}

// runEnsure retries each scope whose ensure failed every ensureRetry until
// ctx ends, and opens the sessions the scale sets it found need.
func (g *generation) runEnsure(ctx context.Context) {
	t := time.NewTicker(ensureRetry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if g.retryEnsure(ctx) && ctx.Err() == nil {
			g.reg.sync()
		}
	}
}

// retryEnsure ensures again each scope whose last ensure failed, and
// reports whether any was due.
func (g *generation) retryEnsure(ctx context.Context) bool {
	g.mu.Lock()
	var due []*scope
	for i := range g.cfg.Scopes {
		if sc := g.scopes[g.cfg.Scopes[i].Key]; sc.ensureErr != nil {
			due = append(due, sc)
		}
	}
	g.mu.Unlock()
	for _, sc := range due {
		g.ensureScope(ctx, sc)
	}
	return len(due) > 0
}

// addSet records a scale set Ensure found or created; g.mu is held.
func (g *generation) addSet(ss github.ScaleSet) {
	sc := g.scopes[ss.Scope]
	b := g.l.addBook(ss, sc.cfg.Kind != config.ScopeRepo)
	g.sets[b] = &set{scope: sc}
}

// book finds the scale set's records, nil for one this generation does
// not hold.
func (g *generation) book(ss github.ScaleSet) (*book, *set) {
	b := g.bookOf(ss.Scope, ss.Profile)
	if b == nil {
		return nil, nil
	}
	return b, g.sets[b]
}

// bookOf finds the scale set a runner's scope and profile name, nil for
// one the settings do not hold.
func (g *generation) bookOf(scope, profile string) *book {
	for _, b := range g.l.books {
		if b.ss.Scope == scope && b.ss.Profile == profile {
			return b
		}
	}
	return nil
}

// servedSets answers the scale sets whose sessions should be open: every
// one of a scope not proven public.
func (g *generation) servedSets() map[setKey]github.ScaleSet {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[setKey]github.ScaleSet{}
	for b, s := range g.sets {
		if !s.scope.public {
			out[setKey{b.ss.Scope, b.ss.Profile}] = b.ss
		}
	}
	return out
}

// takeOver carries old's records into g, which holds the next revision;
// both locks are held and old's loops have stopped. Scopes, scale sets
// and hosts keep their state by name; a runner keeps its record where its
// scale set and host remain, marked stale when its profile's spec changed,
// and becomes a stray its next listing judges where its scale set went.
func (g *generation) takeOver(old *generation) {
	g.rechecked, g.booting = old.rechecked, old.booting
	for key, sc := range g.scopes {
		if o := old.scopes[key]; o != nil {
			sc.carry(o)
		}
	}
	at := make([]int, len(old.hosts))
	for j, oh := range old.hosts {
		at[j] = slices.IndexFunc(g.hosts, func(h *host) bool { return h.cfg.Name == oh.cfg.Name })
		if at[j] >= 0 {
			g.hosts[at[j]].carry(oh)
		}
	}
	for _, ob := range old.l.books {
		g.takeBook(old, ob, at)
	}
	for name, st := range old.l.strays {
		g.takeStray(old, name, &st, at)
	}
	for i := range g.hosts {
		if g.hosts[i].st != nil {
			g.judge(i)
		}
	}
	g.gate()
}

// takeStray carries old's stray name into g where its host remains, its
// profile named by g's index; both locks are held.
func (g *generation) takeStray(old *generation, name string, st *stray, at []int) {
	h := at[st.host]
	if h < 0 {
		return
	}
	c := *st
	c.host, c.profile = h, -1
	if st.profile >= 0 {
		c.profile = g.profileIndex(old.cfg.Profiles[st.profile].Name)
	}
	g.l.hold(name, &c)
}

// takeBook carries old's scale set ob and its runners into g, with old's
// host indices moved by at; both locks are held.
func (g *generation) takeBook(old *generation, ob *book, at []int) {
	nb := g.bookOf(ob.ss.Scope, ob.ss.Profile)
	if nb != nil {
		nb.carry(ob, at, g.l.profiles[nb.profile].hosts)
		s, os := g.sets[nb], old.sets[ob]
		s.beat, s.retryAt, s.opened, s.held = os.beat, os.retryAt, os.opened, os.held
	}
	same := nb != nil && sameSpec(&old.cfg.Profiles[ob.profile], &g.cfg.Profiles[nb.profile])
	for name, r := range ob.runners {
		h := at[r.host]
		switch {
		case h < 0:
		case nb == nil:
			g.l.hold(name, &stray{created: r.created, res: r.res, host: h, profile: g.profileIndex(ob.ss.Profile), recheck: true})
		default:
			c := *r
			c.host, c.stale = h, r.stale || !same
			nb.runners[name] = &c
		}
	}
}

// profileIndex is the profile named name's index, -1 for none.
func (g *generation) profileIndex(name string) int {
	if i, ok := g.l.byName[name]; ok {
		return i
	}
	return -1
}

// sameSpec reports that a runner built for a would be built the same for
// b: they differ at most in where and how often the profile is served.
func sameSpec(a, b *config.Profile) bool {
	x, y := *a, *b
	for _, p := range []*config.Profile{&x, &y} {
		p.Scopes, p.Hosts, p.Retiring, p.Deleting = nil, nil, nil, false
		p.MaxRunners, p.MaxLifetime, p.IdleTimeout = 0, 0, 0
	}
	return reflect.DeepEqual(x, y)
}

// carry takes old's state for the same scope key.
func (sc *scope) carry(o *scope) {
	sc.proof, sc.read, sc.failed, sc.above = o.proof, o.read, o.failed, o.above
	sc.unregister, sc.jobs = o.unregister, o.jobs
	sc.partial, sc.public, sc.wanted = o.partial, o.public, o.wanted
}

// carry takes old's GitHub-side state for the same scale set: its
// assignments, skipped jobs, poll in flight and slots. A slot moves to its
// host's new index by at, and only onto hosts, the profile's hosts now:
// the ledger's trim reads only those, so a slot elsewhere would be held
// for good.
func (b *book) carry(o *book, at, hosts []int) {
	b.standSince, b.standEnded, b.unplacedSince, b.lastAssigned = o.standSince, o.standEnded, o.unplacedSince, o.lastAssigned
	b.asg, b.skipped, b.misses, b.jobs, b.flight = o.asg, o.skipped, o.misses, o.jobs, o.flight
	for j, n := range o.pend {
		if h := at[j]; h >= 0 && slices.Contains(hosts, h) {
			b.pend[h] += n
		}
	}
}

// gate brings every gate the ledger reads to the generation's state and
// settles it; g.mu is held.
func (g *generation) gate() {
	for b, s := range g.sets {
		b.serve = !g.stopping && !s.scope.public && !s.held
		b.fundable = b.serve && s.scope.private() && !s.scope.partial
	}
	for i, h := range g.hosts {
		hb := &g.l.hosts[i]
		hb.reachable = h.reachable
		hb.serving = h.serving()
		for pr := range hb.ready {
			hb.ready[pr] = h.reachable && h.ready != nil && h.ready[pr]
		}
	}
	g.l.settle(g.now())
}

// poke wakes the start worker.
func (g *generation) poke() { wake(g.kick) }

// wake signals a coalescing channel of one.
func wake(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// runnerName is a new runner's name, unique within its scale set, and its
// container's: the pool, the profile and 12 random hex digits, at most 60
// bytes, inside GitHub's 64.
func (g *generation) runnerName(b *book) string {
	for {
		var r [6]byte
		_, _ = rand.Read(r[:])
		name := g.cfg.PoolName + "-" + b.ss.Profile + "-" + hex.EncodeToString(r[:])
		if _, taken := b.runners[name]; !taken {
			return name
		}
	}
}

// hot reports whether the budget left after every holding fits one more
// runner of some profile on a host that takes it: the sweep then reads
// every started run's jobs.
func (g *generation) hot() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	free := g.l.free()
	for pr := range g.l.profiles {
		if _, ok := g.l.fit(pr, free, nil); ok {
			return true
		}
	}
	return false
}
