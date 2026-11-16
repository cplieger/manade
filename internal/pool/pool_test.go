package pool

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

const app = "acme/app"

// oneHost is a host default of 8 CPUs serving profile small of 2.
func oneHost() *config.Config {
	return poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}},
		[]profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app)
}

func (r *rig) small() *book { return r.book(app, "small") }

// assign has GitHub report n jobs assigned to b, running of them running.
func (r *rig) assign(b *book, n, running int) {
	r.reg.Opened(b.ss, github.Stats{Assigned: n, Running: running})
	r.starts()
}

// record answers b's record of runner name under the pool's lock.
func (r *rig) record(b *book, name string) (runner, bool) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	rr, ok := b.runners[name]
	if !ok {
		return runner{}, false
	}
	return *rr, true
}

func (r *rig) runners(b *book) int {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return len(b.runners)
}

func TestAStartRunsOnTheHostItsSlotWasReservedOn(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "nas", cpus: 2, mem: 4}, {name: "default", cpus: 8, mem: 32}},
		[]profileSpec{{name: "small", hosts: []string{"nas", "default"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	r.assign(r.small(), 2, 0)
	if got := len(r.hosts[0].running()); got != 1 {
		t.Errorf("nas runs %d runners, want the 1 its budget holds", got)
	}
	if got := len(r.hosts[1].running()); got != 1 {
		t.Errorf("default runs %d runners, want the 1 nas had no room for", got)
	}
	r.pass()
	if r.m.starts["true"] != 2 {
		t.Errorf("starts counted ok = %d, want 2", r.m.starts["true"])
	}
}

func TestAListBegunBeforeThePOSTWasAnsweredNeverJudgesTheRunner(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	h := r.p.hosts[0]
	r.p.mu.Lock()
	h.seq++
	begin := h.seq
	r.p.mu.Unlock()
	r.assign(r.small(), 1, 0)
	name := r.hosts[0].running()[0]
	r.p.mu.Lock()
	acts := r.p.plan(0, begin, &hostapi.RunnerList{InstanceID: "another"})
	r.p.mu.Unlock()
	if len(acts) != 0 {
		t.Fatalf("a list begun before the POST's answer acts on the runner: %+v", acts)
	}
	if _, ok := r.record(r.small(), name); !ok {
		t.Fatal("the record is gone after a list begun before the POST's answer")
	}
	r.p.mu.Lock()
	h.seq++
	acts = r.p.plan(0, h.seq, &hostapi.RunnerList{InstanceID: "another"})
	r.p.mu.Unlock()
	if len(acts) != 1 || acts[0].kind != actGone {
		t.Errorf("a later list from another supervisor without the runner gives %+v, want it gone", acts)
	}
}

func TestARunnerAbsentUnderAnotherInstanceIsLost(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	r.pass()
	name := r.hosts[0].running()[0]
	rec, _ := r.record(r.small(), name)
	r.hosts[0].set(func(f *fakeHost) { delete(f.runs, name) })
	r.hosts[0].restart(255)
	r.pass()
	if _, ok := r.record(r.small(), name); ok {
		t.Error("a runner absent from a new supervisor's list keeps its record")
	}
	if !r.gh.wasRemoved(rec.id) {
		t.Error("the registration of a runner a new supervisor does not hold stays")
	}
	if r.m.exits[ExitLost] != 1 {
		t.Errorf("lost exits = %d, want 1", r.m.exits[ExitLost])
	}
}

func TestAHostClockSkewChangesNoVerdict(t *testing.T) {
	t.Parallel()
	t.Run("adopted", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.assign(r.small(), 1, 0)
		name := r.hosts[0].running()[0]
		r.gh.set(func(g *fakeGitHub) { g.busy[1] = true })
		r.hosts[0].set(func(f *fakeHost) { f.runs[name].StartedAt = epoch.Add(-365 * 24 * time.Hour) })
		r.restart()
		r.pass()
		if got := r.hosts[0].running(); len(got) != 1 {
			t.Errorf("a busy runner whose host's clock is a year behind passed its lifetime at adoption: %v", got)
		}
	})
	t.Run("idle", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.assign(r.small(), 1, 0)
		name := r.hosts[0].running()[0]
		r.hosts[0].set(func(f *fakeHost) { f.runs[name].StartedAt = epoch.Add(365 * 24 * time.Hour) })
		r.now = r.now.Add(idleFor + time.Second)
		r.pass()
		if got := r.hosts[0].running(); slices.Contains(got, name) {
			t.Errorf("an idle runner past idle_timeout on this clock stays, its host's clock a year ahead: %v", got)
		}
	})
}

