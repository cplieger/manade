package pool

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

func TestALostDeleteAnswerClosesTheRecordWithinOnePass(t *testing.T) {
	t.Parallel()
	exited := func(r *rig, name string) { r.hosts[0].exit(name, 0) }
	idle := func(r *rig, _ string) {
		r.now = r.now.Add(idleFor + time.Second)
		r.reg.Opened(r.small().ss, github.Stats{})
	}
	cases := []struct {
		end                                func(r *rig, name string)
		name                               string
		want                               ExitReason
		start, newSupervisor, restartAfter bool
	}{
		{name: "exited", want: ExitCompleted, end: exited},
		{name: "idle", want: exitIdle, end: idle},
		{name: "failedStart", start: true},
		{name: "exitedAtARestart", want: ExitCompleted, end: func(r *rig, _ string) { r.hosts[0].restart(0) }},
		{name: "failedStartListedByANewSupervisor", start: true, newSupervisor: true},
		{name: "exitedThenARestart", want: ExitCompleted, end: exited, restartAfter: true},
		{name: "idleThenARestart", want: exitIdle, end: idle, restartAfter: true},
		{name: "failedStartThenARestart", start: true, restartAfter: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := poolConfig([]hostSpec{{name: "default", cpus: 2, mem: 4}},
				[]profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app)
			r := newRig(t, cfg)
			b := r.small()
			r.hosts[0].set(func(f *fakeHost) { f.failStart = tc.start })
			r.assign(b, 1, 0)
			r.hosts[0].set(func(f *fakeHost) { f.failStart = false })
			var name string
			r.p.mu.Lock()
			for n := range b.runners {
				name = n
			}
			r.p.mu.Unlock()
			if !tc.start {
				r.pass()
				tc.end(r, name)
			}
			r.hosts[0].set(func(f *fakeHost) {
				f.lost = 1
				if tc.newSupervisor {
					f.inst++
				}
			})
			r.pass()
			if _, ok := r.record(b, name); !ok {
				t.Fatal("a DELETE whose answer was lost closed the record at once")
			}
			if tc.restartAfter {
				r.hosts[0].restart(0)
			}
			r.pass()
			if _, ok := r.record(b, name); ok {
				t.Fatal("the record of a runner its host removed stays after a list without it")
			}
			if n := r.gh.removals(1); n != 1 {
				t.Errorf("registration 1 removed %d times, want once", n)
			}
			want := map[ExitReason]int{}
			if tc.want != "" {
				want[tc.want] = 1
			}
			r.m.mu.Lock()
			exits := r.m.exits
			r.m.mu.Unlock()
			if len(exits) != len(want) || exits[tc.want] != want[tc.want] {
				t.Errorf("exits = %v, want %v", exits, want)
			}
			if posts := r.hosts[0].posts; posts != 2 && tc.start {
				t.Errorf("POSTs = %d, want a second start in the freed slot", posts)
			}
		})
	}
}

func TestAnExitTheHostRemovedForADeleteClosesAsRemoved(t *testing.T) {
	t.Parallel()
	code := 137
	removed := func(name string) *hostapi.Runner {
		return &hostapi.Runner{
			Name: name, Scope: app, Profile: "small", RunnerID: 1, State: hostapi.StateExited,
			ExitCode: &code, RemovedBy: "host: the orchestrator's DELETE",
		}
	}
	check := func(t *testing.T, r *rig) {
		t.Helper()
		r.m.mu.Lock()
		defer r.m.mu.Unlock()
		if r.m.exits[exitRemoved] != 1 || r.m.exits[ExitFailed] != 0 {
			t.Errorf("exits = %v, want one removed and no failure", r.m.exits)
		}
	}
	t.Run("held", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.assign(r.small(), 1, 0)
		r.pass()
		name := r.hosts[0].running()[0]
		r.hosts[0].set(func(f *fakeHost) { f.runs[name] = removed(name) })
		r.pass()
		check(t, r)
	})
	t.Run("adopted", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, oneHost())
		r.hosts[0].set(func(f *fakeHost) { f.runs["manade-small-1"] = removed("manade-small-1") })
		r.restart()
		check(t, r)
	})
}

