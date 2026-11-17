package pool

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// scopeStatus answers the snapshot's entry of scope.
func scopeStatus(t *testing.T, snap *Snapshot, scope string) ScopeStatus {
	t.Helper()
	i := slices.IndexFunc(snap.Scopes, func(s ScopeStatus) bool { return s.Scope == scope })
	if i < 0 {
		t.Fatalf("scope %s is not in the snapshot: %+v", scope, snap.Scopes)
	}
	return snap.Scopes[i]
}

// A lasting 409 leaves the scope not served with the holder GitHub named
// on its row and beside each of its waiting jobs.
func TestAHeldScopeNamesItsHolder(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	snap := r.reg.Snapshot()
	if got := scopeStatus(t, &snap, app); !got.Serving || got.Detail != "" || len(got.Held) != 0 {
		t.Fatalf("scope %+v before any conflict, want served", got)
	}
	r.reg.Held(r.small().ss, true, "other-pool")
	r.listed(app, "small", 1)
	snap = r.reg.Snapshot()
	if got := scopeStatus(t, &snap, app); got.Serving || got.Detail != DetailHeldByOtherPool ||
		!slices.Equal(got.Held, []HeldSet{{Profile: "small", Owner: "other-pool"}}) {
		t.Errorf("scope %+v, want not served, held by other-pool on small", got)
	}
	if w := waitingRun(t, &snap, 1); w.Reason != JobScopeNotServed || w.Detail != DetailHeldByOtherPool || w.HeldBy != "other-pool" {
		t.Errorf("job %+v, want scope_not_served (held_by_other_pool) naming other-pool", w)
	}
	r.reg.Held(r.small().ss, false, "")
	snap = r.reg.Snapshot()
	if got := scopeStatus(t, &snap, app); !got.Serving || got.Detail != "" || len(got.Held) != 0 {
		t.Errorf("scope %+v once the session opened, want served with no holder", got)
	}
}

// Each scope's row reads why it is not served, the first that holds.
func TestAScopeRowReadsWhyItIsNotServed(t *testing.T) {
	t.Parallel()
	t.Run("public", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.gh.set(func(g *fakeGitHub) { g.public[app] = true })
		r.recheck()
		snap := r.reg.Snapshot()
		if got := scopeStatus(t, &snap, app); got.Serving || got.Detail != DetailPublic {
			t.Errorf("scope %+v, want not served, public", got)
		}
	})
	t.Run("noCredential", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.side = nil
		r.restart()
		snap := r.reg.Snapshot()
		if got := scopeStatus(t, &snap, app); got.Serving || got.Detail != DetailNotConnected {
			t.Errorf("scope %+v, want not served, github_not_connected", got)
		}
	})
	t.Run("ensureFailed", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.gh.set(func(g *fakeGitHub) { g.ensureFail = true })
		r.restart()
		snap := r.reg.Snapshot()
		if got := scopeStatus(t, &snap, app); got.Serving || got.Detail != DetailEnsureFailed {
			t.Errorf("scope %+v, want not served, ensure_failed", got)
		}
	})
}

