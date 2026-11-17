package pool

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

const (
	web     = "acme/web"
	runnerV = "MANADE_RUNNER"
)

// twoScopes is twoProfiles serving app and web, each defaulting to small.
func twoScopes() *config.Config {
	cfg := twoProfiles()
	owner, repo, _ := strings.Cut(web, "/")
	cfg.Scopes = append(cfg.Scopes, config.Scope{Key: web, Owner: owner, Repo: repo, Kind: config.ScopeRepo})
	for i := range cfg.Scopes {
		cfg.Scopes[i].DefaultProfile = "small"
	}
	return cfg
}

func routeOf(t *testing.T, snap *Snapshot, scope string) RouteStatus {
	t.Helper()
	i := slices.IndexFunc(snap.Routes, func(r RouteStatus) bool { return r.Scope == scope })
	if i < 0 {
		t.Fatalf("no route of %s among %+v", scope, snap.Routes)
	}
	return snap.Routes[i]
}

// handSet sets scope's variable on GitHub as a user would, outside the pool.
func (r *rig) handSet(scope, name, value string) {
	r.gh.set(func(g *fakeGitHub) { g.vars[varKey(scope, name)] = value })
}

// recheck runs the visibility check's pass over every scope.
func (r *rig) recheck() {
	r.now = r.now.Add(recheckEvery)
	r.pass()
}

// A served scope's variable names its default profile once the pool
// boots, and the write is recorded as the pool's.
func TestAServedScopeRoutesToItsDefaultProfile(t *testing.T) {
	t.Parallel()
	r := quietRig(t, twoScopes())
	if got := r.gh.takeOps(); !slices.Equal(got, []string{"set acme/app MANADE_RUNNER small", "set acme/web MANADE_RUNNER small"}) {
		t.Errorf("variable writes %q, want each scope's set to small", got)
	}
	snap := r.reg.Snapshot()
	if got, want := routeOf(t, &snap, app), (RouteStatus{Scope: app, Variable: runnerV, Value: "small", Found: true, State: RouteSet}); got != want {
		t.Errorf("route %+v, want %+v", got, want)
	}
	if got := r.records.routes[app]; got != (Route{Scope: app, Variable: runnerV, Value: "small"}) {
		t.Errorf("record %+v, want the pool's write", got)
	}
	r.recheck()
	if got := r.gh.takeOps(); len(got) != 0 {
		t.Errorf("the visibility check wrote %q, want nothing", got)
	}
}

// Turning a row off deletes its variable first, so no new job is routed
// to the pool, and the scope keeps serving the jobs it holds.
func TestTurningARowOffDeletesTheVariableFirst(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.gh.takeOps()
	b := r.small()
	r.busy(b)
	r.commit(func(cfg *config.Config) { cfg.Scopes[0].Stopping = true })
	if got := r.gh.takeOps(); !slices.Equal(got, []string{"delete acme/app MANADE_RUNNER"}) {
		t.Errorf("variable writes %q, want the delete", got)
	}
	if _, ok := r.p.servedSets()[setKey{app, "small"}]; !ok {
		t.Error("the stopping scope's session closed, want it serving its assigned job")
	}
	snap := r.reg.Snapshot()
	if got := routeOf(t, &snap, app); got.State != RouteSet || got.Found {
		t.Errorf("route %+v, want the variable gone", got)
	}
}

// A stop ends only once its variable is gone: a failing delete, or a
// credential that may not write variables, holds the stop with nothing
// left to run, and the delete's retry a minute later frees it.
func TestAStopHoldsUntilItsVariableIsDeleted(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"failing":   fmt.Errorf("fake: GitHub answered 502"),
		"forbidden": fmt.Errorf("%w: fake", github.ErrVariablesForbidden),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := quietRig(t, oneHost())
			r.gh.set(func(g *fakeGitHub) { g.varErr = refusal })
			r.commit(func(cfg *config.Config) { cfg.Scopes[0].Stopping = true })
			r.quiet(r.small())
			r.pass()
			r.pass()
			if calls := r.settler.take(); len(calls) != 0 {
				t.Fatalf("settled %+v with the variable still routing jobs", calls)
			}
			snap := r.reg.Snapshot()
			if want := map[string]RouteState{"failing": RouteFailed, "forbidden": RouteForbidden}[name]; routeOf(t, &snap, app).State != want {
				t.Errorf("route %+v, want %s", routeOf(t, &snap, app), want)
			}
			r.gh.set(func(g *fakeGitHub) { g.varErr = nil })
			r.now = r.now.Add(routeRetry)
			r.pass()
			r.now = r.now.Add(time.Second)
			r.quiet(r.small())
			r.pass()
			if calls := r.settler.take(); len(calls) != 1 || calls[0].ended[0].Scope != app {
				t.Errorf("settled %+v, want the scope's stop once its variable was deleted", calls)
			}
			if _, set := r.gh.variable(app, runnerV); set {
				t.Error("the variable is still set")
			}
		})
	}
}

