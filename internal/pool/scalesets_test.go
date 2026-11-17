package pool

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

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
// on its row and beside each of its waiting jobs (§9.3).
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
// replaced (§9.3).
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
	if h := r.reg.Health(); !h.Listening {
		t.Errorf("health %+v, want a lost scale set's ended session to count against no lease", h)
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
		Links: &fakeLinks{hosts: []*fakeHost{newFakeHost(cfg, "default", sectionHash(1))}}, Metrics: newFakeMetrics(), Settle: &fakeSettler{},
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

// At the visibility check the pool drops, with its journal row, the record
// of each orphaned scale set GitHub no longer has, reading one of a scope
// the settings dropped under the scope its key names. A served one, one
// GitHub still has and one whose read fails are kept (§9.3).
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