// A session create answered not found makes the scope ensure its scale set
// again under its name, record the new ID and want its session under it; a
// runner registered on the lost scale set takes no job, so an idle one is
// replaced.
func TestALostScaleSetIsEnsuredAgainUnderItsNewID(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	r.pass()
	old := r.hosts[0].running()[0]
	lost := r.small().ss
	r.gh.set(func(g *fakeGitHub) { g.idBase, g.ensureFail = 40, true })
	r.reg.Lost(lost)
	// One job is the assignment the idle runner covers, 7 waits.
	r.listed(app, "small", 7, 8)
	snap := r.reg.Snapshot()
	if w := waitingRun(t, &snap, 7); w.Reason != JobScopeNotServed || w.Detail != DetailScaleSetLost {
		t.Errorf("job %+v while the ensure is due, want scope_not_served (scale_set_lost)", w)
	}
	if got := scopeStatus(t, &snap, app); got.Serving || got.Detail != DetailScaleSetLost {
		t.Errorf("scope %+v, want not served, scale_set_lost", got)
	}
	if _, ok := r.p.servedSets()[setKey{app, "small"}]; ok {
		t.Error("a lost scale set still wants a session under its old ID")
	}
	r.pass()
	snap = r.reg.Snapshot()
	if w := waitingRun(t, &snap, 7); w.Detail != DetailEnsureFailed {
		t.Errorf("job %+v once the ensure failed, want ensure_failed", w)
	}
	r.gh.set(func(g *fakeGitHub) { g.ensureFail = false })
	r.pass()
	b := r.small()
	if b.ss.ID != 41 {
		t.Fatalf("scale set %+v after the ensure, want the new ID 41", b.ss)
	}
	r.records.mu.Lock()
	rec := r.records.sets[keyOf(b.ss)]
	r.records.mu.Unlock()
	if rec.ID != 41 {
		t.Errorf("record %+v, want the new ID recorded", rec)
	}
	if w, ok := r.p.servedSets()[setKey{app, "small"}]; !ok || w.ss.ID != 41 {
		t.Errorf("session wanted %+v, %v; want one under the new ID", w.ss, ok)
	}
	r.assign(b, 1, 0)
	r.reg.Opened(lost, github.Stats{Assigned: 5})
	r.pass()
	if got := r.hosts[0].running(); len(got) != 1 || got[0] == old {
		t.Errorf("running %v, want the lost scale set's idle runner replaced by one registered on the new one", got)
	}
	snap = r.reg.Snapshot()
	for _, sc := range snap.Sets {
		if sc.Scope == app && sc.Profile == "small" && sc.AssignedUnstarted > 1 {
			t.Errorf("set %+v, want the old ID's late call dropped", sc)
		}
	}
	if got := scopeStatus(t, &snap, app); !got.Serving {
		t.Errorf("scope %+v once the scale set was found again, want served", got)
	}
}

// losingGitHub reports its scale set lost at its first session, as a
// create answered not found does, and serves the sessions after it.
type losingGitHub struct {
	*fakeGitHub
	opens []int
	mu    sync.Mutex
}

func (g *losingGitHub) Listen(ctx context.Context, _ *config.Scope, ss github.ScaleSet, _ string, h github.Handler) {
	g.mu.Lock()
	g.opens = append(g.opens, ss.ID)
	first := len(g.opens) == 1
	g.mu.Unlock()
	if first {
		h.Lost(ss)
		return
	}
	<-ctx.Done()
}

func (g *losingGitHub) opened() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.opens)
}

// A not found GitHub then contradicts, its ensure finding the scale set
// under the same ID, still reopens the session, at once and not at the
// ensure loop's next minute.
func TestALostScaleSetFoundUnderItsIDReopensItsSession(t *testing.T) {
	cfg := oneHost()
	gh := &losingGitHub{fakeGitHub: newFakeGitHub(cfg)}
	reg := NewRegistry(&Settings{
		Config: cfg, GitHub: &GitHub{ScaleSets: gh, REST: gh, Sweep: gh}, Revision: 1,
		Sections: map[string]string{"default": sectionHash(1)},
	}, &Deps{
		Links: &fakeLinks{hosts: []*fakeHost{newFakeHost(cfg, "default", sectionHash(1))}}, Metrics: newFakeMetrics(), Runs: newFakeRuns(), Hosts: newFakeHostLog(), Settle: &fakeSettler{},
		Records: newRecords(), Routes: newRecords(), Log: slog.New(slog.DiscardHandler),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, "the session reopened after the loss", func() bool { return len(gh.opened()) == 2 })
	if got := gh.opened(); !slices.Equal(got, []int{1, 1}) {
		t.Errorf("sessions opened under %v, want the same scale set reopened", got)
	}
}

// A scale set an ensure finds under a new ID, GitHub having created it
// again, has its open session closed and one opened under the new ID, so
// no session keeps polling the scale set GitHub deleted.
func TestASessionFollowsItsScaleSetToANewID(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	old := r.small().ss
	if r.sessions.usable(old) == nil {
		t.Fatalf("no session open on %+v before the ensure", old)
	}
	next := cloneConfig(r.cfg)
	next.Profiles = append(next.Profiles, config.Profile{Name: "large", Hosts: []string{"default"}, MilliCPUs: 4000, Memory: 8 * gib, MaxLifetime: 2 * time.Hour, IdleTimeout: idleFor})
	r.gh.set(func(g *fakeGitHub) { g.ids[setKey{app, "small"}] = 50 })
	r.apply(next)
	moved := r.small().ss
	if moved.ID != 50 {
		t.Fatalf("scale set %+v after the ensure, want the new ID 50", moved)
	}
	r.sessions.mu.Lock()
	for _, fs := range r.sessions.live {
		if fs.w.ss == old && !fs.closing {
			t.Errorf("the session on the old ID %d is still open", old.ID)
		}
	}
	r.sessions.mu.Unlock()
	if r.sessions.usable(moved) == nil {
		t.Errorf("no session open on %+v, want one under the new ID", moved)
	}
}

// tick runs one round of the ensure loop as runEnsure does.
func (r *rig) tick() {
	r.reg.record(context.Background(), nil)
	if r.p.retryEnsure(context.Background()) {
		r.reg.sync()
	}
}

// storedSet answers the store's record of scope's scale set for profile.
func (r *rig) storedSet(scope, profile string) github.ScaleSet {
	r.records.mu.Lock()
	defer r.records.mu.Unlock()
	return r.records.sets[setKey{strings.ToLower(scope), profile}]
}

func (g *fakeGitHub) ensureCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ensures
}