// A value set by hand is left alone, across a restart too, until the user
// changes that row; a hand-set value naming no profile the scope may use
// reads as such.
func TestAHandSetVariableIsLeftAlone(t *testing.T) {
	t.Parallel()
	r := quietRig(t, twoScopes())
	r.gh.takeOps()
	r.handSet(app, runnerV, "large")
	r.handSet(web, runnerV, "huge")
	r.recheck()
	r.restart()
	r.recheck()
	if got := r.gh.takeOps(); len(got) != 0 {
		t.Errorf("variable writes %q, want none over hand-set values", got)
	}
	snap := r.reg.Snapshot()
	if got := routeOf(t, &snap, app); got.State != RouteHandSet || got.Value != "large" {
		t.Errorf("app route %+v, want hand_set large", got)
	}
	if got := routeOf(t, &snap, web); got.State != RouteNamesNoProfile || got.Value != "huge" {
		t.Errorf("web route %+v, want names_no_profile huge", got)
	}
	r.commit(func(cfg *config.Config) { cfg.Scopes[1].DefaultProfile = "large" })
	if got := r.gh.takeOps(); !slices.Equal(got, []string{"set acme/web MANADE_RUNNER large"}) {
		t.Errorf("variable writes %q, want the changed row written", got)
	}
}

// A profile retiring from a scope whose default it is gets the scope's
// variable rewritten to the new default before its stop can end; a scope
// whose variable names the retiring profile by hand is left alone.
func TestRetiringADefaultRewritesTheVariableBeforeTheStop(t *testing.T) {
	t.Parallel()
	r := quietRig(t, twoScopes())
	r.commit(func(cfg *config.Config) { cfg.Scopes[0].DefaultProfile = "large" })
	r.handSet(web, runnerV, "large")
	r.gh.takeOps()
	r.commit(func(cfg *config.Config) {
		cfg.Scopes[0].DefaultProfile = "small"
		cfg.Profiles[0].Scopes, cfg.Profiles[0].Retiring = []string{}, []string{app, web}
	})
	if got := r.gh.takeOps(); !slices.Equal(got, []string{"set acme/app MANADE_RUNNER small"}) {
		t.Errorf("variable writes %q, want app rewritten to its new default", got)
	}
	if v, _ := r.gh.variable(web, runnerV); v != "large" {
		t.Errorf("web's hand-set variable = %q, want large left alone", v)
	}
	snap := r.reg.Snapshot()
	if got := routeOf(t, &snap, web); got.State != RouteNamesNoProfile {
		t.Errorf("web route %+v, want names_no_profile: large is leaving it", got)
	}
}

// Renaming the variable sets the new name on every served scope and
// deletes the old one only where it still holds the pool's value.
func TestRenamingTheVariable(t *testing.T) {
	t.Parallel()
	r := quietRig(t, twoScopes())
	r.handSet(web, runnerV, "large")
	r.gh.takeOps()
	r.commit(func(cfg *config.Config) { cfg.RunnerVariable = "CI_RUNNER" })
	want := []string{"set acme/app CI_RUNNER small", "delete acme/app MANADE_RUNNER", "set acme/web CI_RUNNER small"}
	if got := r.gh.takeOps(); !slices.Equal(got, want) {
		t.Errorf("variable writes %q, want %q", got, want)
	}
	if v, _ := r.gh.variable(web, runnerV); v != "large" {
		t.Errorf("web's hand-set old variable = %q, want large left alone", v)
	}
}
