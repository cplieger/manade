package main

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostlink"
	"github.com/cplieger/manade/internal/obs"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), store.File), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// What the pool records of its scale sets reads back as it wrote it, and a
// drop removes only the record of the ID it names.
func TestScaleSetRecordsRoundTrip(t *testing.T) {
	st := openStore(t)
	recs := scaleSetRecords{st}
	at := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	sets := []github.ScaleSet{{Scope: "acme/app", Profile: "small", ID: 7}, {Scope: "acme/web", Profile: "large", ID: 9}}
	if err := recs.RecordScaleSets(t.Context(), sets, at); err != nil {
		t.Fatal(err)
	}
	got, err := recs.ScaleSets(t.Context())
	if err != nil || !slices.Equal(got, sets) {
		t.Errorf("ScaleSets() = %v, %v; want %v", got, err, sets)
	}
	stored, err := st.ScaleSets(t.Context())
	if err != nil || len(stored) != 2 || !stored[0].EnsuredAt.Equal(at) {
		t.Errorf("the store holds %+v, %v; want each ensured at %v", stored, err, at)
	}
	if err := recs.DropScaleSet(t.Context(), github.ScaleSet{Scope: "acme/app", Profile: "small", ID: 8}, at); err != nil {
		t.Fatal(err)
	}
	if err := recs.DropScaleSet(t.Context(), sets[1], at); err != nil {
		t.Fatal(err)
	}
	if got, err := recs.ScaleSets(t.Context()); err != nil || !slices.Equal(got, sets[:1]) {
		t.Errorf("ScaleSets() after the drops = %v, %v; want %v", got, err, sets[:1])
	}
}

func TestRouteRecordsRoundTrip(t *testing.T) {
	st := openStore(t)
	recs := routeRecords{st}
	at := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	rt := pool.Route{Scope: "acme/app", Variable: "MANADE_RUNNER", Value: "small"}
	if err := recs.RecordRoute(t.Context(), rt, at); err != nil {
		t.Fatal(err)
	}
	got, err := recs.Routes(t.Context())
	if err != nil || !slices.Equal(got, []pool.Route{rt}) {
		t.Errorf("Routes() = %v, %v; want %v", got, err, rt)
	}
	stored, err := st.Routes(t.Context())
	if err != nil || len(stored) != 1 || !stored[0].WrittenAt.Equal(at) {
		t.Errorf("the store holds %+v, %v; want it written at %v", stored, err, at)
	}
}

// A host with no connection answers a nil interface, which the pool reads
// as disconnected.
func TestHostLinksOfAnUnconnectedHost(t *testing.T) {
	api, ok := hostLinks{hostlink.NewServer(slog.New(slog.DiscardHandler))}.Host("default")
	if ok || api != nil {
		t.Errorf("Host(default) = %v, %v; want nil and false", api, ok)
	}
}

func TestGitHubSideFollowsTheCredential(t *testing.T) {
	m, err := obs.New(version, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if gh, err := githubSide(&config.GitHub{}, m, nil); gh != nil || err != nil {
		t.Errorf("githubSide(no credential) = %v, %v; want nil", gh, err)
	}
	t.Setenv("GITHUB_TOKEN", "test-token")
	cp, err := config.LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	gh, err := githubSide(&cp.GitHub, m, nil)
	if err != nil || gh == nil || gh.ScaleSets == nil || gh.REST == nil || gh.Sweep == nil {
		t.Errorf("githubSide(token) = %+v, %v; want every part", gh, err)
	}
}