func TestAnAdoptedRunnerStillStartingIsNotRemoved(t *testing.T) {
	t.Parallel()
	cfg := oneHost()
	r := newRig(t, cfg)
	id := int64(77)
	r.gh.set(func(g *fakeGitHub) { g.regs[id] = true })
	r.hosts[0].set(func(f *fakeHost) {
		f.runs["manade-small-1"] = &hostapi.Runner{
			Name: "manade-small-1", Scope: app, Profile: "small", RunnerID: id, State: hostapi.StateCreating,
			Limits: hostapi.Limits{MilliCPUs: 2000, Memory: 4 * gib},
		}
	})
	r.restart()
	r.pass()
	rec, ok := r.record(r.small(), "manade-small-1")
	if !ok || rec.state != starting {
		t.Fatalf("a runner the host lists creating is adopted as %+v, %v; want starting", rec, ok)
	}
	if r.gh.wasRemoved(id) || len(r.hosts[0].deleted) != 0 {
		t.Error("an adopted runner still being started was removed")
	}
}

func TestAPOSTRefusedNotReadyFreesItsSlotAtOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) { f.refuse = true })
	r.assign(r.small(), 1, 0)
	if n := r.runners(r.small()); n != 0 {
		t.Errorf("records after a 409 = %d, want 0", n)
	}
	if !r.gh.wasRemoved(1) {
		t.Error("the registration of a start the host refused stays")
	}
	if r.m.starts["false"] != 1 {
		t.Errorf("failed starts = %d, want 1", r.m.starts["false"])
	}
	if posts := r.hosts[0].posts; posts != 1 {
		t.Errorf("POSTs = %d, want 1: the profile is not ready until the host's next status", posts)
	}
}

func TestAnUnansweredPOSTIsSentAgainByName(t *testing.T) {
	t.Parallel()
	t.Run("recorded", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.hosts[0].set(func(f *fakeHost) { f.timeouts = 1 })
		r.assign(r.small(), 1, 0)
		r.pass()
		if posts := r.hosts[0].posts; posts != 1 {
			t.Errorf("POSTs = %d, want 1: the host lists the runner it recorded", posts)
		}
		if got := r.hosts[0].running(); len(got) != 1 || r.gh.wasRemoved(1) {
			t.Errorf("running %v, registration removed %v; want one runner kept", got, r.gh.wasRemoved(1))
		}
	})
	t.Run("repeated", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.reg.Opened(r.small().ss, github.Stats{Assigned: 1})
		r.hosts[0].set(func(f *fakeHost) { f.down = true })
		r.starts()
		r.hosts[0].set(func(f *fakeHost) { f.down = false })
		r.pass()
		if posts := r.hosts[0].posts; posts != 2 {
			t.Errorf("POSTs = %d, want the first and its repeat", posts)
		}
		if got := r.hosts[0].running(); len(got) != 1 {
			t.Errorf("running %v, want one runner", got)
		}
		if r.gh.wasRemoved(1) {
			t.Error("an unanswered POST removed its registration")
		}
	})
}

func TestASlotFreesAtItsExitReportNotItsJobCompleted(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 2, mem: 4}},
		[]profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	b := r.small()
	r.assign(b, 1, 0)
	first := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: first, Name: "a", RunID: 1})
	r.poll(b, github.Stats{Assigned: 1, Running: 0},
		github.Event{Kind: github.Completed, Runner: first, Name: "a", RunID: 1, Result: "succeeded"},
		github.Event{Kind: github.Assigned, Name: "b", RunID: 2})
	r.starts()
	if posts := r.hosts[0].posts; posts != 1 {
		t.Fatalf("POSTs = %d before the first runner's exit, want 1", posts)
	}
	r.hosts[0].exit(first, 0)
	r.pass()
	if posts := r.hosts[0].posts; posts != 2 {
		t.Errorf("POSTs = %d after the exit report, want 2", posts)
	}
	if r.m.exits[ExitCompleted] != 1 {
		t.Errorf("completed exits = %d, want 1", r.m.exits[ExitCompleted])
	}
	if r.gh.wasRemoved(1) {
		t.Error("a runner a JobCompleted named had its registration removed")
	}
}

