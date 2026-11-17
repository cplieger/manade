package pool

import (
	"slices"
	"testing"

	"github.com/cplieger/manade/internal/github"
)

// Each job message reaches the run history with the host holding its
// runner, and none for a message no record's runner is named in.
func TestRunsTakeEachJobMessageWithItsRunnersHost(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.assign(b, 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1}, github.Event{Kind: github.Assigned, RequestID: 7, Name: "a", RunID: 1})
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, RequestID: 7, Runner: name, Name: "a", RunID: 1})
	r.poll(b, github.Stats{}, github.Event{Kind: github.Completed, RequestID: 8, Runner: "elsewhere", Name: "b", RunID: 2, Result: "failed"})
	got := r.runs.jobs
	if len(got) != 3 {
		t.Fatalf("jobs = %+v, want the 3 messages", got)
	}
	for i, want := range []string{"", "default", ""} {
		if got[i].host != want || got[i].ss != b.ss {
			t.Errorf("message %d went with host %q of %v, want %q of %v", i, got[i].host, got[i].ss, want, b.ss)
		}
	}
	if got[1].e.RequestID != 7 || got[1].e.Runner != name {
		t.Errorf("started = %+v, want request 7 on %s", got[1].e, name)
	}
}

// An unclean exit hands its run the log tail and the _diag tar its
// removal answered; a clean one hands neither, and an idle runner's
// retirement hands nothing, since it held no job.
func TestARunnersExitReachesTheRunHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		reason   ExitReason
		log      string
		diag     string
		code     int
		evidence bool
	}{
		{name: "failed", code: 1, reason: ExitFailed, evidence: true},
		{name: "completed", code: 0, reason: ExitCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, oneHost())
			b := r.small()
			r.assign(b, 1, 0)
			name := r.hosts[0].running()[0]
			r.pass()
			r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, RequestID: 7, Runner: name, Name: "a", RunID: 1})
			r.hosts[0].exit(name, tc.code)
			r.pass()
			got := r.runs.exitOf(name)
			if len(got) != 1 {
				t.Fatalf("exits of %s = %+v, want one", name, got)
			}
			x := got[0]
			if x.Reason != tc.reason || x.Host != "default" || x.Code == nil || *x.Code != tc.code || !x.At.Equal(r.now) {
				t.Errorf("exit = %+v, want %s with code %d on default now", x, tc.reason, tc.code)
			}
			wantLog, wantDiag := "", ""
			if tc.evidence {
				wantLog, wantDiag = "the last 200 lines of "+name, "diag of "+name
			}
			if string(x.Log) != wantLog || string(x.Diag) != wantDiag {
				t.Errorf("evidence = %q and %q, want %q and %q", x.Log, x.Diag, wantLog, wantDiag)
			}
		})
	}
	t.Run("idle", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.assign(r.small(), 1, 0)
		r.pass()
		old := r.hosts[0].running()[0]
		cfg := cloneConfig(r.cfg)
		cfg.Profiles[0].MilliCPUs = 1000
		r.apply(cfg)
		r.pass()
		if slices.Contains(r.hosts[0].running(), old) {
			t.Fatal("the idle runner was not retired")
		}
		if got := r.runs.exitOf(old); len(got) != 0 {
			t.Errorf("exits of the retired idle runner = %+v, want none", got)
		}
	})
}

// The run history gets each host's first runner list of a process once,
// and again after a restart, which is when it closes the runs the new
// process holds no record of.
func TestRunsTakeEachHostsFirstListOncePerProcess(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.pass()
	r.restart()
	r.pass()
	r.pass()
	got := r.runs.listed["default"]
	if len(got) != 2 || len(got[0]) != 0 || !slices.Equal(got[1], []string{name}) {
		t.Errorf("lists = %q, want the boot's empty one and the restart's naming %s", got, name)
	}
}

// A runner that exited while no process held its record hands its end to
// the run history when the next one finds it, with its evidence.
func TestAnExitNoRecordHeldReachesTheRunHistory(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.assign(b, 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, RequestID: 7, Runner: name, Name: "a", RunID: 1})
	r.hosts[0].exit(name, 137)
	r.restart()
	r.pass()
	got := r.runs.exitOf(name)
	if len(got) != 1 || got[0].Reason != ExitFailed || got[0].Code == nil || *got[0].Code != 137 || string(got[0].Diag) != "diag of "+name {
		t.Errorf("exits of %s = %+v, want one failed with code 137 and its _diag tar", name, got)
	}
}
