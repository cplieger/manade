package pool

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// routeRetry spaces the routing of a scope whose last one failed.
const routeRetry = time.Minute

// Route is a scope's routing variable as the pool last wrote it: Value is
// empty once the pool deleted it.
type Route struct {
	Scope, Variable, Value string
}

// routeRecords is the store's record of each scope's routing variable as
// the pool last wrote it, which tells a hand-set value from the pool's.
type routeRecords interface {
	Routes(ctx context.Context) ([]Route, error)
	// RecordRoute replaces the record of rt's scope, compared without case.
	RecordRoute(ctx context.Context, rt Route, at time.Time) error
}

// RouteState is what the pool last read of a scope's routing variable.
type RouteState string

// The route states; the client words each.
const (
	// routePending: not read since the generation began.
	routePending RouteState = "pending"
	// routeSet: GitHub holds what the settings route to, the variable
	// absent for a scope that is stopping or public.
	routeSet RouteState = "set"
	// routeHandSet: a value the pool did not write, left alone.
	routeHandSet RouteState = "hand_set"
	// routeNamesNoProfile: a hand-set value naming no profile the scope
	// may use, whose jobs no runner takes.
	routeNamesNoProfile RouteState = "names_no_profile"
	// routeForbidden: the credential may not write variables, so the user
	// sets it by hand.
	routeForbidden RouteState = "forbidden"
	// routeFailed: the last read or write failed, retried every minute.
	routeFailed RouteState = "failed"
)

// RouteStatus is a scope's routing variable in the snapshot: its name, the
// value GitHub held at the last read (Found false when unset) and its state.
type RouteStatus struct {
	Scope, Variable, Value string
	State                  RouteState
	Found                  bool
}

// routing sets, rewrites or deletes the routing variable of each scope due:
// every repository and organization scope once per generation, a failed
// one routeRetry after its last try, and all of them when all is set, the
// visibility check's pass. A scope's stop ends only on counts read
// after its variable settled.
func (g *generation) routing(ctx context.Context, all bool, now time.Time) {
	recs, err := g.reg.routes(ctx)
	if err != nil {
		return
	}
	for _, j := range g.routesDue(all, now) {
		rec, recorded := recs[strings.ToLower(j.sc.cfg.Key)]
		var prev *Route
		if recorded {
			prev = &rec
		}
		st, err := g.route(ctx, j.sc.cfg, Route{Scope: j.sc.cfg.Key, Variable: g.cfg.RunnerVariable, Value: j.value}, prev)
		if ctx.Err() != nil {
			return
		}
		g.mu.Lock()
		g.routed(j.sc, &st, err, now)
		g.mu.Unlock()
	}
}

// routeJob is one scope's routing: its target value, empty for none.
type routeJob struct {
	sc    *scope
	value string
}

// routesDue answers the scopes routing takes now with their targets, and
// stamps their try.
func (g *generation) routesDue(all bool, now time.Time) []routeJob {
	g.mu.Lock()
	defer g.mu.Unlock()
	var due []routeJob
	for i := range g.cfg.Scopes {
		sc := g.scopes[g.cfg.Scopes[i].Key]
		if sc.cfg.Kind == config.ScopeEnterprise || !all && (!sc.routed.IsZero() || now.Sub(sc.routeTry) < routeRetry) {
			continue
		}
		sc.routeTry = now
		value := sc.cfg.DefaultProfile
		if sc.cfg.Stopping || sc.public {
			value = ""
		}
		due = append(due, routeJob{sc, value})
	}
	return due
}

// route brings scope's routing variable to want. GitHub's value is left
// alone when it differs from want while the pool's last write was want: a
// user set it by hand. Otherwise it is written, since want then is a change
// the user made to the row. A renamed variable is deleted under its old
// name where it still holds the pool's last value.
func (g *generation) route(ctx context.Context, scope *config.Scope, want Route, prev *Route) (RouteStatus, error) {
	st := RouteStatus{Scope: want.Scope, Variable: want.Variable, State: routeSet}
	cur, found, err := g.gh.REST.Variable(ctx, scope, want.Variable)
	if err != nil {
		return st, err
	}
	st.Value, st.Found = cur, found
	holds := found && cur == want.Value || !found && want.Value == ""
	if !holds && prev != nil && *prev == want {
		st.State = routeHandSet
		if found && !g.usableBy(scope.Key, cur) {
			st.State = routeNamesNoProfile
		}
		return st, nil
	}
	if !holds {
		if err := g.write(ctx, scope, want.Variable, want.Value); err != nil {
			return st, err
		}
		st.Value, st.Found = want.Value, want.Value != ""
	}
	if err := g.dropRenamed(ctx, scope, want, prev); err != nil {
		return st, err
	}
	if prev == nil || *prev != want {
		return st, g.reg.recordRoute(ctx, want)
	}
	return st, nil
}

