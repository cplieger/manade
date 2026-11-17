package pool

import (
	"testing"

	"github.com/cplieger/manade/internal/github"
)

// drained commits the settings with host name's accepting set.
func drained(r *rig, name string, accepting bool) {
	cfg := cloneConfig(r.cfg)
	for i := range cfg.Hosts {
		if cfg.Hosts[i].Name == name {
			cfg.Hosts[i].Accepting = accepting
		}
	}
	r.apply(cfg)
}

// A drained host takes no new reservation and retires its idle runners,
// its running job finishes there, it keeps its proof, and the drain holds
// across a restart of either role until Resume.
func TestADrainedHostTakesNoNewJobUntilResumed(t *testing.T) {
	t.Parallel()
	r := quietRig(t, oneHost())
	b := r.small()
	r.busy(b)
	r.assign(b, 2, 1)
	r.pass()
	if n := len(r.hosts[0].running()); n != 2 {
		t.Fatalf("%d runners before the drain, want one busy and one idle", n)
	}
	drained(r, "default", false)
	r.pass()
	if n := len(r.hosts[0].running()); n != 1 {
		t.Fatalf("%d runners on the drained host, want the busy one alone, its idle one retired", n)
	}
	r.assign(b, 2, 1)
	if n := len(r.hosts[0].running()); n != 1 {
		t.Errorf("%d runners after a new assignment, want no start on a drained host", n)
	}
	t.Run("keepsItsProof", func(t *testing.T) {
		r.p.mu.Lock()
		defer r.p.mu.Unlock()
		if h := r.p.hosts[0]; h.proof(r.now) != proofHeld {
			t.Errorf("a drained host's proof reads %v, want held", h.proof(r.now))
		}
	})
	t.Run("acrossAControlPlaneRestart", func(t *testing.T) {
		r.restart()
		r.assign(r.small(), 2, 1)
		r.pass()
		w := r.reg.Snapshot().Waiting
		if len(w) != 1 || w[0].Reason != JobNoHostServing || w[0].Hosts[0].Reason != hostDraining {
			t.Errorf("waiting %+v, want the assignment held by the drain", w)
		}
	})
	t.Run("acrossAHostRestart", func(t *testing.T) {
		r.hosts[0].restart(stopExitCode)
		r.pass()
		r.assign(r.small(), 1, 0)
		if n := len(r.hosts[0].running()); n != 0 {
			t.Errorf("%d runners on the restarted drained host, want none", n)
		}
	})
	t.Run("resume", func(t *testing.T) {
		drained(r, "default", true)
		r.pass()
		r.reg.Opened(r.small().ss, github.Stats{Assigned: 1})
		r.starts()
		if n := len(r.hosts[0].running()); n == 0 {
			t.Error("no runner started once the host was resumed")
		}
	})
}