func TestALostRunnerHoldsItsJobsSlotAndItsRemovalIsRetried(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.assign(b, 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "a", RunID: 1})
	r.gh.set(func(g *fakeGitHub) { g.busy[1] = true })
	r.hosts[0].restart(255)
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1})
	r.pass()
	if r.m.exits[ExitLost] != 1 {
		t.Errorf("lost exits = %d, want 1", r.m.exits[ExitLost])
	}
	if posts := r.hosts[0].posts; posts != 1 {
		t.Errorf("POSTs = %d, want 1: the dead job is no demand", posts)
	}
	if figure := r.reg.Capacity(b.ss, r.now); figure < 1 {
		t.Errorf("capacity = %d, want the lost runner's slot", figure)
	}
	if r.gh.wasRemoved(1) {
		t.Fatal("a registration GitHub refused as running a job counts as removed")
	}
	r.gh.set(func(g *fakeGitHub) { g.busy[1] = false })
	r.pass()
	if !r.gh.wasRemoved(1) {
		t.Error("the lost runner's removal is not retried once GitHub takes it")
	}
}

func TestAPoolRemovalClosesWithItsReason(t *testing.T) {
	t.Parallel()
	t.Run("idle", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.assign(r.small(), 1, 0)
		r.pass()
		r.now = r.now.Add(idleFor + time.Second)
		r.reg.Opened(r.small().ss, github.Stats{})
		r.pass()
		if r.m.exits[ExitIdle] != 1 || r.m.exits[ExitFailed] != 0 {
			t.Errorf("exits = %v, want one idle and no failure", r.m.exits)
		}
		if len(r.hosts[0].running()) != 0 {
			t.Error("the idle runner is still running")
		}
	})
	t.Run("lifetime", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		b := r.small()
		r.assign(b, 1, 0)
		name := r.hosts[0].running()[0]
		r.pass()
		r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "a", RunID: 1})
		r.now = r.now.Add(3 * time.Hour)
		r.pass()
		if r.m.exits[ExitLifetime] != 1 {
			t.Errorf("exits = %v, want one lifetime", r.m.exits)
		}
	})
}

func TestAFailedStartRemovesItsRegistrationAndEntry(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) { f.failStart = true })
	r.assign(r.small(), 1, 0)
	r.hosts[0].set(func(f *fakeHost) { f.failStart = false })
	r.pass()
	if !r.gh.wasRemoved(1) {
		t.Error("a failed start's registration stays")
	}
	if len(r.hosts[0].deleted) != 1 {
		t.Errorf("DELETEs = %v, want the failed entry's", r.hosts[0].deleted)
	}
	if r.m.starts["false"] != 1 {
		t.Errorf("failed starts = %d, want 1", r.m.starts["false"])
	}
	if posts := r.hosts[0].posts; posts != 2 {
		t.Errorf("POSTs = %d, want a second start once the slot freed", posts)
	}
}

func TestAFailedJITConfigFreesTheSlotAndWaits(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.gh.set(func(g *fakeGitHub) { g.jitFail = true })
	r.assign(r.small(), 1, 0)
	r.gh.set(func(g *fakeGitHub) { g.jitFail = false })
	if n := r.runners(r.small()); n != 0 {
		t.Fatalf("records after a failed JIT config = %d, want 0", n)
	}
	r.starts()
	if r.hosts[0].posts != 0 {
		t.Fatal("a start ran inside the retry wait")
	}
	r.now = r.now.Add(startRetry)
	r.starts()
	if r.hosts[0].posts != 1 {
		t.Errorf("POSTs = %d after the retry wait, want 1", r.hosts[0].posts)
	}
}

func TestAnUnreachableHostKeepsItsReservations(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	name := r.hosts[0].running()[0]
	r.hosts[0].set(func(f *fakeHost) { f.down = true })
	r.pass()
	if _, ok := r.record(r.small(), name); !ok {
		t.Fatal("an unreachable host's runner lost its record")
	}
	if r.m.serving["default"] {
		t.Error("an unreachable host serves")
	}
	r.hosts[0].set(func(f *fakeHost) { f.down = false })
	r.pass()
	if !r.m.serving["default"] {
		t.Error("the host does not serve once it answers and proves again")
	}
}