// A record write that fails after an ensure is retried at the ensure
// loop's next tick, with no second ensure, and an ensure answering the
// recorded scale set writes nothing. Once a settings change drops the
// scope, its scale set reads orphaned under the ID GitHub answered, both
// before the write succeeds and after, and its dropped record is not
// written again, whether its write was retried or not.
func TestAFailedRecordWriteIsRetriedAtTheNextTick(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	lost := r.small().ss
	r.gh.set(func(g *fakeGitHub) { g.idBase = 40 })
	r.records.mu.Lock()
	r.records.writeFails = 1
	r.records.mu.Unlock()
	r.reg.Lost(lost)
	r.pass()
	if id := r.small().ss.ID; id != 41 {
		t.Fatalf("held ID %d after the ensure, want 41", id)
	}
	if rec := r.storedSet(app, "small"); rec.ID != 1 {
		t.Fatalf("record %+v with the write refused, want the old ID 1 still stored", rec)
	}
	ensures := r.gh.ensureCount()
	r.tick()
	if rec := r.storedSet(app, "small"); rec.ID != 41 {
		t.Errorf("record %+v after the next tick, want the new ID 41", rec)
	}
	if n := r.gh.ensureCount() - ensures; n != 0 {
		t.Errorf("%d ensures at the tick, want the record written without one", n)
	}
	r.records.mu.Lock()
	writes := r.records.writes
	r.records.mu.Unlock()
	r.reg.Lost(r.small().ss)
	r.pass()
	r.tick()
	r.records.mu.Lock()
	if n := r.records.writes - writes; n != 0 {
		t.Errorf("%d record writes after an ensure answered the recorded scale set, want none", n)
	}
	r.records.mu.Unlock()
	want := []Orphan{{Scope: app, Profile: "small", ID: 41}}
	r.commit(func(cfg *config.Config) { cfg.Scopes = nil })
	if snap := r.reg.Snapshot(); !slices.Equal(snap.Orphaned, want) {
		t.Errorf("orphaned %+v once the scope is dropped, want %+v", snap.Orphaned, want)
	}
	r.gh.set(func(g *fakeGitHub) { g.gone[goneKey(app, 41)] = true })
	r.recheck()
	r.tick()
	if rec := r.storedSet(app, "small"); rec != (github.ScaleSet{}) {
		t.Errorf("record %+v after GitHub lost the orphan, want it dropped and not written again", rec)
	}

	r = quietRig(t, oneHost())
	r.gh.set(func(g *fakeGitHub) { g.idBase = 40 })
	r.reg.Lost(r.small().ss)
	r.pass()
	r.commit(func(cfg *config.Config) { cfg.Scopes = nil })
	r.gh.set(func(g *fakeGitHub) { g.gone[goneKey(app, 41)] = true })
	r.recheck()
	r.tick()
	if rec := r.storedSet(app, "small"); rec != (github.ScaleSet{}) {
		t.Errorf("record %+v after a written orphan GitHub lost was dropped, want it not written again", rec)
	}

	r = quietRig(t, oneHost())
	r.gh.set(func(g *fakeGitHub) { g.idBase = 40 })
	r.records.mu.Lock()
	r.records.writeFails = 1 << 20
	r.records.mu.Unlock()
	r.reg.Lost(r.small().ss)
	r.pass()
	r.commit(func(cfg *config.Config) { cfg.Scopes = nil })
	if snap := r.reg.Snapshot(); !slices.Equal(snap.Orphaned, want) {
		t.Errorf("orphaned %+v with the record unwritten, want %+v", snap.Orphaned, want)
	}
}

