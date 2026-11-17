package pool

import (
	"slices"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// twoProfiles is oneHost with a second profile, large, on the same host,
// sorted by name as the parser sorts them.
func twoProfiles() *config.Config {
	return poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}}, []profileSpec{
		{name: "large", hosts: []string{"default"}, cpus: 4, mem: 8},
		{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4},
	}, app)
}

// commit applies edit to a copy of the rig's settings as the next
// revision, a second later.
func (r *rig) commit(edit func(cfg *config.Config)) {
	cfg := cloneConfig(r.cfg)
	edit(cfg)
	r.apply(cfg)
	r.now = r.now.Add(time.Second)
}

// quiet has GitHub report b with nothing queued or assigned and the last
// read list nothing for its scope.
func (r *rig) quiet(b *book) {
	r.poll(b, github.Stats{})
	r.listed(b.ss.Scope, b.ss.Profile)
}

func stopOf(t *testing.T, snap *Snapshot, scope, profile string) Stop {
	t.Helper()
	i := slices.IndexFunc(snap.Stops, func(s Stop) bool { return s.Scope == scope && s.Profile == profile })
	if i < 0 {
		t.Fatalf("no stop of %s %q among %+v", scope, profile, snap.Stops)
	}
	return snap.Stops[i]
}

// A scope turned off keeps serving its queued and assigned jobs, and ends
// only once they all ran: the settler is handed it once, and the written
// off scope's scale set then reads orphaned.
func TestAStoppingScopeServesItsJobsBeforeItEnds(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	b := r.small()
	r.busy(b)
	r.listed(app, "small", 1)
	r.commit(func(cfg *config.Config) { cfg.Scopes[0].Stopping = true })
	b = r.small()
	snap := r.reg.Snapshot()
	if st := stopOf(t, &snap, app, ""); st.Ended || st.Assigned != 1 || st.Queued != 1 {
		t.Errorf("stop %+v, want 1 queued and 1 assigned, not ended", st)
	}
	before := len(r.hosts[0].running())
	r.poll(b, github.Stats{Assigned: 2, Running: 1}, github.Event{Kind: github.Assigned, Name: "job", RunID: 1})
	r.starts()
	if got := len(r.hosts[0].running()); got != before+1 {
		t.Fatalf("%d runners after the stopping scope's job was assigned, want %d", got, before+1)
	}
	r.pass()
	if calls := r.settler.take(); len(calls) != 0 {
		t.Fatalf("settled %+v with jobs still assigned", calls)
	}
	r.quiet(b)
	r.pass()
	r.pass()
	calls := r.settler.take()
	if len(calls) != 1 || calls[0].rev != r.rev || len(calls[0].ended) != 1 || calls[0].ended[0].Scope != app {
		t.Fatalf("settled %+v, want the scope's stop once at revision %d", calls, r.rev)
	}
	r.commit(func(cfg *config.Config) { cfg.Scopes = nil })
	snap = r.reg.Snapshot()
	if want := []Orphan{{Scope: app, Profile: "small", ID: 1}}; !slices.Equal(snap.Orphaned, want) || len(snap.Stops) != 0 {
		t.Errorf("orphaned %+v, stops %+v; want the written-off scale set orphaned with nothing queued", snap.Orphaned, snap.Stops)
	}
}

// Counts read before the generation was built end nothing: a stop with
// nothing left still waits for a poll and a whole read after it began.
func TestAStopEndsOnlyOnCountsReadAfterItBegan(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.quiet(r.small())
	r.commit(func(cfg *config.Config) { cfg.Scopes[0].Stopping = true })
	r.pass()
	snap := r.reg.Snapshot()
	if st := stopOf(t, &snap, app, ""); st.Ended {
		t.Errorf("stop %+v ended on counts from before it began", st)
	}
	r.poll(r.small(), github.Stats{})
	r.pass()
	if calls := r.settler.take(); len(calls) != 0 {
		t.Errorf("settled %+v on a poll with no read of the queue since", calls)
	}
	r.listed(app, "small")
	r.pass()
	if calls := r.settler.take(); len(calls) != 1 {
		t.Errorf("settled %+v, want the stop once both reads came after it", calls)
	}
}

// The stopping state is the settings', so a new control plane reads it
// again and ends it the same way.
func TestAStoppingScopeResumesAfterAControlPlaneRestart(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.commit(func(cfg *config.Config) { cfg.Scopes[0].Stopping = true })
	r.restart()
	r.now = r.now.Add(time.Second)
	snap := r.reg.Snapshot()
	if st := stopOf(t, &snap, app, ""); st.Ended {
		t.Errorf("stop %+v ended at boot", st)
	}
	r.quiet(r.small())
	r.pass()
	if calls := r.settler.take(); len(calls) != 1 || calls[0].ended[0].Scope != app {
		t.Errorf("settled %+v, want the scope's stop after the restart", calls)
	}
}

