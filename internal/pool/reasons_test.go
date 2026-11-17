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

// listed queues jobs on scope's profile in a whole read, the oldest first,
// each a minute apart and ending at the rig's clock.
func (r *rig) listed(scope, profile string, runs ...int64) {
	var jobs []github.QueuedJob
	for i, run := range runs {
		jobs = append(jobs, github.QueuedJob{Created: r.now.Add(time.Duration(i-len(runs)) * time.Minute), Name: "job", Profile: profile, RunID: run})
	}
	r.p.queued(r.now, []github.Queue{{Scope: scope, Jobs: jobs, Complete: true}})
}

// waitingRun answers the snapshot's waiting job of run.
func waitingRun(t *testing.T, snap *Snapshot, run int64) Waiting {
	t.Helper()
	i := slices.IndexFunc(snap.Waiting, func(w Waiting) bool { return !w.Assigned && w.RunID == run })
	if i < 0 {
		t.Fatalf("run %d is not waiting: %+v", run, snap.Waiting)
	}
	return snap.Waiting[i]
}

// hostWaits maps a waiting job's hosts to their reasons and details.
func hostWaits(w *Waiting) map[string]HostWait {
	out := map[string]HostWait{}
	for _, hw := range w.Hosts {
		out[hw.Host] = hw
	}
	return out
}

// busy starts one runner of b and has it take a job.
func (r *rig) busy(b *book) {
	r.assign(b, 1, 0)
	r.pass()
	r.p.mu.Lock()
	var name string
	for n, rr := range b.runners {
		if rr.state != busy {
			name = n
		}
	}
	r.p.mu.Unlock()
	r.poll(b, github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, Name: "running", RunID: 99})
}

// reasonOf runs one pass and answers the waiting job of run 1 on small.
func reasonOf(t *testing.T, r *rig) Waiting {
	t.Helper()
	r.pass()
	r.listed(app, "small", 1)
	snap := r.reg.Snapshot()
	return waitingRun(t, &snap, 1)
}

// Each host reason, read through the job whose one
// host it is, which also reads that job's reason.
func TestEachHostReasonAndTheJobReasonItGives(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		set    func(t *testing.T, r *rig)
		name   string
		host   HostReason
		detail string
		job    JobReason
	}{
		{name: "tooSmall", host: HostTooSmall, job: JobTooLarge, set: func(_ *testing.T, r *rig) {
			cfg := cloneConfig(r.cfg)
			cfg.Profiles[0].MilliCPUs = 16000
			r.apply(cfg)
		}},
		{name: "disconnected", host: HostDisconnected, job: JobNoHostServing, set: func(_ *testing.T, r *rig) {
			r.hosts[0].set(func(f *fakeHost) { f.disconnected = true })
		}},
		{name: "draining", host: HostDraining, job: JobNoHostServing, set: func(_ *testing.T, r *rig) {
			cfg := cloneConfig(r.cfg)
			cfg.Hosts[0].Accepting = false
			r.apply(cfg)
		}},
		{name: "olderSection", host: HostSettingsNotApplied, detail: "waiting_for_section", job: JobHostNotReady, set: func(_ *testing.T, r *rig) {
			r.apply(cloneConfig(r.cfg), "default")
		}},
		{name: "pendingSection", host: HostSettingsNotApplied, detail: "restart_pending", job: JobHostNotReady, set: func(_ *testing.T, r *rig) {
			r.hosts[0].set(func(f *fakeHost) { f.state = hostapi.SectionRestartPending })
		}},
		{name: "unprovedByAFailure", host: HostUnproved, job: JobNoHostServing, set: func(_ *testing.T, r *rig) {
			r.hosts[0].set(func(f *fakeHost) {
				f.probe = func(context.Context) (hostapi.ProbeReport, error) { return hostapi.ProbeReport{Unproven: "dns"}, nil }
			})
			r.hosts[0].restart(stopExitCode)
		}},
		{name: "unprovedPastTheInterval", host: HostUnproved, job: JobNoHostServing, set: func(_ *testing.T, r *rig) {
			r.p.mu.Lock()
			r.p.hosts[0].provedAt = r.now.Add(-reproveEvery - time.Second)
			r.p.mu.Unlock()
			r.hosts[0].set(func(f *fakeHost) {
				f.probe = func(context.Context) (hostapi.ProbeReport, error) {
					return hostapi.ProbeReport{}, hostapi.ErrProbeTimeout
				}
			})
		}},
		{name: "dataFull", host: HostNotReady, detail: "data_full", job: JobHostNotReady, set: func(_ *testing.T, r *rig) {
			r.hosts[0].set(func(f *fakeHost) { f.notReady["small"], f.reasons["small"] = true, hostapi.ReasonDataFull })
		}},
		{name: "capacityOverLimits", host: HostNotReady, detail: "capacity_over_limits", job: JobHostNotReady, set: func(_ *testing.T, r *rig) {
			cfg := cloneConfig(r.cfg)
			cfg.Hosts[0].Capacity.Memory = 1 << 50
			r.apply(cfg)
		}},
		{name: "imagePulling", host: HostImagePulling, job: JobImagePulling, set: func(_ *testing.T, r *rig) {
			r.hosts[0].set(func(f *fakeHost) { f.notReady["small"], f.reasons["small"] = true, hostapi.ReasonPulling })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := quietRig(t, oneHost())
			tc.set(t, r)
			w := reasonOf(t, r)
			hw := hostWaits(&w)["default"]
			if w.Reason != tc.job || hw.Reason != tc.host || hw.Detail != tc.detail {
				t.Errorf("job %q, host %q (%q); want %q, %q (%q)", w.Reason, hw.Reason, hw.Detail, tc.job, tc.host, tc.detail)
			}
		})
	}
}

