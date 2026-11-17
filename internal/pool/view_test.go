package pool

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

func TestASnapshotCarriesWhatTheReadSeriesRead(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.pass()
	snap := r.reg.Snapshot()
	if !snap.At.Equal(r.now) || !snap.Booted || !slices.Equal(snap.Profiles, []string{"small"}) {
		t.Errorf("snapshot at %v booted %v profiles %v, want now, booted and small", snap.At, snap.Booted, snap.Profiles)
	}
	h := r.view("default")
	t.Run("a serving host", func(t *testing.T) {
		if !h.Connected || !h.Serving || !h.Accepting || !h.Current || h.EgressOpen || h.ProbeFailed {
			t.Errorf("view %+v, want connected, serving, accepting and current, egress closed and no failed probe", h)
		}
		if h.Status == nil || h.Seen != h.Status || h.Probe == nil || !h.Probe.Proven {
			t.Errorf("status %v seen %v probe %v, want the status read and the passing report", h.Status, h.Seen, h.Probe)
		}
		if !h.ProvedAt.Equal(r.now) || !h.ProbeUntil.IsZero() {
			t.Errorf("proved at %v probe until %v, want the proof's time and no probe running", h.ProvedAt, h.ProbeUntil)
		}
		if want := (Resources{MilliCPUs: 8000, Memory: 32 << 30}); h.Capacity == nil || *h.Capacity != want {
			t.Errorf("capacity %v, want %v", h.Capacity, want)
		}
		if !slices.Equal(h.Profiles, []string{"small"}) || h.ImageRefresh != r.cfg.Hosts[0].ImageRefresh {
			t.Errorf("profiles %v refresh %v, want small and the host's images.refresh", h.Profiles, h.ImageRefresh)
		}
		if !h.Reconciled.Equal(r.now) || !h.ReconcileDue.Equal(r.now.Add(ReconcileLease)) {
			t.Errorf("reconciled %v due %v, want the pass's end and its lease after it", h.Reconciled, h.ReconcileDue)
		}
	})
	t.Run("a scope's whole read and the read it named next", func(t *testing.T) {
		next := r.now.Add(10 * time.Second)
		r.p.queued(r.now, []github.Queue{{Scope: app, Complete: true, Next: next}})
		r.now = r.now.Add(time.Minute)
		r.p.queued(r.now, []github.Queue{{Scope: app, Next: r.now.Add(time.Hour)}})
		sc := r.reg.Snapshot().Scopes[0]
		if sc.Kind != config.ScopeRepo || !sc.ReadAt.Equal(r.now.Add(-time.Minute)) || !sc.ReadDue.Equal(next) {
			t.Errorf("scope %+v, want the whole read's end and next, kept through a partial read", sc)
		}
	})
	t.Run("a scale set's last good poll", func(t *testing.T) {
		r.poll(r.small(), github.Stats{})
		if sets := r.reg.Snapshot().Sets; len(sets) != 1 || !sets[0].Polled.Equal(r.now) {
			t.Errorf("sets %+v, want small's poll at now", sets)
		}
	})
}

func TestAHostViewWithoutAnAnswer(t *testing.T) {
	t.Parallel()
	t.Run("disconnected", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.hosts[0].set(func(f *fakeHost) { f.disconnected = true })
		r.pass()
		h := r.view("default")
		if h.Connected || h.Serving || h.Status != nil || h.Current || h.Seen == nil || h.Probe == nil {
			t.Errorf("view %+v, want disconnected, no status read, the last one seen and the last report kept", h)
		}
	})
	t.Run("connected and not answering", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.hosts[0].set(func(f *fakeHost) { f.down = true })
		r.pass()
		if h := r.view("default"); !h.Connected || h.Status != nil || h.Seen == nil {
			t.Errorf("view %+v, want connected with no status read and the last one seen", h)
		}
	})
	t.Run("a reported capacity before the first status", func(t *testing.T) {
		t.Parallel()
		cfg := oneHost()
		cfg.Hosts[0].Capacity = config.Capacity{}
		r := newRig(t, cfg)
		if h := r.view("default"); h.Capacity == nil || h.Capacity.MilliCPUs != 64000 {
			t.Errorf("capacity %v once the host reported, want its 64 CPUs", h.Capacity)
		}
		r.hosts[0].set(func(f *fakeHost) { f.disconnected = true })
		r.restart()
		if h := r.view("default"); h.Capacity != nil {
			t.Errorf("capacity %v before any report, want none", h.Capacity)
		}
	})
}

