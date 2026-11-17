package pool

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// HostAPI is one connected host's API.
type HostAPI interface {
	Status(ctx context.Context) (hostapi.Status, error)
	CreateRunner(ctx context.Context, req *hostapi.RunnerRequest) (hostapi.RunnerAnswer, error)
	Runners(ctx context.Context) (hostapi.RunnerList, error)
	DeleteRunner(ctx context.Context, name string, diag bool) ([]byte, error)
	Logs(ctx context.Context, name string, tail int) ([]byte, error)
	Probe(ctx context.Context, deadline time.Duration) (hostapi.ProbeReport, error)
	Events(ctx context.Context, fn func(*hostapi.Event)) error
}

// Links answers the API of each connected host by its settings name, and
// false while the host has no open connection.
type Links interface {
	Host(name string) (HostAPI, bool)
}

// scaleSets is the scale-set side the pool drives, one per credential:
// every call names its scope.
type scaleSets interface {
	Ensure(ctx context.Context, cfg *config.Config, scope *config.Scope) ([]github.ScaleSet, error)
	Listen(ctx context.Context, scope *config.Scope, ss github.ScaleSet, owner string, h github.Handler)
	JIT(ctx context.Context, scope *config.Scope, ss github.ScaleSet, name string) (github.JIT, error)
	RemoveRunner(ctx context.Context, scope *config.Scope, id int64) error
	Recheck(ctx context.Context) error
	Exists(ctx context.Context, scope *config.Scope, ss github.ScaleSet) (bool, error)
}

// restAPI is the REST reads the pool makes besides the queue's.
type restAPI interface {
	Private(ctx context.Context, s *config.Scope) (bool, error)
	Runner(ctx context.Context, s *config.Scope, id int64) (github.Runner, bool, error)
	Variable(ctx context.Context, s *config.Scope, name string) (value string, found bool, err error)
	SetVariable(ctx context.Context, s *config.Scope, name, value string) error
	DeleteVariable(ctx context.Context, s *config.Scope, name string) error
}

// scaleSetRecords is the store's record of every scale set the pool
// ensured, which tells its own retired scale sets from another system's.
type scaleSetRecords interface {
	ScaleSets(ctx context.Context) ([]github.ScaleSet, error)
	// RecordScaleSets writes each of sets, replacing the record of the same
	// scope and profile, in one transaction.
	RecordScaleSets(ctx context.Context, sets []github.ScaleSet, at time.Time) error
	// DropScaleSet drops the record of ss, one GitHub no longer has, with
	// its journal row in one transaction, unless the record names another
	// ID.
	DropScaleSet(ctx context.Context, ss github.ScaleSet, at time.Time) error
}

// sweeper reads the queued jobs of cfg's repository scopes until ctx ends.
type sweeper interface {
	Run(ctx context.Context, cfg *config.Config, hot func() bool, sink func(ended time.Time, qs []github.Queue))
}

// GitHub is one credential's GitHub side. A revision naming the same
// *GitHub as the one before keeps every session open; a different one is a
// credential change, which closes every session before any opens under it.
type GitHub struct {
	ScaleSets scaleSets
	REST      restAPI
	Sweep     sweeper
}

// Settings is one settings revision as the pool reads it: the model, the
// credential's GitHub side (nil with no credential, which serves no scope)
// and, by host name, the hash of the section the control plane delivers
// that host for this revision. A host serves only while it runs that
// section.
type Settings struct {
	Config   *config.Config
	GitHub   *GitHub
	Sections map[string]string
	Revision uint64
}

// Deps is what the pool drives; every field but Log, which may be nil for
// slog.Default, is required.
type Deps struct {
	Links   Links
	Metrics Recorder
	Settle  settler
	Records scaleSetRecords
	Routes  routeRecords
	Log     *slog.Logger
}

