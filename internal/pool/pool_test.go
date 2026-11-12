package pool

import (
	"testing"

	"github.com/cplieger/manade/internal/github"
)

func TestHot(t *testing.T) {
	fx := newFixture(t)
	if !fx.p.Hot() {
		t.Error("Hot = false on an empty pool")
	}
	fx.polled(libDefault, fx.at, &github.Stats{Assigned: 4})
	if fx.p.Hot() {
		t.Error("Hot = true with every slot backing an assignment")
	}
}

func TestSnapshotCounts(t *testing.T) {
	fx := newFixture(t)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 3})
	if c := counts(fx.p.Snapshot(), appDefault); c.AssignedUnstarted != 3 {
		t.Errorf("assigned unstarted = %d, want 3", c.AssignedUnstarted)
	}
	fx.startAll(t)
	var name string
	fx.p.mu.Lock()
	for n := range fx.p.set(appDefault).runners {
		name = n
		break
	}
	fx.p.mu.Unlock()
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 3, Running: 1}, github.Event{Kind: github.Started, Runner: name, RunID: 1, Name: "lint"})
	c := counts(fx.p.Snapshot(), appDefault)
	if c.AssignedUnstarted != 0 || c.Idle != 2 || c.Busy != 1 || c.Starting != 0 {
		t.Errorf("counts = %+v, want 2 idle, 1 busy and nothing unstarted", c)
	}
}

func TestAnAllowedProfileWithNoScaleSetIsNotServing(t *testing.T) {
	fx := newFixtureFrom(t, load(t, layout), appDefault, libDefault, orgDefault)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("large", 1, "big", 0)}})
	if c := counts(fx.p.Snapshot(), appLarge); c.NotServing != 1 || c.NotAllowed != 0 {
		t.Errorf("app large counts = %+v, want 1 not_serving", c)
	}
}

func TestRepoStandingSlotsGoToTheMostRecentlyAssigned(t *testing.T) {
	// lib holds two slots and the org's standing slot takes a third, so one
	// standing slot is left for a repo scale set.
	fx := newFixture(t)
	fx.polled(libDefault, fx.at, &github.Stats{Assigned: 2}, assigned(1, "a"), assigned(2, "b"))
	fx.capacity(orgDefault)
	if got := fx.capacity(appDefault); got != 0 {
		t.Errorf("app capacity = %d, want 0: lib was assigned more recently", got)
	}
	if got := fx.capacity(libDefault); got != 3 {
		t.Errorf("lib capacity = %d, want its 2 slots and the standing one", got)
	}
}