func TestAFailedProbeKeepsItsReport(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	failed := hostapi.ProbeReport{Checks: []hostapi.ProbeCheck{{Name: "job-gateway", Leg: "job", Required: true, Reason: "connected"}}}
	r.hosts[0].set(func(f *fakeHost) {
		f.probe = func(context.Context) (hostapi.ProbeReport, error) { return failed, nil }
	})
	r.restart()
	h := r.view("default")
	if !h.ProbeFailed || !h.ProvedAt.IsZero() || h.Probe == nil || len(h.Probe.Checks) != 1 {
		t.Errorf("view %+v, want the failed probe with its report and no proof", h)
	}
	r.hosts[0].restart(0)
	r.p.passHost(t.Context(), 0)
	if h := r.view("default"); h.ProbeFailed {
		t.Error("the failed probe of the host's earlier instance reads as the restarted host's")
	}
}

func TestAReconcileLeaseLeavesOutTheCallsItWaitsOn(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.pass()
	r.now = r.now.Add(2 * ReconcileLease)
	r.p.mu.Lock()
	r.p.hosts[0].waiting = true
	r.p.mu.Unlock()
	if h := r.view("default"); !h.ReconcileDue.Equal(r.now.Add(ReconcileLease)) {
		t.Errorf("due %v while the loop waits on a call, want a lease from now", h.ReconcileDue)
	}
}

func TestNoGenerationIsAnEmptySnapshot(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(&Settings{Config: oneHost()}, &Deps{})
	if snap := reg.Snapshot(); snap.Booted || len(snap.Hosts) != 0 {
		t.Errorf("snapshot %+v before Run, want not booted with no host", snap)
	}
}

// The recorded series' label sets follow each generation's settings, so
// they are handed over before the boot pass records anything under them.
func TestAGenerationsSettingsAreHandedOverBeforeItBoots(t *testing.T) {
	t.Parallel()
	r := newRig(t, poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}},
		[]profileSpec{{name: "small", hosts: []string{"default"}, cpus: 1, mem: 1}, {name: "large", hosts: []string{"default"}, cpus: 2, mem: 2}}, app))
	r.assign(r.book(app, "large"), 1, 0)
	r.pass()
	r.assign(r.book(app, "large"), 0, 0)
	cfg := cloneConfig(r.cfg)
	cfg.Profiles[0].MilliCPUs = 2000
	cfg.Profiles[1].MilliCPUs = 4000
	r.apply(cfg)
	r.m.mu.Lock()
	events := slices.Clone(r.m.events)
	r.m.mu.Unlock()
	at := slices.Index(events, "generation 2")
	exit := slices.IndexFunc(events, func(e string) bool {
		return e == "exit large "+string(ExitRemoved) || e == "exit large "+string(ExitIdle)
	})
	if at < 0 || exit < 0 || exit < at {
		t.Errorf("events %v, want generation 2 before its boot retires large's idle runner", events)
	}
}

func TestAGenerationChangeKeepsWhatTheReadSeriesRead(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.pass()
	r.poll(r.small(), github.Stats{})
	next := r.now.Add(10 * time.Second)
	r.p.queued(r.now, []github.Queue{{Scope: app, Complete: true, Next: next}})
	was := r.view("default")
	r.now = r.now.Add(time.Minute)
	r.apply(cloneConfig(r.cfg))
	snap := r.reg.Snapshot()
	if sc := snap.Scopes[0]; !sc.ReadAt.Equal(next.Add(-10*time.Second)) || !sc.ReadDue.Equal(next) {
		t.Errorf("scope %+v, want the last whole read and its next kept", sc)
	}
	if len(snap.Sets) != 1 || !snap.Sets[0].Polled.Equal(next.Add(-10*time.Second)) {
		t.Errorf("sets %+v, want small's last poll kept", snap.Sets)
	}
	h := r.view("default")
	if h.Probe != was.Probe || h.Seen == nil || !h.ProvedAt.Equal(was.ProvedAt) {
		t.Errorf("host %+v, want its report, status and proof kept", h)
	}
}

func TestAHostsPassStampsItsReconcile(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.now = r.now.Add(time.Minute)
	r.p.passHost(t.Context(), 0)
	if h := r.view("default"); !h.Reconciled.Equal(r.now) || !h.ReconcileDue.Equal(r.now.Add(ReconcileLease)) {
		t.Errorf("reconciled %v due %v, want the host's pass at %v and its lease after it", h.Reconciled, h.ReconcileDue, r.now)
	}
}

// A proof stands for one status: once the host restarts, the proof of its
// earlier instance no longer counts, before any new probe has run.
func TestAProofOfAnEarlierStatusIsNotTheHostsProof(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	if h := r.view("default"); h.ProvedAt.IsZero() {
		t.Fatalf("view %+v after boot, want the host proved", h)
	}
	r.hosts[0].restart(0)
	r.p.passHost(t.Context(), 0)
	if h := r.view("default"); !h.ProvedAt.IsZero() || h.ProbeFailed {
		t.Errorf("proved at %v failed %v for the restarted host, want no proof and no failure", h.ProvedAt, h.ProbeFailed)
	}
}
