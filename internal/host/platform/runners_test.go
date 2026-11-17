package platform

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"
)

const runnersDir = "manade-runners"

func openRunners(t *testing.T, r Root) *RunnerCgroups {
	t.Helper()
	ms, err := Mountinfo(r)
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenRunnerCgroups(r, ms, runnersDir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// v2Runner is runner r1's parent on a cgroup v2 host, as measured on Linux
// 6.12: 300 MiB in use, of which 150 MiB page cache and 100 MiB of that
// inactive, a 280 MiB watermark and 3.29 s of CPU.
func v2Runner() map[string]string {
	p := "/sys/fs/cgroup/" + runnersDir + "/r1/"
	return with(v2Layout(), map[string]string{
		p + "memory.current": "314572800\n",
		p + "memory.stat":    "anon 157286400\nfile 157286400\nactive_file 52428800\ninactive_file 104857600\n",
		p + "memory.peak":    "293601280\n",
		p + "cpu.stat":       "usage_usec 3290016\nuser_usec 3007352\nsystem_usec 265595\n",
	})
}

func TestRunnerCgroupsV2(t *testing.T) {
	c := openRunners(t, tree(t, v2Runner()))
	t.Run("working set", func(t *testing.T) {
		ws, found, err := c.WorkingSet("r1")
		if err != nil || !found || ws != 200*mib {
			t.Errorf("WorkingSet = %d, %t, %v, want 200 MiB: the use less inactive page cache", ws, found, err)
		}
	})
	t.Run("ended", func(t *testing.T) {
		e, found, err := c.Ended("r1")
		if err != nil || !found || e.CPU != 3290016*time.Microsecond || e.Watermark != 280*mib {
			t.Errorf("Ended = %+v, %t, %v, want 3.290016 s and the 280 MiB watermark", e, found, err)
		}
	})
	t.Run("a runner with no cgroup", func(t *testing.T) {
		if _, found, err := c.WorkingSet("r2"); found || err != nil {
			t.Errorf("WorkingSet of r2 = %t, %v, want not found", found, err)
		}
		if _, found, err := c.Ended("r2"); found || err != nil {
			t.Errorf("Ended of r2 = %t, %v, want not found", found, err)
		}
	})
}

// A kernel before 5.19 keeps no memory.peak, so an ended runner has no
// watermark.
func TestRunnerCgroupsV2WithoutPeak(t *testing.T) {
	c := openRunners(t, tree(t, with(v2Runner(), map[string]string{"/sys/fs/cgroup/" + runnersDir + "/r1/memory.peak": ""})))
	e, found, err := c.Ended("r1")
	if err != nil || !found || e.Watermark != 0 || e.CPU == 0 {
		t.Errorf("Ended = %+v, %t, %v, want the CPU time and no watermark", e, found, err)
	}
}

func TestRunnerCgroupsV1(t *testing.T) {
	c := openRunners(t, tree(t, with(v1Layout(), map[string]string{
		"/sys/fs/cgroup/memory/" + runnersDir + "/r1/memory.usage_in_bytes":     "537444352\n",
		"/sys/fs/cgroup/memory/" + runnersDir + "/r1/memory.stat":               "cache 323162112\nrss 214282240\ninactive_file 0\ntotal_inactive_file 323162112\n",
		"/sys/fs/cgroup/memory/" + runnersDir + "/r1/memory.max_usage_in_bytes": "537444352\n",
		"/sys/fs/cgroup/cpuacct/" + runnersDir + "/r1/cpuacct.usage":            "6095751116\n",
	})))
	ws, found, err := c.WorkingSet("r1")
	if err != nil || !found || ws != 214282240 {
		t.Errorf("WorkingSet = %d, %t, %v, want the hierarchical use less total_inactive_file", ws, found, err)
	}
	e, found, err := c.Ended("r1")
	if err != nil || !found || e.CPU != 6095751116*time.Nanosecond || e.Watermark != 537444352 {
		t.Errorf("Ended = %+v, %t, %v, want cpuacct.usage and memory.max_usage_in_bytes", e, found, err)
	}
}

func TestRunnerCgroupsRefuseABadFigure(t *testing.T) {
	c := openRunners(t, tree(t, with(v2Runner(), map[string]string{"/sys/fs/cgroup/" + runnersDir + "/r1/cpu.stat": "user_usec 1\n"})))
	if _, _, err := c.Ended("r1"); !errors.Is(err, errCgroup) {
		t.Errorf("Ended with no usage_usec = %v, want errCgroup", err)
	}
	c = openRunners(t, tree(t, with(v2Runner(), map[string]string{"/sys/fs/cgroup/" + runnersDir + "/r1/memory.current": "lots\n"})))
	if _, _, err := c.WorkingSet("r1"); err == nil {
		t.Error("WorkingSet of an unparsable memory.current answered no error")
	}
}

// A runner's cgroup is removed from every hierarchy runc made it in, the
// unified one of a hybrid layout included, and Names lists each runner any
// hierarchy holds.
func TestRunnerCgroupsRemoveEveryHierarchy(t *testing.T) {
	files := with(v1Layout(), map[string]string{
		"/proc/self/mountinfo": v1Layout()["/proc/self/mountinfo"] +
			"611 599 0:71 / /sys/fs/cgroup/unified rw - cgroup2 cgroup2 rw\n",
	})
	r := tree(t, files)
	hierarchies := []string{"cpu", "cpuacct", "blkio", "memory", "freezer", "cpuset", "devices", "unified"}
	for _, h := range hierarchies {
		for _, n := range []string{"r1", "r2"} {
			if err := os.MkdirAll(r.Path("/sys/fs/cgroup/"+h+"/"+runnersDir+"/"+n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(r.Path("/sys/fs/cgroup/memory/"+runnersDir+"/r3"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := openRunners(t, r)
	if names, err := c.Names(); err != nil || !slices.Equal(names, []string{"r1", "r2", "r3"}) {
		t.Errorf("Names = %q, %v, want r1, r2 and r3", names, err)
	}
	if err := c.Remove("r1"); err != nil {
		t.Fatal(err)
	}
	for _, h := range hierarchies {
		if _, err := os.Stat(r.Path("/sys/fs/cgroup/" + h + "/" + runnersDir + "/r1")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("r1 left in %s: %v", h, err)
		}
		if _, err := os.Stat(r.Path("/sys/fs/cgroup/" + h + "/" + runnersDir + "/r2")); err != nil {
			t.Errorf("r2 gone from %s: %v", h, err)
		}
	}
	if err := c.Remove("r3"); err != nil {
		t.Errorf("removing a runner only one hierarchy holds = %v", err)
	}
	if err := c.Remove("gone"); err != nil {
		t.Errorf("removing a runner no hierarchy holds = %v", err)
	}
	if names, err := c.Names(); err != nil || !slices.Equal(names, []string{"r2"}) {
		t.Errorf("Names after the removals = %q, %v, want r2", names, err)
	}
}

// A cgroup that still holds a process is not removed: rmdir answers EBUSY,
// which a non-empty directory stands for here.
func TestRunnerCgroupsRemoveReportsABusyOne(t *testing.T) {
	r := tree(t, with(v2Runner(), nil))
	c := openRunners(t, r)
	if err := c.Remove("r1"); err == nil {
		t.Error("Remove of a cgroup rmdir refuses answered no error")
	}
}
