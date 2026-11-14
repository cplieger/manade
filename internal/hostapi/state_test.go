package hostapi

import (
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/docker"
)

func TestState(t *testing.T) {
	type want struct {
		state  RunnerState
		reason string
		orphan bool
		exited bool
		listed bool
	}
	records := map[string]*Record{
		"creating": {State: RecordCreating},
		"starting": {State: RecordStarting},
		"started":  {State: RecordStarted},
		"failed":   {State: RecordFailed, Reason: "create refused"},
		// The host removed the container itself.
		"startedRemoved": {State: RecordStarted, RemovedBy: "pool: idle"},
		"failedRemoved":  {State: RecordFailed, Reason: "create refused", RemovedBy: "pool: idle"},
		"none":           nil,
	}
	failed := want{state: StateFailed, reason: "create refused", listed: true}
	orphaned := want{state: StateFailed, reason: "create refused", orphan: true, listed: true}
	running := want{state: StateRunning, listed: true}
	exited := want{state: StateExited, exited: true, listed: true}
	cases := map[string]map[string]want{
		"": {
			"creating":       {state: StateCreating, listed: true},
			"starting":       {state: StateStarting, listed: true},
			"started":        {state: StateFailed, reason: reasonRemovedOutside, listed: true},
			"failed":         failed,
			"startedRemoved": {state: StateExited, listed: true},
			"failedRemoved":  failed,
			"none":           {},
		},
		docker.StateCreated: {
			"creating": {state: StateCreating, listed: true},
			"starting": {state: StateStarting, listed: true},
			"started":  {state: StateFailed, reason: reasonResetOutside, listed: true},
			"failed":   orphaned,
			"none":     {state: StateFailed, reason: ReasonHostRestarted, listed: true},
		},
	}

	for _, s := range []string{docker.StateRunning, docker.StatePaused, docker.StateRestarting} {
		cases[s] = map[string]want{"creating": running, "starting": running, "started": running, "failed": orphaned, "none": running}
	}
	for _, s := range []string{docker.StateExited, docker.StateDead, docker.StateRemoving} {
		cases[s] = map[string]want{"creating": exited, "starting": exited, "started": exited, "failed": orphaned, "none": exited}
	}
	finished := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	for docker, row := range cases {
		for rec, w := range row {
			name := "absent"
			if docker != "" {
				name = docker
			}
			t.Run(name+"_"+rec, func(t *testing.T) {
				var c *Container
				if docker != "" {
					c = &Container{ID: "c1", Name: "r1", State: docker, ExitCode: 143, OOMKilled: true, FinishedAt: finished}
				}
				var r *Record
				if records[rec] != nil {
					cp := *records[rec]
					cp.Request = RunnerRequest{RunnerName: "r1"}
					r = &cp
				}
				got, listed := State(c, r)
				if listed != w.listed {
					t.Fatalf("listed = %v, want %v", listed, w.listed)
				}
				if !listed {
					return
				}
				if r != nil && got.RemovedBy != r.RemovedBy {
					t.Errorf("removed_by = %q, want %q", got.RemovedBy, r.RemovedBy)
				}
				if got.State != w.state || got.Reason != w.reason {
					t.Errorf("state = %s %q, want %s %q", got.State, got.Reason, w.state, w.reason)
				}
				if (len(got.Orphans) == 1 && got.Orphans[0] == "c1") != w.orphan || (!w.orphan && len(got.Orphans) > 0) {
					t.Errorf("orphans = %v, want orphan %v", got.Orphans, w.orphan)
				}
				hasExit := got.ExitCode != nil && *got.ExitCode == 143 && got.OOMKilled && got.ExitedAt.Equal(finished)
				if hasExit != w.exited || (!w.exited && (got.ExitCode != nil || got.OOMKilled)) {
					t.Errorf("exit = %v oom %v at %v, want exit shown %v", got.ExitCode, got.OOMKilled, got.ExitedAt, w.exited)
				}
			})
		}
	}
}

func TestStateUnknownDockerState(t *testing.T) {
	for name, word := range map[string]string{"empty": "", "unknown": "hibernating", "multiline": "odd\nword"} {
		t.Run(name, func(t *testing.T) {
			c := &Container{ID: "c1", Name: "r1", State: word, ExitCode: 0}
			got, listed := State(c, &Record{State: RecordStarted, Request: RunnerRequest{RunnerName: "r1"}})
			if !listed || got.State != StateFailed || got.ExitCode != nil || !strings.HasPrefix(got.Reason, reasonUnknownState+` "`) || strings.ContainsAny(got.Reason, "\n\r") {
				t.Errorf("runner = %+v, want failed naming the state word on one line, no exit code", got)
			}
			if name == "unknown" && got.Reason != reasonUnknownState+` "hibernating"` {
				t.Errorf("reason = %q", got.Reason)
			}
		})
	}
}

func TestStateLimitsAndLabels(t *testing.T) {
	recorded := Limits{MilliCPUs: 2000, Memory: 4 << 30}
	built := Limits{CPUSet: "0,1", Memory: 4 << 30}
	req := RunnerRequest{RunnerName: "r1", Profile: "small", Scope: "acme/app", RunnerID: 7}
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	t.Run("creating shows the recorded limits", func(t *testing.T) {
		got, _ := State(&Container{Name: "r1", State: docker.StateCreated, Limits: built}, &Record{Request: req, Limits: recorded, State: RecordCreating})
		if got.Limits != recorded {
			t.Errorf("limits = %+v, want %+v", got.Limits, recorded)
		}
	})
	t.Run("a container's built limits win once created", func(t *testing.T) {
		got, _ := State(&Container{Name: "r1", State: docker.StateRunning, Limits: built, StartedAt: started}, &Record{Request: req, Limits: recorded, State: RecordStarted})
		if got.Limits != built || !got.StartedAt.Equal(started) {
			t.Errorf("limits = %+v started %v, want %+v at %v", got.Limits, got.StartedAt, built, started)
		}
	})
	t.Run("no record reads the container's labels", func(t *testing.T) {
		got, _ := State(&Container{Name: "r2", Profile: "big", Scope: "acme/web", RunnerID: 9, State: docker.StateRunning, Limits: built}, nil)
		if got.Name != "r2" || got.Profile != "big" || got.Scope != "acme/web" || got.RunnerID != 9 || got.Limits != built {
			t.Errorf("runner = %+v", got)
		}
	})
	t.Run("a record without a container keeps its request and removal", func(t *testing.T) {
		got, _ := State(nil, &Record{Request: req, Limits: recorded, State: RecordStarting, RemovedBy: "pool: idle"})
		if got.Name != "r1" || got.Profile != "small" || got.Scope != "acme/app" || got.RunnerID != 7 || got.Limits != recorded || got.RemovedBy != "pool: idle" {
			t.Errorf("runner = %+v", got)
		}
	})
	t.Run("a created container has no start time", func(t *testing.T) {
		got, _ := State(&Container{Name: "r1", State: docker.StateCreated, StartedAt: started}, &Record{Request: req, State: RecordStarting})
		if !got.StartedAt.IsZero() {
			t.Errorf("started = %v, want zero", got.StartedAt)
		}
	})
}
