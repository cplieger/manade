//go:build integration

package hostit

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// holdProfile holds 64 MiB in a shell variable for 3 seconds, then exits.
const holdProfile = `"hold": {"runner_image": "base", "cpus": 1, "memory": "512MiB", "scopes": ["acme/app"],` +
	` "command": ["/bin/bash", "-c", "x=$(head -c 67108864 /dev/zero | tr '\\0' a); sleep 3; echo ${#x}"]}`

// An exited runner reports the usage its own cgroup kept, read after its
// container exited, and the cgroup goes with the runner's DELETE.
func TestARunnersUsage(t *testing.T) {
	cp := newControlPlane(t)
	h := startHost(t, cp, []string{"MANADE_ALLOW_OPEN=true"})
	boot := settings(`{"egress": {"open": true}}`)
	if _, err := h.deliver(1, boot); err != nil {
		t.Fatal(err)
	}
	h.up(hashOf(t, boot))
	h.load("base")
	withHold := settings(`{"egress": {"open": true}}`, holdProfile)
	if _, err := h.deliver(2, withHold); err != nil {
		t.Fatal(err)
	}
	h.await("profile hold ready", 5*time.Minute, func(st *hostapi.Status) bool {
		return slices.ContainsFunc(st.Profiles, func(p hostapi.ProfileStatus) bool { return p.Name == "hold" && p.Ready })
	})
	r, logs := h.run(t, "hold", 5*time.Minute)
	if *r.ExitCode != 0 || !strings.Contains(logs, "67108864") {
		t.Fatalf("exit %d, logs:\n%s", *r.ExitCode, logs)
	}
	t.Run("usage", func(t *testing.T) {
		u := r.Usage
		if u == nil {
			t.Fatal("the exited runner reports no usage")
		}
		if u.CPU <= 0 || u.Ran < 3*time.Second || u.PeakMemory < 64<<20 || u.PeakMemory > 512<<20 {
			t.Errorf("usage = %+v, want some CPU over at least 3 s and a peak between 64 and 512 MiB", *u)
		}
	})
	t.Run("the cgroup goes with the DELETE", func(t *testing.T) {
		find := "ls -d /sys/fs/cgroup/" + jobspec.CgroupDir + "/" + r.Name + " /sys/fs/cgroup/*/" + jobspec.CgroupDir + "/" + r.Name + " 2>/dev/null; true"
		if got := docker(t, "exec", h.ctr, "sh", "-c", find); got == "" {
			t.Fatal("the exited runner has no cgroup of its own")
		}
		if _, err := h.api().DeleteRunner(t.Context(), r.Name, false); err != nil {
			t.Fatal(err)
		}
		if got := docker(t, "exec", h.ctr, "sh", "-c", find); got != "" {
			t.Errorf("cgroups left after the DELETE: %s", got)
		}
	})
}