// write sets scope's variable name to value, or deletes it for none.
func (g *generation) write(ctx context.Context, scope *config.Scope, name, value string) error {
	if value == "" {
		return g.gh.REST.DeleteVariable(ctx, scope, name)
	}
	return g.gh.REST.SetVariable(ctx, scope, name, value)
}

// dropRenamed deletes the variable prev names when want renamed it and it
// still holds the pool's last value; a value set by hand stays.
func (g *generation) dropRenamed(ctx context.Context, scope *config.Scope, want Route, prev *Route) error {
	if prev == nil || strings.EqualFold(prev.Variable, want.Variable) || prev.Value == "" {
		return nil
	}
	old, found, err := g.gh.REST.Variable(ctx, scope, prev.Variable)
	if err != nil || !found || old != prev.Value {
		return err
	}
	return g.gh.REST.DeleteVariable(ctx, scope, prev.Variable)
}

// usableBy reports a profile named name, without case as GitHub matches
// labels, that new jobs of the scope with key may use.
func (g *generation) usableBy(key, name string) bool {
	for i := range g.cfg.Profiles {
		if pr := &g.cfg.Profiles[i]; strings.EqualFold(pr.Name, name) {
			return pr.Usable(key)
		}
	}
	return false
}

// routed records a routing of sc begun at at; g.mu is held. A scope whose
// variable the credential may not write stays unsettled, so its stops hold
// until the user sets it by hand or uses Stop now.
func (g *generation) routed(sc *scope, st *RouteStatus, err error, at time.Time) {
	before := sc.route.State
	switch {
	case errors.Is(err, github.ErrVariablesForbidden):
		st.State = routeForbidden
	case err != nil:
		st.State = routeFailed
	case sc.routed.IsZero():
		sc.routed = at
	}
	sc.route = *st
	if st.State == before {
		return
	}
	switch st.State {
	case routeForbidden:
		g.log.Warn("The credential may not write the scope's runner variable, so set it by hand", "scope", sc.cfg.Key, "variable", st.Variable, "error", err)
	case routeFailed:
		g.log.Warn("Writing the scope's runner variable failed, so it is retried every minute", "scope", sc.cfg.Key, "variable", st.Variable, "error", err)
	case routeHandSet:
		g.log.Info("The scope's runner variable was set by hand, so the pool leaves it alone", "scope", sc.cfg.Key, "variable", st.Variable)
	case routeNamesNoProfile:
		g.log.Warn("The scope's runner variable was set by hand to no profile it may use, so its jobs get no runner", "scope", sc.cfg.Key, "variable", st.Variable)
	case routeSet, routePending:
	}
}

// routes answers the store's route records by scope without case, reading
// them on first use; a failed read is logged and tried again at the next
// routing.
func (r *Registry) routes(ctx context.Context) (map[string]Route, error) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.routeRecs != nil {
		return r.routeRecs, nil
	}
	got, err := r.d.Routes.Routes(ctx)
	if err != nil {
		r.d.Log.Warn("Reading the recorded runner variables failed, so no scope's variable is written until a read succeeds", "error", err)
		return nil, err
	}
	r.routeRecs = map[string]Route{}
	for _, rt := range got {
		r.routeRecs[strings.ToLower(rt.Scope)] = rt
	}
	return r.routeRecs, nil
}

// recordRoute writes rt to the store and the registry's copy.
func (r *Registry) recordRoute(ctx context.Context, rt Route) error {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if err := r.d.Routes.RecordRoute(ctx, rt, r.now()); err != nil {
		return err
	}
	r.routeRecs[strings.ToLower(rt.Scope)] = rt
	return nil
}

// routeStatuses answers every repository and organization scope's routing
// variable in settings order; g.mu is held.
func (g *generation) routeStatuses() []RouteStatus {
	var out []RouteStatus
	for i := range g.cfg.Scopes {
		sc := g.scopes[g.cfg.Scopes[i].Key]
		if sc.cfg.Kind == config.ScopeEnterprise {
			continue
		}
		st := sc.route
		if st.State == "" {
			st = RouteStatus{Scope: sc.cfg.Key, Variable: g.cfg.RunnerVariable, State: routePending}
		}
		out = append(out, st)
	}
	return out
}