// Registry holds what outlives a pool generation (the host links, the
// GitHub client and each scale set's session) and the generation that
// serves the current settings revision. Its methods are safe for
// concurrent use.
type Registry struct {
	d   Deps
	cur *generation
	// spawn opens w's session; nil until Run.
	spawn   func(w want) *session
	pending *Settings
	// sessions holds each open scale-set session by scope and profile.
	sessions map[setKey]*session
	// recorded holds the store's scale-set records once read, nil before.
	recorded map[setKey]github.ScaleSet
	// routeRecs holds the store's route records by scope without case once
	// read, nil before.
	routeRecs map[string]Route
	// unreadable holds the recorded scale sets whose last read failed.
	unreadable map[setKey]bool
	changed    chan struct{}
	now        func() time.Time
	swg        sync.WaitGroup
	// mu guards cur: a handler call holds it for reading, so no call lands
	// in a generation after its successor copied it. smu guards spawn and
	// sessions, and is never held while a generation's lock is taken or a
	// session's close is waited on; syncMu serializes sync across that
	// wait. rmu guards recorded and routeRecs and serializes their writes.
	mu                    sync.RWMutex
	smu, syncMu, pmu, rmu sync.Mutex
}

// setKey names a scale set by scope and profile.
type setKey struct{ scope, profile string }

// session is one scale set's open session: what it was opened with, what
// ends it and done, closed once its close is done.
type session struct {
	cancel context.CancelFunc
	done   chan struct{}
	want
}

// want is a session a generation serves: its scale set, the scope's
// settings, the owner it opens under and the credential's GitHub side.
type want struct {
	gh    *GitHub
	scope *config.Scope
	owner string
	ss    github.ScaleSet
}

// NewRegistry builds the pool for its first settings revision. It calls
// nothing until Run.
func NewRegistry(s *Settings, d *Deps) *Registry {
	r := &Registry{d: *d, sessions: map[setKey]*session{}, unreadable: map[setKey]bool{}, changed: make(chan struct{}, 1), now: time.Now}
	r.d.Log = cmp.Or(r.d.Log, slog.Default())
	r.pending = s
	return r
}

// Run serves the settings revisions Apply hands it until ctx ends, then
// shuts the pool down: no poll reports capacity, every session closes,
// idle runners are removed and busy ones keep running for the next start
// to adopt. It returns once the shutdown is done or gave up.
func (r *Registry) Run(ctx context.Context) {
	calls, endCalls := context.WithCancel(context.WithoutCancel(ctx))
	defer endCalls()
	sessions, endSessions := context.WithCancel(calls)
	defer endSessions()
	r.listenUnder(sessions)
	r.record(calls, nil)
	r.install(calls, r.take()).run(calls)
	r.sync()
	for {
		select {
		case <-ctx.Done():
			r.current().shutdown(func() {
				endSessions()
				r.swg.Wait()
			})
			return
		case <-r.changed:
			if s := r.take(); s != nil {
				r.install(calls, s).run(calls)
				r.sync()
			}
		}
	}
}

// Apply hands the pool a committed settings revision. Changes are applied
// one at a time, and revisions handed over meanwhile are applied together
// from the latest.
func (r *Registry) Apply(s *Settings) {
	r.pmu.Lock()
	r.pending = s
	r.pmu.Unlock()
	wake(r.changed)
}

func (r *Registry) take() *Settings {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	s := r.pending
	r.pending = nil
	return s
}

// listenUnder runs every session the registry opens under ctx.
func (r *Registry) listenUnder(ctx context.Context) {
	r.smu.Lock()
	defer r.smu.Unlock()
	r.spawn = func(w want) *session {
		sctx, cancel := context.WithCancel(ctx)
		s := &session{want: w, cancel: cancel, done: make(chan struct{})}
		r.swg.Go(func() {
			defer close(s.done)
			w.gh.ScaleSets.Listen(sctx, w.scope, w.ss, w.owner, r)
		})
		return s
	}
}

func (r *Registry) current() *generation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