func TestANotReadyProfileIsNotFunded(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) { f.notReady["small"] = true })
	r.pass()
	r.assign(r.small(), 1, 0)
	if r.hosts[0].posts != 0 {
		t.Fatal("a profile its host reports not ready was funded")
	}
	r.hosts[0].set(func(f *fakeHost) { f.notReady["small"] = false })
	r.pass()
	if r.hosts[0].posts != 1 {
		t.Errorf("POSTs = %d once the profile is ready, want 1", r.hosts[0].posts)
	}
}

func TestBootStartsTheAssignmentsLessTheAdoptedBusyRunners(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	old := r.hosts[0].running()[0]
	r.gh.set(func(g *fakeGitHub) { g.busy[1] = true })
	r.restart()
	r.assign(r.small(), 2, 1)
	if rec, ok := r.record(r.small(), old); !ok || rec.state != busy {
		t.Fatalf("the busy runner is adopted as %+v, %v; want busy", rec, ok)
	}
	if got := len(r.hosts[0].running()); got != 2 {
		t.Errorf("running %d, want the adopted runner and one start", got)
	}
}

func TestBootRemovesAnIdleRunnerOfAPreviousProcess(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	r.restart()
	if got := r.hosts[0].running(); len(got) != 0 {
		t.Errorf("running %v after boot, want the idle runner removed", got)
	}
	if !r.gh.wasRemoved(1) {
		t.Error("the idle runner's registration stays")
	}
}

func TestBootCountsAnExitedRunnerNoRecordHoldsByItsExitCode(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	crash, done := 255, 0
	r.gh.set(func(g *fakeGitHub) { g.regs[77], g.regs[78] = true, true })
	r.hosts[0].set(func(f *fakeHost) {
		for name, rn := range map[string]*hostapi.Runner{
			"manade-small-1": {RunnerID: 77, ExitCode: &crash},
			"manade-small-2": {RunnerID: 78, ExitCode: &done},
		} {
			rn.Name, rn.Scope, rn.Profile, rn.State = name, app, "small", hostapi.StateExited
			rn.Limits = hostapi.Limits{MilliCPUs: 2000, Memory: 4 * gib}
			f.runs[name] = rn
		}
	})
	r.restart()
	r.m.mu.Lock()
	exits := maps.Clone(r.m.exits)
	r.m.mu.Unlock()
	if len(exits) != 2 || exits[ExitLost] != 1 || exits[ExitCompleted] != 1 {
		t.Errorf("exits = %v, want one lost for the crash and one completed", exits)
	}
	if !r.gh.wasRemoved(77) {
		t.Error("the crashed runner's registration stays")
	}
	var left int
	r.hosts[0].set(func(f *fakeHost) { left = len(f.runs) })
	if left != 0 {
		t.Errorf("%d exited runners stay on the host, want both removed", left)
	}
}

func TestAStrayHoldsItsReservationUntilItEnds(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 2, mem: 4}},
		[]profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	r.hosts[0].set(func(f *fakeHost) {
		f.runs["manade-gone-1"] = &hostapi.Runner{
			Name: "manade-gone-1", Scope: "acme/old", Profile: "gone", RunnerID: 9, State: hostapi.StateRunning,
			Limits: hostapi.Limits{MilliCPUs: 2000, Memory: 4 * gib},
		}
	})
	r.restart()
	r.assign(r.small(), 1, 0)
	if r.hosts[0].posts != 0 {
		t.Fatal("a start took the budget a stray holds")
	}
	r.hosts[0].exit("manade-gone-1", 0)
	r.pass()
	if r.hosts[0].posts != 1 {
		t.Errorf("POSTs = %d once the stray ended, want 1", r.hosts[0].posts)
	}
}

func TestAStrayPastTheDefaultLifetimeIsRemoved(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) {
		f.runs["manade-gone-1"] = &hostapi.Runner{Name: "manade-gone-1", Scope: "acme/old", Profile: "gone", RunnerID: 9, State: hostapi.StateRunning}
	})
	r.restart()
	r.now = r.now.Add(config.DefaultMaxLifetime)
	r.pass()
	if len(r.hosts[0].deleted) != 0 {
		t.Fatal("a stray was removed at its lifetime, not past it")
	}
	r.now = r.now.Add(time.Second)
	r.pass()
	if !slices.Contains(r.hosts[0].deleted, "manade-gone-1") {
		t.Error("a stray past the default max_lifetime stays")
	}
}