// An ensure whose record read fails has its scale sets written at the
// ensure loop's next tick once a read succeeds, with no second ensure.
func TestAnEnsureUnrecordedForAFailedReadIsWrittenAtTheNextTick(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.gh.set(func(g *fakeGitHub) { g.idBase = 40 })
	r.records.mu.Lock()
	r.records.fail = true
	r.records.mu.Unlock()
	r.restart()
	if id := r.small().ss.ID; id != 41 {
		t.Fatalf("held ID %d after the boot's ensure, want 41", id)
	}
	r.records.mu.Lock()
	r.records.fail = false
	r.records.mu.Unlock()
	ensures := r.gh.ensureCount()
	r.tick()
	if rec := r.storedSet(app, "small"); rec.ID != 41 {
		t.Errorf("record %+v after the next tick, want the boot's ensured ID 41", rec)
	}
	if n := r.gh.ensureCount() - ensures; n != 0 {
		t.Errorf("%d ensures at the tick, want the record written without one", n)
	}
}

// At the visibility check the pool drops, with its journal row, the record
// of each orphaned scale set GitHub no longer has, reading one of a scope
// the settings dropped under the scope its key names. A served one, one
// GitHub still has and one whose read fails are kept.
func TestTheVisibilityCheckDropsRecordsGitHubNoLongerHas(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	served := r.small().ss
	for _, ss := range []github.ScaleSet{{Scope: app, Profile: "old", ID: 7}, {Scope: "acme/web", Profile: "small", ID: 3}, {Scope: "acme/lib", Profile: "small", ID: 5}} {
		r.records.sets[keyOf(ss)] = ss
	}
	r.gh.set(func(g *fakeGitHub) {
		g.gone[goneKey(app, served.ID)], g.gone[goneKey(app, 7)] = true, true
		g.existsErr["acme/lib"] = true
	})
	r.restart()
	r.recheck()
	r.recheck()
	r.records.mu.Lock()
	kept := slices.Collect(func(yield func(github.ScaleSet) bool) {
		for _, ss := range r.records.sets {
			if !yield(ss) {
				return
			}
		}
	})
	drops := slices.Clone(r.records.drops)
	r.records.mu.Unlock()
	slices.SortFunc(kept, func(a, b github.ScaleSet) int { return strings.Compare(a.Scope+a.Profile, b.Scope+b.Profile) })
	if want := []github.ScaleSet{served, {Scope: "acme/lib", Profile: "small", ID: 5}, {Scope: "acme/web", Profile: "small", ID: 3}}; !slices.Equal(kept, want) {
		t.Errorf("records %+v, want %+v kept", kept, want)
	}
	if want := []github.ScaleSet{{Scope: app, Profile: "old", ID: 7}}; !slices.Equal(drops, want) {
		t.Errorf("drops %+v, want only the orphan GitHub no longer has", drops)
	}
	r.gh.mu.Lock()
	asked := slices.Clone(r.gh.asked)
	r.gh.mu.Unlock()
	if i := slices.IndexFunc(asked, func(s config.Scope) bool { return s.Key == "acme/web" }); i < 0 ||
		asked[i].Kind != config.ScopeRepo || asked[i].Owner != "acme" || asked[i].Repo != "web" {
		t.Errorf("reads %+v, want acme/web read as the repository its key names", asked)
	}
	r.gh.mu.Lock()
	readSets := slices.Clone(r.gh.askedSets)
	r.gh.mu.Unlock()
	if slices.Contains(readSets, served) {
		t.Errorf("reads %+v, want no read of a served scale set, whose loss its session finds", readSets)
	}
	if n := strings.Count(r.log.String(), "Reading a recorded scale set failed"); n != 1 {
		t.Errorf("failed-read WARN lines = %d over the boot's check and two more, want 1 while it fails", n)
	}
	snap := r.reg.Snapshot()
	if slices.ContainsFunc(snap.Orphaned, func(o Orphan) bool { return o.Profile == "old" }) {
		t.Errorf("orphans %+v, want the dropped record gone", snap.Orphaned)
	}
}
