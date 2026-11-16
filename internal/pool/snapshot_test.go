package pool

import (
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
)

func TestSnapshot(t *testing.T) {
	r := newRig(t, oneHost())
	r.assign(r.small(), 2, 1)
	jobs := []github.QueuedJob{
		{Created: r.now.Add(-time.Minute), Name: "a", Profile: "small", RunID: 1},
		{Created: r.now.Add(-30 * time.Second), Name: "b", Profile: "small", RunID: 2},
	}
	r.p.queued(r.now, []github.Queue{{Scope: app, Jobs: jobs, Complete: true}})
	snap := r.reg.Snapshot()
	if !snap.HeadSince.Equal(jobs[0].Created) {
		t.Errorf("head since %v, want the oldest job's %v", snap.HeadSince, jobs[0].Created)
	}
	t.Run("the host's reservation and runners", func(t *testing.T) {
		if len(snap.Hosts) != 1 {
			t.Fatalf("hosts %+v, want default only", snap.Hosts)
		}
		h := snap.Hosts[0]
		if h.Host != "default" || h.Reserved.MilliCPUs != 2000 || h.Reserved.Memory != 4<<30 {
			t.Errorf("host %+v, want default holding one 2-CPU 4-GiB runner", h)
		}
		want := []RunnerCounts{{ScaleSet: r.small().ss, Starting: 1}}
		if len(h.Runners) != 1 || h.Runners[0] != want[0] {
			t.Errorf("runners %+v, want %+v", h.Runners, want)
		}
	})
	t.Run("the assigned job and the one waiting", func(t *testing.T) {
		if len(snap.Sets) != 1 {
			t.Fatalf("sets %+v, want small in acme/app only", snap.Sets)
		}
		want := SetCounts{ScaleSet: r.small().ss, AssignedUnstarted: 1, Queued: 1}
		if snap.Sets[0] != want {
			t.Errorf("set %+v, want %+v", snap.Sets[0], want)
		}
	})
	t.Run("a waiting job no host can take counts as no_host", func(t *testing.T) {
		r.hosts[0].set(func(f *fakeHost) { f.down = true })
		r.pass()
		if c := r.reg.Snapshot().Sets[0]; c.NoHost != 1 || c.Queued != 0 {
			t.Errorf("set %+v with its only host down, want the waiting job as no_host", c)
		}
	})
}