// A profile leaving a scope keeps its scale set serving, which is not
// orphaned while its retiring entry stands; Stop now, the entry cleared
// and the scope taken from the profile, leaves exactly the named jobs
// queued on a scale set that then reads orphaned.
func TestStopNowLeavesTheNamedJobsOnAnOrphanedScaleSet(t *testing.T) {
	t.Parallel()
	r := quietRig(t, twoProfiles())
	r.commit(func(cfg *config.Config) { cfg.Profiles[1].Retiring = []string{app} })
	r.listed(app, "small", 1, 2)
	snap := r.reg.Snapshot()
	st := stopOf(t, &snap, app, "small")
	if len(snap.Orphaned) != 0 || st.Ended || st.Queued != 2 {
		t.Fatalf("orphaned %+v, stop %+v; want a retiring scale set serving its 2 jobs, not orphaned", snap.Orphaned, st)
	}
	var named []int64
	for _, j := range st.Jobs {
		named = append(named, j.RunID)
	}
	if !slices.Equal(named, []int64{1, 2}) {
		t.Errorf("the stop names runs %v, want 1 and 2", named)
	}
	r.commit(func(cfg *config.Config) { cfg.Profiles[1].Retiring, cfg.Profiles[1].Scopes = nil, []string{} })
	snap = r.reg.Snapshot()
	want := []Orphan{{Scope: app, Profile: "small", ID: 2, Queued: 2}}
	if !slices.Equal(snap.Orphaned, want) || len(snap.Stops) != 0 || r.small() != nil {
		t.Errorf("orphaned %+v, stops %+v; want small's scale set orphaned with its 2 jobs and no longer served", snap.Orphaned, snap.Stops)
	}
}

// Delete profile with jobs queued, then Stop now: the profile is gone at
// once, its scale set reads orphaned with its queue, and its name is free
// for a new profile, whose scale set is the same one served again.
func TestADeletedProfileStoppedNowFreesItsName(t *testing.T) {
	t.Parallel()
	r := quietRig(t, twoProfiles())
	r.commit(func(cfg *config.Config) {
		cfg.Profiles[1].Deleting, cfg.Profiles[1].Retiring = true, []string{app}
	})
	r.listed(app, "small", 3)
	snap := r.reg.Snapshot()
	if st := stopOf(t, &snap, app, "small"); st.Queued != 1 || r.small() == nil {
		t.Fatalf("stop %+v, want the deleting profile serving its 1 job", st)
	}
	deleted := cloneConfig(r.cfg)
	deleted.Profiles = deleted.Profiles[:1]
	r.apply(deleted)
	snap = r.reg.Snapshot()
	if want := []Orphan{{Scope: app, Profile: "small", ID: 2, Queued: 1}}; !slices.Equal(snap.Orphaned, want) {
		t.Errorf("orphaned %+v, want small's scale set with its 1 job", snap.Orphaned)
	}
	r.commit(func(cfg *config.Config) {
		cfg.Profiles = append(cfg.Profiles, config.Profile{Name: "small", Hosts: []string{"default"}, MilliCPUs: 1000, Memory: gib, MaxLifetime: time.Hour, IdleTimeout: idleFor})
	})
	if snap = r.reg.Snapshot(); len(snap.Orphaned) != 0 || r.small() == nil {
		t.Errorf("orphaned %+v with a new profile of the name, want its scale set served again", snap.Orphaned)
	}
}

// A profile leaving one scope while it serves another keeps the other's
// scale set out of the stop and out of the orphans.
func TestARetiringEntryStopsOnlyItsScope(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}}, []profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app, "acme/web")
	r := quietRig(t, cfg)
	r.commit(func(cfg *config.Config) { cfg.Profiles[0].Retiring = []string{"acme/web"} })
	r.quiet(r.book("acme/web", "small"))
	r.listed(app, "small", 1)
	r.pass()
	calls := r.settler.take()
	if len(calls) != 1 || len(calls[0].ended) != 1 || calls[0].ended[0].Scope != "acme/web" {
		t.Fatalf("settled %+v, want acme/web's stop alone", calls)
	}
	r.commit(func(cfg *config.Config) { cfg.Profiles[0].Retiring, cfg.Profiles[0].Scopes = nil, []string{app} })
	snap := r.reg.Snapshot()
	if len(snap.Orphaned) != 1 || snap.Orphaned[0].Scope != "acme/web" || r.small() == nil {
		t.Errorf("orphaned %+v, want acme/web's scale set alone, acme/app's still served", snap.Orphaned)
	}
}

// A scope the settings dropped leaves its runners to the next listing: an
// idle one is deregistered under the dropped scope's settings and removed,
// so it holds no budget for the rest of its lifetime.
func TestADroppedScopesIdleRunnerIsDeregisteredAndRemoved(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}}, []profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app, "acme/web")
	r := quietRig(t, cfg)
	r.assign(r.book("acme/web", "small"), 1, 0)
	r.pass()
	name := r.hosts[0].running()[0]
	id := r.hosts[0].runs[name].RunnerID
	r.commit(func(cfg *config.Config) { cfg.Scopes = cfg.Scopes[:1] })
	r.pass()
	if got := r.hosts[0].running(); len(got) != 0 || !r.gh.wasRemoved(id) {
		t.Errorf("runners %v, registration removed %v; want the idle runner of the dropped scope deregistered and removed", got, r.gh.wasRemoved(id))
	}
}

// Until the store's record of scale sets has been read, none reads as
// orphaned; the next read finds the record a failed one missed.
func TestOrphansWaitForTheRecordsRead(t *testing.T) {
	t.Parallel()
	r := quietRig(t, twoProfiles())
	r.records.mu.Lock()
	r.records.fail = true
	r.records.mu.Unlock()
	r.restart()
	r.commit(func(cfg *config.Config) { cfg.Profiles[1].Scopes = []string{} })
	if snap := r.reg.Snapshot(); len(snap.Orphaned) != 0 {
		t.Fatalf("orphaned %+v with the record unread", snap.Orphaned)
	}
	r.records.mu.Lock()
	r.records.fail = false
	r.records.mu.Unlock()
	r.reg.record(t.Context(), nil)
	if snap := r.reg.Snapshot(); len(snap.Orphaned) != 1 || snap.Orphaned[0].Profile != "small" {
		t.Errorf("orphaned %+v, want small's recorded scale set", snap.Orphaned)
	}
}
