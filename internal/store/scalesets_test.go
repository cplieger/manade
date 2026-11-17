package store

import (
	"slices"
	"testing"
	"time"
)

// A record outlives the process, and a scale set ensured again under the
// same scope, compared without case, and profile replaces its record.
func TestScaleSetRecordsReplaceAndPersist(t *testing.T) {
	path := testPath(t)
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := mustOpen(t, path, "v1.0.0-dev.1")
	if err := s.RecordScaleSets(t.Context(), []ScaleSet{
		{Scope: "acme/web", Profile: "small", ID: 3, EnsuredAt: at},
		{Scope: "acme/app", Profile: "small", ID: 1, EnsuredAt: at},
		{Scope: "acme/app", Profile: "large", ID: 2, EnsuredAt: at},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordScaleSets(t.Context(), []ScaleSet{{Scope: "ACME/App", Profile: "small", ID: 7, EnsuredAt: at.Add(time.Hour)}}); err != nil {
		t.Fatalf("record again: %v", err)
	}
	mustClose(t, s)
	s = mustOpen(t, path, "v1.0.0-dev.1")
	defer mustClose(t, s)
	got, err := s.ScaleSets(t.Context())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []ScaleSet{
		{Scope: "acme/app", Profile: "large", ID: 2, EnsuredAt: at},
		{Scope: "ACME/App", Profile: "small", ID: 7, EnsuredAt: at.Add(time.Hour)},
		{Scope: "acme/web", Profile: "small", ID: 3, EnsuredAt: at},
	}
	if !slices.Equal(got, want) {
		t.Errorf("records %+v, want %+v", got, want)
	}
}