// A probe running inside its deadline reads proving, while it runs.
func TestAHostWhoseProbeRunsReadsProving(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.listed(app, "small", 1)
	var during Waiting
	r.hosts[0].set(func(f *fakeHost) {
		f.probe = func(context.Context) (hostapi.ProbeReport, error) {
			snap := r.reg.Snapshot()
			during = waitingRun(t, &snap, 1)
			return hostapi.ProbeReport{Proven: true}, nil
		}
	})
	r.hosts[0].restart(stopExitCode)
	r.pass()
	if hw := hostWaits(&during)["default"]; during.Reason != JobProving || hw.Reason != HostProving {
		t.Errorf("during the probe: job %q, host %q; want proving for both", during.Reason, hw.Reason)
	}
	t.Run("aReproofOfAFailedHashIsNotProving", func(t *testing.T) {
		r.hosts[0].set(func(f *fakeHost) {
			f.probe = func(context.Context) (hostapi.ProbeReport, error) { return hostapi.ProbeReport{Unproven: "dns"}, nil }
		})
		r.hosts[0].restart(stopExitCode)
		r.pass()
		r.hosts[0].set(func(f *fakeHost) {
			f.probe = func(context.Context) (hostapi.ProbeReport, error) {
				snap := r.reg.Snapshot()
				during = waitingRun(t, &snap, 1)
				return hostapi.ProbeReport{Unproven: "dns"}, nil
			}
		})
		r.p.mu.Lock()
		wake(r.p.hosts[0].want)
		r.p.mu.Unlock()
		r.prove()
		if hw := hostWaits(&during)["default"]; hw.Reason != HostUnproved {
			t.Errorf("host %q during a re-probe of a failed hash, want unproved", hw.Reason)
		}
	})
}