func TestAnAdoptedStartingRunnerOnAHeldHostIsNotDeleted(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 4, mem: 16}},
		[]profileSpec{{name: "big", hosts: []string{"default"}, cpus: 4, mem: 8}, {name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	r.gh.set(func(g *fakeGitHub) { g.regs[77] = true })
	r.hosts[0].set(func(f *fakeHost) {
		f.runs["manade-small-1"] = &hostapi.Runner{
			Name: "manade-small-1", Scope: app, Profile: "small", RunnerID: 77, State: hostapi.StateStarting,
			Limits: hostapi.Limits{MilliCPUs: 2000, Memory: 4 * gib},
		}
	})
	r.restart()
	big := r.book(app, "big")
	r.reg.Opened(big.ss, github.Stats{})
	r.reg.Opened(r.small().ss, github.Stats{})
	r.p.queued(r.now, []github.Queue{{Scope: app, Jobs: []github.QueuedJob{job(1, "big", r.now)}, Complete: true}})
	r.now = r.now.Add(5 * time.Minute)
	r.reg.Capacity(big.ss, r.now)
	r.p.mu.Lock()
	held := r.p.l.held[0]
	r.p.mu.Unlock()
	if !held {
		t.Fatal("the head reserve does not hold default for big's job")
	}
	r.pass()
	if len(r.hosts[0].deleted) != 0 || r.gh.wasRemoved(77) {
		t.Errorf("a runner still starting on a held host was removed: DELETEs %v", r.hosts[0].deleted)
	}
}

func TestASlowHostHoldsBackNoOtherHostsExits(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "slow", cpus: 2, mem: 4}, {name: "fast", cpus: 2, mem: 4}},
		[]profileSpec{{name: "small", hosts: []string{"slow", "fast"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	b := r.small()
	r.assign(b, 2, 0)
	r.pass()
	slow, fast := r.hosts[0].running(), r.hosts[1].running()
	if len(slow) != 1 || len(fast) != 1 {
		t.Fatalf("running %v and %v, want one runner on each host", slow, fast)
	}
	hold, held := make(chan struct{}), make(chan struct{}, 1)
	r.hosts[0].set(func(f *fakeHost) { f.hold, f.held = hold, held })
	r.hosts[0].exit(slow[0], 0)
	r.hosts[1].exit(fast[0], 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.p.passHost(context.Background(), 0)
	}()
	<-held
	r.now = r.now.Add(2 * reconcileLease)
	r.p.passHost(context.Background(), 1)
	if _, ok := r.record(b, fast[0]); ok {
		t.Error("fast's exit kept its slot while slow's DELETE was in flight")
	}
	r.p.step(context.Background())
	if h := r.reg.Health(); !h.Reconciling {
		t.Errorf("health %+v while slow's loop waits on its DELETE, want Reconciling", h)
	}
	close(hold)
	<-done
	if _, ok := r.record(b, slow[0]); ok {
		t.Error("slow's exit kept its slot once its DELETE answered")
	}
}

func TestAHostLoopPastItsLeaseIsNotReconciling(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.reg.Opened(r.small().ss, github.Stats{})
	r.pass()
	if !r.reg.Health().Reconciling {
		t.Fatalf("health %+v right after a pass, want Reconciling", r.reg.Health())
	}
	r.now = r.now.Add(reconcileLease + time.Second)
	if h := r.reg.Health(); h.Reconciling {
		t.Errorf("health %+v with no beat for longer than the lease, want not Reconciling", h)
	}
}

func TestAPassOfEveryHostRenewsEachHostsLease(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "slow", cpus: 2, mem: 4}, {name: "fast", cpus: 2, mem: 4}},
		[]profileSpec{{name: "small", hosts: []string{"slow", "fast"}, cpus: 2, mem: 4}}, app)
	r := newRig(t, cfg)
	r.assign(r.small(), 1, 0)
	r.pass()
	slow := r.hosts[0].running()
	if len(slow) != 1 {
		t.Fatalf("running %v on slow, want one runner", slow)
	}
	hold, held := make(chan struct{}), make(chan struct{}, 1)
	r.hosts[0].set(func(f *fakeHost) { f.hold, f.held = hold, held })
	r.hosts[0].exit(slow[0], 0)
	select {
	case <-r.p.kick:
	default:
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.p.pass(context.Background())
	}()
	// slow waits on its DELETE and fast's pass has ended: its poke is the
	// pass's only one.
	<-held
	<-r.p.kick
	r.now = r.now.Add(2 * reconcileLease)
	close(hold)
	<-done
	if h := r.reg.Health(); !h.Reconciling {
		t.Errorf("health %+v right after a pass of every host, want Reconciling", h)
	}
	for _, h := range r.reg.Snapshot().Hosts {
		if !h.Reconciled.Equal(r.now) || !h.ReconcileDue.Equal(r.now.Add(reconcileLease)) {
			t.Errorf("host %s reconciled %v due %v, want the pass's end and its lease after it", h.Host, h.Reconciled, h.ReconcileDue)
		}
	}
}

