package store

import (
	"maps"
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

// A record GitHub no longer has is dropped with its journal row in one
// transaction (§9.3, §5.3), and a record an ensure moved to another ID
// since is kept with no row.
func TestDroppingAScaleSetRecordJournalsIt(t *testing.T) {
	path := testPath(t)
	at := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	s := mustOpen(t, path, "v1.0.0-dev.1")
	if err := s.RecordScaleSets(t.Context(), []ScaleSet{
		{Scope: "acme/app", Profile: "small", ID: 1, EnsuredAt: at},
		{Scope: "acme/web", Profile: "small", ID: 4, EnsuredAt: at},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.DropScaleSet(t.Context(), ScaleSet{Scope: "ACME/App", Profile: "small", ID: 1}, at.Add(time.Minute)); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.DropScaleSet(t.Context(), ScaleSet{Scope: "acme/web", Profile: "small", ID: 3}, at.Add(2*time.Minute)); err != nil {
		t.Fatalf("drop of a moved record: %v", err)
	}
	mustClose(t, s)
	s = mustOpen(t, path, "v1.0.0-dev.1")
	defer mustClose(t, s)
	got, err := s.ScaleSets(t.Context())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := []ScaleSet{{Scope: "acme/web", Profile: "small", ID: 4, EnsuredAt: at}}; !slices.Equal(got, want) {
		t.Errorf("records %+v, want only the moved one kept", got)
	}
	rows := journalRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("journal = %+v, want one row for the one drop", rows)
	}
	e := rows[0]
	if !e.At.Equal(at.Add(time.Minute)) || e.Actor != (Actor{Kind: ActorPool}) || e.Action != ActionScaleSetRecordDropped ||
		e.Subject != "ACME/App" || e.Outcome != OutcomeDone || !maps.Equal(e.Facts, map[string]string{"profile": "small", "scale_set_id": "1"}) {
		t.Errorf("journal row %+v, want the pool's drop of acme/app small, scale set 1", e)
	}
}