// install builds the generation for s and makes it current. Under the
// same credential it keeps the current generation's scale sets that s
// still serves; a new credential ensures every scope again, which finds
// the scale sets by name and proves it reaches each scope (§9.1). The
// ensures come first; then the current generation stops at a pass
// boundary and the new one takes over its records and boots through the
// boot path (§10.6). The caller starts the new generation's loops.
func (r *Registry) install(ctx context.Context, s *Settings) *generation {
	g := newGeneration(r, s)
	old := r.current()
	if old != nil && old.gh == g.gh {
		g.keepSets(old)
	}
	g.ensure(ctx)
	if old != nil {
		old.stop()
	}
	r.mu.Lock()
	if old != nil {
		old.mu.Lock()
		g.mu.Lock()
		g.takeOver(old)
		g.mu.Unlock()
		old.mu.Unlock()
	}
	r.cur = g
	r.mu.Unlock()
	g.boot(ctx)
	r.d.Metrics.Generation(s.Revision)
	return g
}

// sync opens a session for each scale set the current generation serves
// and closes the rest. After a credential change every session is closed,
// and each close done, before any opens under the new credential.
func (r *Registry) sync() {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	g := r.current()
	wants := g.servedSets()
	r.smu.Lock()
	var closing []*session
	for k, s := range r.sessions {
		if w, ok := wants[k]; !ok || w.ss.ID != s.ss.ID || w.owner != s.owner || w.gh != s.gh {
			s.cancel()
			delete(r.sessions, k)
			closing = append(closing, s)
		}
	}
	r.smu.Unlock()
	for _, s := range closing {
		if s.gh != g.gh {
			<-s.done
		}
	}
	r.smu.Lock()
	defer r.smu.Unlock()
	for k, w := range wants {
		if _, open := r.sessions[k]; open || r.spawn == nil {
			continue
		}
		r.sessions[k] = r.spawn(w)
	}
}

// closeScope closes every session of scope, which a public verdict needs
// at once.
func (r *Registry) closeScope(scope string) {
	r.smu.Lock()
	defer r.smu.Unlock()
	for k, s := range r.sessions {
		if k.scope == scope {
			s.cancel()
			delete(r.sessions, k)
		}
	}
}

var _ github.Handler = (*Registry)(nil)

// Capacity answers the poll's maxCapacity from the current generation, 0
// for a scale set it does not hold.
func (r *Registry) Capacity(ss github.ScaleSet, start time.Time) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur.capacity(ss, start)
}

// Opened hands a new session's statistics to the current generation.
func (r *Registry) Opened(ss github.ScaleSet, st github.Stats) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	r.cur.opened(ss, st)
}

// Polled hands an answered poll to the current generation.
func (r *Registry) Polled(ss github.ScaleSet, p *github.Poll) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	r.cur.polled(ss, p)
}

// Held records whether another process holds the scale set's session, and
// the holder GitHub named.
func (r *Registry) Held(ss github.ScaleSet, held bool, owner string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	r.cur.held(ss, held, owner)
}

// Lost records that GitHub no longer has the scale set and forgets its
// ended session, so the session reopens once the scope's ensure answers,
// under the same ID too.
func (r *Registry) Lost(ss github.ScaleSet) {
	r.mu.RLock()
	r.cur.lost(ss)
	r.mu.RUnlock()
	r.smu.Lock()
	defer r.smu.Unlock()
	k := setKey{ss.Scope, ss.Profile}
	if s := r.sessions[k]; s != nil && s.ss.ID == ss.ID {
		s.cancel()
		delete(r.sessions, k)
	}
}

// Beat refreshes the scale set's listener lease.
func (r *Registry) Beat(ss github.ScaleSet) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	r.cur.beat(ss)
}

// Health reads the current generation's health; the zero Health before
// Run installs one.
func (r *Registry) Health() Health {
	g := r.current()
	if g == nil {
		return Health{}
	}
	return g.health()
}