func TestAnExitJudgedAcrossARestartKeepsItsReasonUntilItCloses(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	b := r.small()
	r.assign(b, 1, 0)
	name := r.hosts[0].running()[0]
	r.pass()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "a", RunID: 1})
	r.hosts[0].restart(255)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.p.passHost(ctx, 0)
	if _, ok := r.record(b, name); !ok {
		t.Fatal("a pass whose context ended closed a record")
	}
	r.pass()
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.exits[ExitLost] != 1 || len(r.m.exits) != 1 {
		t.Errorf("exits = %v, want the busy runner the restart ended lost", r.m.exits)
	}
}

func TestABusyRunnerAHostStopEndsClosesAsFailedAndACrashAsLost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		want     ExitReason
		code     int
		removals int
		// lostDelete loses the first DELETE's answer, and the runner's
		// JobCompleted arrives before the next pass.
		lostDelete bool
	}{
		{name: "stop", code: 143, want: ExitFailed, removals: 1},
		{name: "stopThenJobCompletedAfterALostDelete", code: 143, want: ExitFailed, lostDelete: true},
		{name: "crash", code: 255, want: ExitLost, removals: 1},
		{name: "crashThenJobCompletedAfterALostDelete", code: 255, want: ExitLost, lostDelete: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, oneHost())
			b := r.small()
			r.assign(b, 1, 0)
			name := r.hosts[0].running()[0]
			r.pass()
			r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "a", RunID: 1})
			r.hosts[0].restart(tc.code)
			if tc.lostDelete {
				r.hosts[0].set(func(f *fakeHost) { f.lost = 1 })
				r.pass()
				r.poll(b, github.Stats{}, github.Event{Kind: github.Completed, Runner: name, Name: "a", RunID: 1, Result: "failed"})
			}
			r.pass()
			if _, ok := r.record(b, name); ok {
				t.Fatal("the record of a runner its host's restart ended stays open")
			}
			r.m.mu.Lock()
			exits := maps.Clone(r.m.exits)
			r.m.mu.Unlock()
			if len(exits) != 1 || exits[tc.want] != 1 {
				t.Errorf("exits = %v, want one %s", exits, tc.want)
			}
			if n := r.gh.removals(1); n != tc.removals {
				t.Errorf("registration 1 removed %d times, want %d", n, tc.removals)
			}
			if lost := strings.Contains(r.log.String(), "reason=lost"); lost != (tc.want == ExitLost) {
				t.Errorf("a log line names reason lost: %t, want %t", lost, tc.want == ExitLost)
			}
		})
	}
}
