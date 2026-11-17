package history

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

// events answers host's events, oldest first, each as kind, then state or
// outcome, then detail, joined by slashes with the empty tail cut.
func events(t *testing.T, st *store.Store, host string) []string {
	t.Helper()
	var out []string
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
		return h.HostEvents(host, time.Time{}, func(e *store.HostEvent) (bool, error) {
			out = append(out, strings.TrimRight(e.Kind+"/"+e.State+e.Outcome+"/"+e.Detail, "/"))
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// hostEvents answers host's events, oldest first.
func hostEvents(t *testing.T, st *store.Store, host string) []store.HostEvent {
	t.Helper()
	var out []store.HostEvent
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
		return h.HostEvents(host, time.Time{}, func(e *store.HostEvent) (bool, error) {
			out = append(out, *e)
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// probes answers host's probes, newest first.
func probes(t *testing.T, st *store.Store, host string) []store.Probe {
	t.Helper()
	var out []store.Probe
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
		return h.Probes(host, func(p *store.Probe) (bool, error) {
			out = append(out, *p)
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// again is a new recorder over st, as a control-plane restart makes.
func again(st *store.Store, at *time.Time) *Recorder {
	rec := NewRecorder(st, slog.New(slog.DiscardHandler))
	rec.now = func() time.Time { return *at }
	return rec
}

func equal(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

// A state is written at each change and never while it holds, across a
// control-plane restart too.
func TestAStateIsWrittenAtEachChange(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	rec.writeState(t.Context(), "nas", at, "proving", "")
	rec.writeState(t.Context(), "nas", at.Add(time.Second), "ready", "")
	rec.writeState(t.Context(), "nas", at.Add(2*time.Second), "ready", "")
	rec.writeState(t.Context(), "nas", at.Add(3*time.Second), "unproved", "dns (job leg, 1.1.1.1:53): refused")
	rec.writeState(t.Context(), "nas", at.Add(4*time.Second), "unproved", "github (job leg, github.com:443): timeout")
	want := []string{"state/proving", "state/ready", "state/unproved/dns (job leg, 1.1.1.1:53): refused", "state/unproved/github (job leg, github.com:443): timeout"}
	if got := events(t, st, "nas"); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	t.Run("acrossARestart", func(t *testing.T) {
		rec := again(st, &at)
		rec.writeState(t.Context(), "nas", at.Add(time.Minute), "unproved", "github (job leg, github.com:443): timeout")
		rec.writeState(t.Context(), "nas", at.Add(2*time.Minute), "ready", "")
		if got := events(t, st, "nas"); !equal(got, append(want, "state/ready")) {
			t.Errorf("events = %v, want the unchanged state skipped and ready added", got)
		}
	})
}

// status is a host's status with instance inst, policy hash policy, the
// rules restored restored times, and each profile name's digest from
// digests, "" while it pulls.
func status(inst, policy string, restored uint64, digests ...string) *hostapi.Status {
	st := &hostapi.Status{InstanceID: inst, PolicyHash: policy, Rules: hostapi.RulesStatus{Restored: restored}}
	for i := 0; i < len(digests); i += 2 {
		st.Profiles = append(st.Profiles, hostapi.ProfileStatus{Name: digests[i], Image: "ghcr.io/cplieger/manade-runner:" + digests[i], Digest: digests[i+1]})
	}
	return st
}

// Each status is compared with the facts last recorded: the first records
// them with no event, and each later one writes what changed, never again
// after a control-plane restart.
func TestAStatusWritesWhatChanged(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	steps := []struct {
		st   *hostapi.Status
		name string
		want []string
	}{
		{name: "first", st: status("i1", "p1", 1, "small", "sha256:a")},
		{name: "same", st: status("i1", "p1", 1, "small", "sha256:a")},
		{name: "rulesRestored", st: status("i1", "p1", 4, "small", "sha256:a"), want: []string{"rules_restored"}},
		{name: "postureRebuilt", st: status("i1", "p2", 4, "small", "sha256:a"), want: []string{"posture_rebuilt"}},
		{name: "pulling", st: status("i1", "p2", 4, "small", "")},
		{name: "theSameImageBack", st: status("i1", "p2", 4, "small", "sha256:a")},
		{name: "imagePulled", st: status("i1", "p2", 4, "small", "sha256:b", "large", "sha256:c"), want: []string{"image_pulled", "image_pulled"}},
		{name: "restart", st: status("i2", "p2", 0, "small", "sha256:b", "large", "sha256:c"), want: []string{"restart"}},
		{name: "waitingForItsSection", st: status("i2", "", 0)},
		{name: "backOnAnotherPolicy", st: status("i2", "p3", 0, "small", "sha256:b"), want: []string{"posture_rebuilt", "image_pulled"}},
	}
	n := 0
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			at = at.Add(time.Second)
			rec.writeStatus(t.Context(), "nas", at, new(factsOf(s.st)))
			evs := hostEvents(t, st, "nas")
			var got []string
			for _, e := range evs[n:] {
				got = append(got, e.Kind)
				if !e.At.Equal(at) || e.Instance != s.st.InstanceID {
					t.Errorf("event %+v, want at %v on instance %s", e, at, s.st.InstanceID)
				}
			}
			n = len(evs)
			if !equal(got, s.want) {
				t.Errorf("events = %v, want %v", got, s.want)
			}
		})
	}
	evs := hostEvents(t, st, "nas")
	t.Run("rulesRestoredCountsTheRestores", func(t *testing.T) {
		if e := evs[0]; e.Count != 3 {
			t.Errorf("event = %+v, want the 3 restores since the last status", e)
		}
	})
	t.Run("imagePulledNamesItsImage", func(t *testing.T) {
		if e := evs[2]; e.Profile != "large" || e.Digest != "sha256:c" || e.Image != "ghcr.io/cplieger/manade-runner:large" {
			t.Errorf("event = %+v, want large's new image", e)
		}
		if e := evs[6]; e.Profile != "small" || e.Digest != "sha256:b" {
			t.Errorf("event = %+v, want small back in service", e)
		}
	})
	t.Run("nothingAgainAfterARestart", func(t *testing.T) {
		again(st, &at).writeStatus(t.Context(), "nas", at, new(factsOf(status("i2", "p3", 0, "small", "sha256:b"))))
		if got := len(hostEvents(t, st, "nas")); got != n {
			t.Errorf("%d events, want %d: the restart recorded the same facts again", got, n)
		}
	})
}

// Every completed proof is a probe row; a proof event marks each change of
// verdict and never a re-proof that keeps it.
func TestProofsWriteProbesAndVerdictChanges(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	pass := &hostapi.ProbeReport{InstanceID: "i1", Proven: true, Checks: []hostapi.ProbeCheck{{Name: "github", Leg: "job", Target: "github.com:443", Outcome: "connected", Duration: 1500 * time.Millisecond, Pass: true, Required: true}}}
	rec.writeProbe(t.Context(), new(probeOf("nas", at, pass, "")))
	rec.writeProbe(t.Context(), new(probeOf("nas", at.Add(time.Minute), pass, "")))
	rec.writeProbe(t.Context(), new(probeOf("nas", at.Add(2*time.Minute), &hostapi.ProbeReport{InstanceID: "i1"}, "dns: refused")))
	rec.writeProbe(t.Context(), new(probeOf("nas", at.Add(3*time.Minute), nil, "the probe request failed: closed")))
	rec.writeProbe(t.Context(), new(probeOf("nas", at.Add(4*time.Minute), nil, "the probe request failed: closed")))
	want := []string{"proof/passed", "proof/failed/dns: refused", "proof/failed/the probe request failed: closed"}
	if got := events(t, st, "nas"); !equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	ps := probes(t, st, "nas")
	if len(ps) != 5 {
		t.Fatalf("%d probes, want 5", len(ps))
	}
	t.Run("newestFirst", func(t *testing.T) {
		if !ps[0].At.Equal(at.Add(4*time.Minute)) || !ps[4].At.Equal(at) {
			t.Errorf("probes run from %v to %v, want newest first", ps[0].At, ps[4].At)
		}
	})
	t.Run("aPassKeepsItsChecks", func(t *testing.T) {
		p := ps[4]
		want := store.ProbeCheck{Name: "github", Leg: "job", Target: "github.com:443", Outcome: "connected", DurationMS: 1500, Pass: true, Required: true}
		if !p.Proven || p.Failure != "" || p.Instance != "i1" || len(p.Checks) != 1 || p.Checks[0] != want {
			t.Errorf("probe = %+v, want the passing report", p)
		}
	})
	t.Run("aFailedRequestHasNoChecks", func(t *testing.T) {
		if p := ps[0]; p.Proven || p.Checks != nil || p.Failure != "the probe request failed: closed" {
			t.Errorf("probe = %+v, want the failure alone", p)
		}
	})
}

// A host's text is kept on one line and at most 512 bytes, and a probe
// keeps at most 256 checks with the number dropped.
func TestHostTextIsBounded(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	long := "line one\nline two " + strings.Repeat("x", 1000)
	rep := &hostapi.ProbeReport{InstanceID: long}
	for range 300 {
		rep.Checks = append(rep.Checks, hostapi.ProbeCheck{Name: long, Reason: long})
	}
	rec.writeProbe(t.Context(), new(probeOf("nas", at, rep, long)))
	rec.State("nas", at, pool.HostState("unproved"), long)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec.Run(ctx)
	bounded := func(s string) bool { return len(s) <= 512 && !strings.ContainsAny(s, "\n\r") }
	p := probes(t, st, "nas")[0]
	if len(p.Checks) != 256 || p.Dropped != 44 {
		t.Errorf("%d checks, %d dropped; want 256 and 44", len(p.Checks), p.Dropped)
	}
	if !bounded(p.Instance) || !bounded(p.Failure) || !bounded(p.Checks[0].Name) || !bounded(p.Checks[255].Reason) {
		t.Error("a probe kept a host's text unbounded")
	}
	for _, e := range hostEvents(t, st, "nas") {
		if !bounded(e.Detail) || !bounded(e.Instance) {
			t.Errorf("event %s kept a host's text unbounded", e.Kind)
		}
	}
}

// A stopping recorder writes what is queued, then a gap for every host
// whose state it recorded and that is not in a gap already.
func TestAStopWritesAGapPerObservedHost(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	rec.State("nas", at, "ready", "")
	rec.State("box", at, "disconnected", "")
	rec.State("old", at, "ready", "")
	rec.State("old", at.Add(time.Second), "", pool.DetailHostRemoved)
	rec.Status("quiet", at, status("i1", "p1", 0))
	at = at.Add(time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec.Run(ctx)
	for host, want := range map[string][]string{
		"nas":   {"state/ready", "state//" + detailStopped},
		"box":   {"state/disconnected", "state//" + detailStopped},
		"old":   {"state/ready", "state//" + pool.DetailHostRemoved},
		"quiet": nil,
	} {
		t.Run(host, func(t *testing.T) {
			if got := events(t, st, host); !equal(got, want) {
				t.Errorf("events = %v, want %v", got, want)
			}
		})
	}
	t.Run("theGapIsAtTheStop", func(t *testing.T) {
		if evs := hostEvents(t, st, "nas"); !evs[1].At.Equal(at) {
			t.Errorf("gap at %v, want %v", evs[1].At, at)
		}
	})
}

// Host events and probes leave after 30 days, and what the history
// recorded of a host leaves with the host's settings.
func TestHostRetention(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rec, st := recorder(t, &now)
	for _, host := range []string{"nas", "gone"} {
		rec.writeState(t.Context(), host, now.Add(-31*day), "proving", "")
		rec.writeState(t.Context(), host, now.Add(-29*day), "ready", "")
		rec.writeProbe(t.Context(), new(probeOf(host, now.Add(-31*day), &hostapi.ProbeReport{Proven: true}, "")))
		rec.writeProbe(t.Context(), new(probeOf(host, now.Add(-29*day), &hostapi.ProbeReport{Proven: true}, "")))
	}
	cfg := settings(7*day, 2*day)
	cfg.Hosts = []config.Host{{Name: "nas"}}
	if err := housekeeper(st, &cfg).keep(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	t.Run("events", func(t *testing.T) {
		if got := events(t, st, "nas"); !equal(got, []string{"state/ready"}) {
			t.Errorf("events = %v, want the 29-day-old one alone", got)
		}
	})
	t.Run("probes", func(t *testing.T) {
		if ps := probes(t, st, "nas"); len(ps) != 1 || !ps[0].At.Equal(now.Add(-29*day)) {
			t.Errorf("probes = %+v, want the 29-day-old one alone", ps)
		}
	})
	t.Run("seen", func(t *testing.T) {
		if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
			for host, want := range map[string]bool{"nas": true, "gone": false} {
				if _, found, err := h.HostSeen(host); err != nil || found != want {
					t.Errorf("%s recorded = %v (%v), want %v", host, found, err, want)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