// A proof older than the re-proof interval stays held while its re-proof
// runs, and the next pass asks for the re-proof.
func TestAReproofKeepsTheHostServingWhileItRuns(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	r.now = r.now.Add(reproveEvery + time.Minute)
	served := false
	r.hosts[0].set(func(f *fakeHost) {
		f.probe = func(context.Context) (hostapi.ProbeReport, error) {
			r.p.mu.Lock()
			served = r.p.hosts[0].serving(r.now)
			r.p.mu.Unlock()
			return hostapi.ProbeReport{Proven: true}, nil
		}
	})
	r.pass()
	if !served {
		t.Error("the host did not serve while its re-proof ran")
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if !r.p.hosts[0].provedAt.Equal(r.now) || !r.p.hosts[0].serving(r.now) {
		t.Errorf("proved at %v, want the re-proof's %v", r.p.hosts[0].provedAt, r.now)
	}
}

// The job-reason rows the host table does not already give.
func TestEachJobReason(t *testing.T) {
	t.Parallel()
	t.Run("starting", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.assign(r.small(), 1, 0)
		snap := r.reg.Snapshot()
		if len(snap.Waiting) != 1 || !snap.Waiting[0].Assigned || snap.Waiting[0].Reason != JobStarting {
			t.Errorf("waiting %+v, want the assignment starting", snap.Waiting)
		}
	})
	t.Run("awaitingAssignment", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		if w := reasonOf(t, r); w.Reason != JobAwaitingAssignment || w.Hosts != nil {
			t.Errorf("job %+v, want awaiting assignment with no host reasons", w)
		}
	})
	t.Run("unassignable", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.listed(app, "small", 1)
		b := r.small()
		r.reg.Opened(b.ss, github.Stats{})
		for range 2 {
			r.poll(b, github.Stats{Available: 1})
			r.now = r.now.Add(countedPoll)
		}
		r.listed(app, "small", 1)
		snap := r.reg.Snapshot()
		if w := waitingRun(t, &snap, 1); w.Reason != JobUnassignable {
			t.Errorf("job %q after two funded polls GitHub filled nothing for, want unassignable", w.Reason)
		}
	})
	t.Run("scopeHeldByAnotherPool", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.reg.Held(r.small().ss, true, "other-pool")
		if w := reasonOf(t, r); w.Reason != JobScopeNotServed || w.Detail != DetailHeldByOtherPool {
			t.Errorf("job %q (%q), want scope_not_served (held_by_other_pool)", w.Reason, w.Detail)
		}
	})
	t.Run("scopeNotEnsured", func(t *testing.T) {
		t.Parallel()
		r := quietRig(t, oneHost())
		r.gh.set(func(g *fakeGitHub) { g.ensureFail = true })
		cfg := cloneConfig(r.cfg)
		cfg.Scopes = append(cfg.Scopes, config.Scope{Key: "acme/web", Owner: "acme", Repo: "web", Kind: config.ScopeRepo, DefaultProfile: defaultOf(cfg, "acme/web")})
		r.apply(cfg)
		r.listed("acme/web", "small", 1)
		snap := r.reg.Snapshot()
		if w := waitingRun(t, &snap, 1); w.Reason != JobScopeNotServed || w.Detail != DetailEnsureFailed || w.Scope != "acme/web" {
			t.Errorf("job %+v, want scope_not_served (ensure_failed) on acme/web", w)
		}
	})
	t.Run("profileLimit", func(t *testing.T) {
		t.Parallel()
		cfg := oneHost()
		cfg.Profiles[0].MaxRunners = 1
		r := quietRig(t, cfg)
		r.busy(r.small())
		w := reasonOf(t, r)
		if w.Reason != JobProfileLimit || w.Runners != 1 || w.Limit != 1 {
			t.Errorf("job %+v, want profile_limit at 1 of 1", w)
		}
	})
	t.Run("waitingForSlot", func(t *testing.T) {
		t.Parallel()
		cfg := poolConfig([]hostSpec{{name: "default", cpus: 2, mem: 4}}, []profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app)
		r := quietRig(t, cfg)
		r.busy(r.small())
		w := reasonOf(t, r)
		if hw := hostWaits(&w)["default"]; w.Reason != JobWaitingForSlot || hw.Reason != HostFull || hw.Runners != 1 {
			t.Errorf("job %q, host %+v; want waiting_for_slot beside a full host with its 1 runner", w.Reason, hw)
		}
	})
	t.Run("heldForOlder", func(t *testing.T) {
		t.Parallel()
		cfg := poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}}, []profileSpec{
			{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4},
			{name: "large", hosts: []string{"default"}, cpus: 8, mem: 8},
		}, app)
		r := quietRig(t, cfg)
		r.busy(r.small())
		r.p.queued(r.now, []github.Queue{{Scope: app, Complete: true, Jobs: []github.QueuedJob{
			{Created: r.now.Add(-5 * time.Minute), Name: "big", Profile: "large", RunID: 7},
			{Created: r.now.Add(-time.Minute), Name: "job", Profile: "small", RunID: 1},
			{Created: r.now, Name: "job", Profile: "small", RunID: 2},
		}}})
		snap := r.reg.Snapshot()
		if first := waitingRun(t, &snap, 1); first.Reason != JobAwaitingAssignment {
			t.Errorf("the small job the idle runner covers reads %q, want awaiting_assignment", first.Reason)
		}
		w := waitingRun(t, &snap, 2)
		if hw := hostWaits(&w)["default"]; w.Reason != JobHeldForOlder || hw.Reason != HostHeld {
			t.Errorf("small job %q, host %q; want held_for_older behind the large job's reserve", w.Reason, hw.Reason)
		}
		if big := waitingRun(t, &snap, 7); big.Reason != JobWaitingForSlot {
			t.Errorf("large job %q, want waiting_for_slot", big.Reason)
		}
	})
}

// A job whose hosts differ takes the reason of the host nearest a slot and
// lists every host with its own reason.
func TestAMixedHostJobReadsTheNearestHost(t *testing.T) {
	t.Parallel()
	cfg := poolConfig([]hostSpec{{name: "a", cpus: 2, mem: 4}, {name: "b", cpus: 8, mem: 32}, {name: "c", cpus: 8, mem: 32}},
		[]profileSpec{{name: "small", hosts: []string{"a", "b", "c"}, cpus: 2, mem: 4}}, app)
	r := quietRig(t, cfg)
	r.hosts[1].set(func(f *fakeHost) { f.disconnected = true })
	r.hosts[2].set(func(f *fakeHost) { f.notReady["small"], f.reasons["small"] = true, hostapi.ReasonDataFull })
	r.pass()
	r.busy(r.small())
	w := reasonOf(t, r)
	got := hostWaits(&w)
	if w.Reason != JobWaitingForSlot || got["a"].Reason != HostFull || got["b"].Reason != HostDisconnected ||
		got["c"].Reason != HostNotReady || got["c"].Detail != "data_full" {
		t.Errorf("job %q, hosts %+v; want waiting_for_slot with a full, b disconnected, c not_ready (data_full)", w.Reason, got)
	}
}
