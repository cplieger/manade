package store

import (
	"slices"
	"testing"
	"time"
)

// A route record outlives the process, and one written again for the same
// scope, compared without case, replaces it.
func TestRouteRecordsReplaceAndPersist(t *testing.T) {
	path := testPath(t)
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s := mustOpen(t, path, "v1.0.0-dev.1")
	for _, rt := range []Route{
		{Scope: "acme/web", Variable: "MANADE_RUNNER", Value: "small", WrittenAt: at},
		{Scope: "acme/app", Variable: "MANADE_RUNNER", Value: "small", WrittenAt: at},
		{Scope: "ACME/App", Variable: "MANADE_RUNNER", Value: "", WrittenAt: at.Add(time.Hour)},
	} {
		if err := s.RecordRoute(t.Context(), &rt); err != nil {
			t.Fatalf("record %+v: %v", rt, err)
		}
	}
	mustClose(t, s)
	s = mustOpen(t, path, "v1.0.0-dev.1")
	defer mustClose(t, s)
	got, err := s.Routes(t.Context())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []Route{
		{Scope: "ACME/App", Variable: "MANADE_RUNNER", Value: "", WrittenAt: at.Add(time.Hour)},
		{Scope: "acme/web", Variable: "MANADE_RUNNER", Value: "small", WrittenAt: at},
	}
	if !slices.Equal(got, want) {
		t.Errorf("records %+v, want %+v", got, want)
	}
}
