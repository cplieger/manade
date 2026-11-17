package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
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
	// rechecked is when the last pass over every scope began; born, when
	// the generation was built.
	rechecked, born time.Time
	reg             *Registry
	d               *Deps
	// gh is the credential's GitHub side, noCredential with none.
	gh       *GitHub
	cfg      *config.Config
	sections map[string]string
	sets     map[*book]*set
	now      func() time.Time
	kick     chan struct{}
	verify   chan struct{}
	// reensure wakes the ensure loop for a lost scale set.
	reensure chan struct{}
	scopes   map[string]*scope
	l        *ledger
	log      *slog.Logger
	// stopLoops ends the loops at their next pass boundary; nil until start.
	stopLoops context.CancelFunc
	// settled holds each stop handed to the settler.
	settled map[setKey]bool
	// departed holds, by scope key, the registration removals owed in
	// scopes the settings no longer hold, and gone those scopes' settings
	// while a stray or a removal names them.
	departed map[string]map[string]int64
	gone     map[string]*config.Scope
	hosts    []*host
	wg       sync.WaitGroup
	rev      uint64
	mu       sync.Mutex
	// booting holds health at starting until the first generation's boot
	// pass has adopted every runner its hosts list.
	booting, stopping bool
	// credentialed is false while the settings hold no credential.
	credentialed bool
}

func newGeneration(r *Registry, s *Settings) *generation {
	cfg := s.Config
	gh := s.GitHub
	if gh == nil {
		gh = noCredential
	}
	g := &generation{
		reg: r, d: &r.d, gh: gh, credentialed: s.GitHub != nil, cfg: cfg, sections: s.Sections, rev: s.Revision, log: r.d.Log, now: r.now,
		l: newLedger(cfg), booting: true, kick: make(chan struct{}, 1), verify: make(chan struct{}, 1), reensure: make(chan struct{}, 1),
		scopes: map[string]*scope{}, sets: map[*book]*set{}, settled: map[setKey]bool{}, departed: map[string]map[string]int64{}, gone: map[string]*config.Scope{}, born: r.now(),
	}
	for i := range cfg.Hosts {
		g.hosts = append(g.hosts, &host{cfg: &cfg.Hosts[i], want: make(chan struct{}, 1), wake: make(chan struct{}, 1)})
	}
	for i := range cfg.Scopes {
		// A scope this generation adds is read at once. An enterprise scope
		// has no routing variable, so it is routed from the start.
		sc := &scope{cfg: &cfg.Scopes[i], unregister: map[string]int64{}, wanted: true}
		if sc.cfg.Kind == config.ScopeEnterprise {
			sc.routed = g.born
		}
		g.scopes[cfg.Scopes[i].Key] = sc
	}
	return g
}

// scope is one scope's records.
type scope struct {
	// proof is when a read last proved the scope private; read is when its
	// last visibility read began, and failed when its reads began failing,
	// zero while they succeed. above is when a poll last reported capacity
	// above its assignments, which keeps the proof refreshed. readAt is
	// when the last complete read of its queued jobs ended.
	proof, read, failed, above, readAt time.Time
	// routed is when the scope's routing variable settled in this
	// generation, zero until then; routeTry, when its last routing began.
	routed, routeTry time.Time
	// ensureErr is why the scope's scale sets could not be ensured, nil
	// once they are; the ensure loop retries it.
	ensureErr error
	route     RouteStatus
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
	// holder is the owner GitHub named for a held session.
	holder string
	// opened reports a session opened; held, a session another process
	// holds; lost, a scale set GitHub no longer has, which the scope's
	// ensure finds or creates again.
	opened, held, lost bool
}

