package pool

import (
	"context"
	"slices"
	"testing"

	"github.com/cplieger/manade/internal/hostapi"
)

// A host's state reaches the history once a pass judged it and again at
// each change, never while it holds, a new revision included: proving
// until its first proof, ready, disconnected, ready again on the same
// proof, draining.
func TestAHostsStateIsHandedOverAtEachChange(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.pass()
	r.apply(cloneConfig(r.cfg))
	r.pass()
	want := []string{"proving", "ready"}
	if got := r.hostLog.statesOf("default"); !slices.Equal(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = true })
	r.pass()
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = false })
	r.pass()
	drained(r, "default", false)
	r.pass()
	want = append(want, "disconnected", "ready", "draining")
	if got := r.hostLog.statesOf("default"); !slices.Equal(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}
	if r.hostLog.statuses["default"] == 0 {
		t.Error("no status reached the history")
	}
}

// A state that names its cause carries it: the failed proof's check, and
// the section state of a host that does not run the section delivered.
func TestAHostsStateCarriesItsDetail(t *testing.T) {
	t.Parallel()
	t.Run("unproved", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.hosts[0].set(func(f *fakeHost) {
			f.probe = func(context.Context) (hostapi.ProbeReport, error) {
				return hostapi.ProbeReport{Checks: []hostapi.ProbeCheck{{Name: "job-gateway", Leg: "job", Target: "172.18.0.1:7464", Required: true, Reason: "connected"}}}, nil
			}
		})
		r.restart()
		r.pass()
		got := r.hostLog.statesOf("default")
		if want := "unproved/job-gateway (job leg, 172.18.0.1:7464): connected"; len(got) == 0 || got[len(got)-1] != want {
			t.Errorf("states = %v, want the last %q", got, want)
		}
	})
	t.Run("settingsNotApplied", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.apply(cloneConfig(r.cfg), "default")
		r.pass()
		got := r.hostLog.statesOf("default")
		if want := "settings_not_applied/" + string(hostapi.SectionWaiting); len(got) == 0 || got[len(got)-1] != want {
			t.Errorf("states = %v, want the last %q", got, want)
		}
	})
}

// A host the settings drop ends its track with a gap.
func TestARemovedHostEndsItsTrackWithAGap(t *testing.T) {
	t.Parallel()
	r := quietRig(t, poolConfig(twoHosts()))
	r.pass()
	cfg := cloneConfig(r.cfg)
	cfg.Hosts = cfg.Hosts[:1]
	for i := range cfg.Profiles {
		cfg.Profiles[i].Hosts = []string{"default"}
	}
	r.apply(cfg)
	r.pass()
	got := r.hostLog.statesOf("nas")
	if len(got) == 0 || got[len(got)-1] != "/"+DetailHostRemoved {
		t.Errorf("nas's states = %v, want a gap with %q last", got, DetailHostRemoved)
	}
	if got := r.hostLog.statesOf("default"); slices.Contains(got, "/"+DetailHostRemoved) {
		t.Errorf("the kept host's states = %v, want no gap", got)
	}
}

// Each proof that completes reaches the history with its report and
// verdict, a failed request with no report.
func TestEachProofReachesTheHistory(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) {
		f.probe = func(context.Context) (hostapi.ProbeReport, error) {
			return hostapi.ProbeReport{}, hostapi.ErrProbeTimeout
		}
	})
	wake(r.p.hosts[0].want)
	r.prove()
	r.hostLog.mu.Lock()
	defer r.hostLog.mu.Unlock()
	ps := r.hostLog.proofs
	if len(ps) != 2 {
		t.Fatalf("proofs = %+v, want the boot's and the failed request", ps)
	}
	if ps[0].rep == nil || !ps[0].rep.Proven || ps[0].failure != "" {
		t.Errorf("first proof = %+v, want its passing report", ps[0])
	}
	if ps[1].rep != nil || ps[1].failure == "" || ps[1].host != "default" {
		t.Errorf("second proof = %+v, want no report and the request's failure", ps[1])
	}
}