func TestShutdownLeavesARunnerStillStarting(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) { f.timeouts = 1 })
	r.assign(r.small(), 1, 0)
	name := r.hosts[0].running()[0]
	r.hosts[0].set(func(f *fakeHost) { f.runs[name].State = hostapi.StateStarting })
	r.p.mu.Lock()
	r.p.stopping = true
	r.p.mu.Unlock()
	r.pass()
	if len(r.hosts[0].deleted) != 0 || r.gh.wasRemoved(1) {
		t.Errorf("shutdown removed a runner still starting: DELETEs %v", r.hosts[0].deleted)
	}
}

func TestTheProofGatesTheHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host *fakeHost
		name string
		want string
	}{
		{name: "section"},
		{name: "check", want: "job-gateway"},
		{name: "timeout", want: "unproven"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, oneHost())
			h := r.hosts[0]
			switch c.name {
			case "section":
				h.section = "another"
			case "check":
				h.probe = func(context.Context) (hostapi.ProbeReport, error) {
					return hostapi.ProbeReport{Checks: []hostapi.ProbeCheck{{Name: "job-gateway", Leg: "job", Target: "172.18.0.1:7464", Required: true, Reason: "connected"}}}, nil
				}
			default:
				h.probe = func(context.Context) (hostapi.ProbeReport, error) {
					return hostapi.ProbeReport{}, hostapi.ErrProbeTimeout
				}
			}
			r.restart()
			r.assign(r.small(), 1, 0)
			if h.posts != 0 {
				t.Error("a host whose proof failed, or that runs another section, got a start")
			}
			if c.want != "" && !strings.Contains(r.log.String(), c.want) {
				t.Errorf("the log does not name %q:\n%s", c.want, r.log.String())
			}
		})
	}
}

func TestANewInstanceIsProvedBeforeItServes(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].restart(143)
	r.p.pass(t.Context())
	r.assign(r.small(), 1, 0)
	if r.hosts[0].posts != 0 {
		t.Fatal("a new supervisor got a start before its proof")
	}
	r.prove()
	r.starts()
	if r.hosts[0].posts != 1 || r.hosts[0].probes != 2 {
		t.Errorf("POSTs %d, probes %d; want 1 and 2", r.hosts[0].posts, r.hosts[0].probes)
	}
}

func TestTriggersDuringAProofFoldIntoOneFollowUp(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	entered, release := make(chan struct{}), make(chan struct{})
	r.hosts[0].set(func(f *fakeHost) {
		f.probe = func(context.Context) (hostapi.ProbeReport, error) {
			entered <- struct{}{}
			<-release
			return hostapi.ProbeReport{Proven: true}, nil
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { r.p.runProver(ctx, 0) })
	wake(r.p.hosts[0].want)
	<-entered
	for range 3 {
		wake(r.p.hosts[0].want)
	}
	release <- struct{}{}
	<-entered
	release <- struct{}{}
	cancel()
	wg.Wait()
	if got := r.hosts[0].probes; got != 3 {
		t.Errorf("probes = %d, want boot's, the run and one follow-up", got)
	}
}

func TestAPublicScopeIsNotServedAtBootOrAtRunTime(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	r.pass()
	r.p.mu.Lock()
	r.p.verdict(r.p.scopes[app], false, nil, r.now)
	r.p.mu.Unlock()
	r.pass()
	if got := r.hosts[0].running(); len(got) != 0 {
		t.Errorf("running %v after the scope turned public, want its idle runner removed", got)
	}
	if figure := r.reg.Capacity(r.small().ss, r.now); figure != 0 {
		t.Errorf("capacity %d for a public scope, want 0", figure)
	}
	r.gh.set(func(g *fakeGitHub) { g.public[app] = true })
	r.restart()
	if figure := r.reg.Capacity(r.small().ss, r.now); figure != 0 {
		t.Errorf("capacity %d for a scope a boot read as public, want 0", figure)
	}
	if _, open := r.p.servedSets()[setKey{app, "small"}]; open {
		t.Error("a scope a boot read as public keeps a session to open")
	}
}

func TestHealthIgnoresWhetherAnyHostServes(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.reg.Opened(r.small().ss, github.Stats{})
	r.reg.Beat(r.small().ss)
	r.pass()
	if !r.reg.Health().Healthy() {
		t.Fatalf("health %+v after a pass, want healthy", r.reg.Health())
	}
	r.hosts[0].set(func(f *fakeHost) { f.down = true })
	r.pass()
	if !r.reg.Health().Healthy() {
		t.Errorf("health %+v after a pass with no host serving, want healthy", r.reg.Health())
	}
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.serving["default"] {
		t.Error("the unreachable host is recorded as serving")
	}
}

func TestABusyRunnerPastIdleTimeoutIsNotRetired(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.assign(b, 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "a", RunID: 1})
	r.now = r.now.Add(idleFor + time.Second)
	r.pass()
	if r.gh.wasRemoved(1) || len(r.hosts[0].running()) != 1 {
		t.Errorf("a runner on a job was retired as idle: removed %v, running %v", r.gh.wasRemoved(1), r.hosts[0].running())
	}
}

func TestAJobCompletedEndsAPendingRegistrationRemoval(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.assign(b, 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "a", RunID: 1})
	r.gh.set(func(g *fakeGitHub) { g.busy[1] = true })
	r.hosts[0].restart(255)
	r.pass()
	r.poll(b, github.Stats{}, github.Event{Kind: github.Completed, Runner: name, Name: "a", RunID: 1, Result: "failed"})
	r.gh.set(func(g *fakeGitHub) { g.busy[1] = false })
	r.pass()
	if r.gh.wasRemoved(1) {
		t.Error("a registration GitHub deleted with its JobCompleted is removed again")
	}
}