// keepSets records old's scale sets whose scope and profile g's settings
// still serve (config.Profile.Serves): a scale set is GitHub's record of a profile's name in a
// scope, which no other setting changes, so it needs no ensure again.
func (g *generation) keepSets(old *generation) {
	old.mu.Lock()
	defer old.mu.Unlock()
	for _, b := range old.l.books {
		pr, ok := g.cfg.Profile(b.ss.Profile)
		if g.scopes[b.ss.Scope] != nil && ok && pr.Serves(b.ss.Scope) {
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

// lacksSets reports a profile serving sc with no scale set recorded, or
// one of sc's scale sets lost.
func (g *generation) lacksSets(sc *scope) bool {
	for i := range g.cfg.Profiles {
		if g.cfg.Profiles[i].Serves(sc.cfg.Key) && g.bookOf(sc.cfg.Key, g.cfg.Profiles[i].Name) == nil {
			return true
		}
	}
	for _, s := range g.sets {
		if s.scope == sc && s.lost {
			return true
		}
	}
	return false
}

// ensureScope ensures sc's scale sets and records them, or the failure.
func (g *generation) ensureScope(ctx context.Context, sc *scope) {
	ectx, cancel := context.WithTimeout(ctx, ensureTimeout)
	sets, err := g.gh.ScaleSets.Ensure(ectx, g.cfg, sc.cfg)
	cancel()
	if err == nil {
		g.reg.record(ctx, sets)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		if sc.ensureErr == nil && ctx.Err() == nil && !errors.Is(err, errNoCredential) {
			g.log.Warn("Ensuring a scope's scale sets failed, so the scope is not served until a retry succeeds", "scope", sc.cfg.Key, "error", err)
		}
		sc.ensureErr = err
		return
	}
	sc.ensureErr = nil
	for _, ss := range sets {
		if b := g.bookOf(ss.Scope, ss.Profile); b == nil {
			g.addSet(ss)
		} else {
			g.found(b, ss)
		}
	}
	g.gate()
}

// found takes an ensure's answer for a scale set g holds; g.mu is held. A
// new ID is a scale set GitHub created again: a runner registered on the
// old one takes no job, so each is marked stale, and the listener's lease
// starts again for the session the new ID opens.
func (g *generation) found(b *book, ss github.ScaleSet) {
	s := g.sets[b]
	if b.ss.ID != ss.ID {
		b.ss.ID = ss.ID
		for _, rr := range b.runners {
			rr.stale = true
		}
	}
	if s.lost {
		s.lost, s.beat = false, g.now()
	}
}

// runEnsure retries each scope whose ensure failed every ensureRetry until
// ctx ends, and opens the sessions the scale sets it found need; it reads
// the scale-set record too while no read has succeeded.
func (g *generation) runEnsure(ctx context.Context) {
	t := time.NewTicker(ensureRetry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-g.reensure:
		}
		g.reg.record(ctx, nil)
		if g.retryEnsure(ctx) && ctx.Err() == nil {
			g.reg.sync()
		}
	}
}

// retryEnsure ensures again each scope whose last ensure failed or that
// lost a scale set, and reports whether any was due.
func (g *generation) retryEnsure(ctx context.Context) bool {
	g.mu.Lock()
	var due []*scope
	for i := range g.cfg.Scopes {
		if sc := g.scopes[g.cfg.Scopes[i].Key]; sc.ensureErr != nil || g.lacksSets(sc) {
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
// not hold under ss's ID: a closing session's late call never lands on
// the scale set an ensure found again.
func (g *generation) book(ss github.ScaleSet) (*book, *set) {
	b := g.bookOf(ss.Scope, ss.Profile)
	if b == nil || b.ss.ID != ss.ID {
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

// servedSets answers the sessions that should be open: one per scale set
// of a scope not proven public that GitHub still has, none without a
// credential.
func (g *generation) servedSets() map[setKey]want {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[setKey]want{}
	if !g.credentialed {
		return out
	}
	for b, s := range g.sets {
		if !s.scope.public && !s.lost {
			out[setKey{b.ss.Scope, b.ss.Profile}] = want{gh: g.gh, scope: s.scope.cfg, owner: g.cfg.PoolName, ss: b.ss}
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
			// A new credential reads every scope's visibility at once.
			sc.wanted = sc.wanted || old.gh != g.gh
		}
	}
	g.takeRemovals(old)
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
	g.keepGone(old)
	for i := range g.hosts {
		if g.hosts[i].st != nil {
			g.judge(i)
		}
	}
	g.gate()
}

// takeRemovals carries the registration removals old owed: into the scope
// g still holds, or as departed for one it dropped; both locks are held.
func (g *generation) takeRemovals(old *generation) {
	for key, names := range old.departed {
		if sc := g.scopes[key]; sc != nil {
			maps.Copy(sc.unregister, names)
		} else {
			g.departed[key] = names
		}
	}
	for key, o := range old.scopes {
		if g.scopes[key] == nil && len(o.unregister) > 0 {
			g.departed[key] = o.unregister
		}
	}
}

// keepGone keeps the settings of each scope g drops that a stray or an
// owed removal still names; both locks are held and the strays taken.
func (g *generation) keepGone(old *generation) {
	named := map[string]bool{}
	for _, st := range g.l.strays {
		named[st.scope] = true
	}
	for key := range g.departed {
		named[key] = true
	}
	for key := range named {
		switch {
		case g.scopes[key] != nil:
		case old.scopes[key] != nil:
			g.gone[key] = old.scopes[key].cfg
		case old.gone[key] != nil:
			g.gone[key] = old.gone[key]
		}
	}
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
		s.beat, s.retryAt, s.opened, s.held, s.lost, s.holder = os.beat, os.retryAt, os.opened, os.held, os.lost, os.holder
		if s.lost {
			wake(g.reensure)
		}
	}
	same := nb != nil && sameSpec(&old.cfg.Profiles[ob.profile], &g.cfg.Profiles[nb.profile])
	for name, r := range ob.runners {
		h := at[r.host]
		switch {
		case h < 0:
		case nb == nil:
			g.l.hold(name, &stray{created: r.created, res: r.res, host: h, profile: g.profileIndex(ob.ss.Profile), scope: ob.ss.Scope, recheck: true})
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
	sc.proof, sc.read, sc.failed, sc.above, sc.readAt = o.proof, o.read, o.failed, o.above, o.readAt
	sc.unregister, sc.jobs = o.unregister, o.jobs
	sc.partial, sc.public, sc.wanted, sc.route = o.partial, o.public, o.wanted, o.route
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
		b.serve = g.credentialed && !g.stopping && !s.scope.public && !s.held && !s.lost
		b.fundable = b.serve && s.scope.private() && !s.scope.partial
	}
	now := g.now()
	for i, h := range g.hosts {
		hb := &g.l.hosts[i]
		hb.reachable = h.reachable
		hb.serving = h.serving(now)
		for pr := range hb.ready {
			hb.ready[pr] = h.reachable && h.ready != nil && h.ready[pr]
		}
	}
	g.l.settle(now)
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
