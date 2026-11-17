package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostlink"
	"github.com/cplieger/manade/internal/obs"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

// githubSide is the pool's GitHub side under the environment's credential,
// nil with none, which serves no scope.
func githubSide(gh *config.GitHub, m *obs.Metrics, log *slog.Logger) (*pool.GitHub, error) {
	if !gh.Set() {
		return nil, nil
	}
	tr, err := github.NewTransport(gh.Proxy)
	if err != nil {
		return nil, err
	}
	rest := github.NewREST(gh, tr, m, log)
	return &pool.GitHub{ScaleSets: github.NewScaleSets(gh, tr, rest, version), REST: rest, Sweep: github.NewSweep(rest)}, nil
}

// hostLinks answers the pool each connected host's API.
type hostLinks struct{ srv *hostlink.Server }

var _ pool.Links = hostLinks{}

func (h hostLinks) Host(name string) (pool.HostAPI, bool) {
	l, ok := h.srv.Link(name)
	if !ok {
		// A nil *hostapi.Client in the interface would not be nil.
		return nil, false
	}
	return l.API, true
}

// scaleSetRecords is the store's scale-set records as the pool reads them.
type scaleSetRecords struct{ st *store.Store }

func (r scaleSetRecords) ScaleSets(ctx context.Context) ([]github.ScaleSet, error) {
	recs, err := r.st.ScaleSets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]github.ScaleSet, len(recs))
	for i, rec := range recs {
		out[i] = github.ScaleSet{Scope: rec.Scope, Profile: rec.Profile, ID: rec.ID}
	}
	return out, nil
}

func (r scaleSetRecords) RecordScaleSets(ctx context.Context, sets []github.ScaleSet, at time.Time) error {
	recs := make([]store.ScaleSet, len(sets))
	for i, ss := range sets {
		recs[i] = store.ScaleSet{EnsuredAt: at, Scope: ss.Scope, Profile: ss.Profile, ID: ss.ID}
	}
	return r.st.RecordScaleSets(ctx, recs)
}

func (r scaleSetRecords) DropScaleSet(ctx context.Context, ss github.ScaleSet, at time.Time) error {
	return r.st.DropScaleSet(ctx, store.ScaleSet{Scope: ss.Scope, Profile: ss.Profile, ID: ss.ID}, at)
}

// routeRecords is the store's route records as the pool reads them.
type routeRecords struct{ st *store.Store }

func (r routeRecords) Routes(ctx context.Context) ([]pool.Route, error) {
	recs, err := r.st.Routes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]pool.Route, len(recs))
	for i, rec := range recs {
		out[i] = pool.Route{Scope: rec.Scope, Variable: rec.Variable, Value: rec.Value}
	}
	return out, nil
}

func (r routeRecords) RecordRoute(ctx context.Context, rt pool.Route, at time.Time) error {
	return r.st.RecordRoute(ctx, &store.Route{WrittenAt: at, Scope: rt.Scope, Variable: rt.Variable, Value: rt.Value})
}

// settleLog logs each stop the pool read as ended: with no settings store
// yet (D23), nothing can write the stop off, so the pool keeps it.
type settleLog struct{ log *slog.Logger }

func (s settleLog) Settle(rev uint64, ended []pool.Stop) {
	for _, st := range ended {
		s.log.Info("A stopping scope or profile has no queued or assigned job left", "revision", rev, "scope", st.Scope, "profile", st.Profile)
	}
}