func TestAnAdoptedPinnedRunnerReservesItsCPUs(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 4, mem: 32}},
		[]profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	r.gh.set(func(g *fakeGitHub) { g.regs[50], g.busy[50] = true, true })
	r.hosts[0].set(func(f *fakeHost) {
		f.runs["manade-small-1"] = &hostapi.Runner{
			Name: "manade-small-1", Scope: app, Profile: "small", RunnerID: 50, State: hostapi.StateRunning,
			Limits: hostapi.Limits{CPUSet: "0-3", Memory: 4 * gib},
		}
	})
	r.restart()
	r.assign(r.small(), 2, 1)
	if r.hosts[0].posts != 0 {
		t.Errorf("POSTs = %d beside a runner pinned to all 4 CPUs, want 0", r.hosts[0].posts)
	}
}

func TestCPUsIn(t *testing.T) {
	t.Parallel()
	for set, want := range map[string]int{"0-1,3": 3, "5": 1, "": 0, "x": 0, "3-1": 0, "0-x": 0, "2,4-7": 5} {
		if got := cpusIn(set); got != want {
			t.Errorf("cpusIn(%q) = %d, want %d", set, got, want)
		}
	}
}

func TestAPartialReadFundsNothingNew(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.reg.Opened(b.ss, github.Stats{})
	jobs := []github.QueuedJob{job(1, "small", r.now), job(2, "small", r.now)}
	r.p.queued(r.now, []github.Queue{{Scope: app, Jobs: jobs, Complete: true}})
	if got := r.reg.Capacity(b.ss, r.now); got < 2 {
		t.Fatalf("capacity after a whole read of 2 jobs = %d, want them funded", got)
	}
	r.p.queued(r.now, []github.Queue{{Scope: app, Jobs: jobs[:1]}})
	if got := r.reg.Capacity(b.ss, r.now); got != 0 {
		t.Errorf("capacity after a partial read = %d, want 0", got)
	}
}

func TestShutdownEndsTheSessionsBeforeItsLastPass(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.assign(r.small(), 1, 0)
	r.pass()
	var idleAtEnd []string
	r.p.shutdown(func() { idleAtEnd = r.hosts[0].running() })
	if len(idleAtEnd) != 1 {
		t.Errorf("running %v when the sessions ended, want the idle runner still there", idleAtEnd)
	}
	if got := r.hosts[0].running(); len(got) != 0 {
		t.Errorf("running %v after the shutdown, want the idle runner removed", got)
	}
	if h := r.reg.Health(); !h.Stopping || h.Healthy() {
		t.Errorf("health %+v after the shutdown, want Stopping", h)
	}
	if figure := r.reg.Capacity(r.small().ss, r.now); figure != 0 {
		t.Errorf("capacity %d after the shutdown, want 0", figure)
	}
}