// Snapshot is the current generation's state for the pool's read series
// and the API; empty before Run installs one.
func (r *Registry) Snapshot() Snapshot {
	g := r.current()
	if g == nil {
		return Snapshot{}
	}
	return g.snapshot()
}

// record writes the scale sets an ensure found or created into the
// store's record, reading the record first if no read has succeeded. A
// failed read or write is logged and retried at the next ensure.
func (r *Registry) record(ctx context.Context, sets []github.ScaleSet) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.recorded == nil {
		got, err := r.d.Records.ScaleSets(ctx)
		if err != nil {
			r.d.Log.Warn("Reading the recorded scale sets failed, so none reads as orphaned until a read succeeds", "error", err)
			return
		}
		r.recorded = map[setKey]github.ScaleSet{}
		for _, ss := range got {
			r.recorded[keyOf(ss)] = ss
		}
	}
	var fresh []github.ScaleSet
	for _, ss := range sets {
		if r.recorded[keyOf(ss)] != ss {
			fresh = append(fresh, ss)
		}
	}
	if len(fresh) == 0 {
		return
	}
	if err := r.d.Records.RecordScaleSets(ctx, fresh, r.now()); err != nil {
		r.d.Log.Warn("Recording ensured scale sets failed, so the next ensure records them", "error", err)
		return
	}
	for _, ss := range fresh {
		r.recorded[keyOf(ss)] = ss
	}
}

// prune reads each orphaned scale set under g's credential and drops the
// record of one GitHub no longer has (§9.3). A served one's loss is found
// by its session; a failed read keeps the record and is logged once while
// it fails.
func (r *Registry) prune(ctx context.Context, g *generation) {
	for _, ss := range r.orphaned(g.cfg) {
		scope := g.settingsOf(ss.Scope)
		found, err := g.gh.ScaleSets.Exists(ctx, &scope, ss)
		if ctx.Err() != nil {
			return
		}
		r.rmu.Lock()
		k := keyOf(ss)
		if err != nil && !r.unreadable[k] {
			r.d.Log.Warn("Reading a recorded scale set failed, so its record is kept until a read tells", "scope", ss.Scope, "profile", ss.Profile, "id", ss.ID, "error", err)
		}
		if r.unreadable[k] = err != nil; err == nil {
			delete(r.unreadable, k)
		}
		r.rmu.Unlock()
		if err == nil && !found {
			r.drop(ctx, ss)
		}
	}
}

// drop removes ss's record, with its journal row, unless an ensure has
// recorded another ID for it since it was read.
func (r *Registry) drop(ctx context.Context, ss github.ScaleSet) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.recorded[keyOf(ss)] != ss {
		return
	}
	if err := r.d.Records.DropScaleSet(ctx, ss, r.now()); err != nil {
		r.d.Log.Warn("Dropping the record of a scale set GitHub no longer has failed, so the next check drops it", "scope", ss.Scope, "profile", ss.Profile, "error", err)
		return
	}
	delete(r.recorded, keyOf(ss))
	r.d.Log.Info("GitHub no longer has an orphaned scale set, so its record is dropped", "scope", ss.Scope, "profile", ss.Profile, "id", ss.ID)
}

// orphaned answers the recorded scale sets cfg would not ensure (§9.3),
// sorted by scope and profile; none before the record is read.
func (r *Registry) orphaned(cfg *config.Config) []github.ScaleSet {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	var out []github.ScaleSet
	for _, ss := range r.recorded {
		pr, ok := cfg.Profile(ss.Profile)
		if !slices.ContainsFunc(cfg.Scopes, func(s config.Scope) bool { return strings.EqualFold(s.Key, ss.Scope) }) || !ok || !pr.Serves(ss.Scope) {
			out = append(out, ss)
		}
	}
	slices.SortFunc(out, func(a, b github.ScaleSet) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Profile, b.Profile))
	})
	return out
}

// keyOf names a scale set's record: its scope, without case, and profile.
func keyOf(ss github.ScaleSet) setKey { return setKey{strings.ToLower(ss.Scope), ss.Profile} }
