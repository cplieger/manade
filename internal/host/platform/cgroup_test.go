package platform

import (
	"errors"
	"strings"
	"testing"
)

const (
	mib = 1 << 20
	gib = 1 << 30
)

func TestReadV2PrivateNamespace(t *testing.T) {
	c, err := Read(tree(t, v2Layout()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != V2 {
		t.Errorf("Version = %d, want 2", c.Version)
	}
	if c.Memory != 2*gib {
		t.Errorf("Memory = %d, want the 2 GiB one level above /init", c.Memory)
	}
	if !c.Quota || c.MilliCPUs != 2000 {
		t.Errorf("Quota, MilliCPUs = %t, %d, want true, 2000 from cpu.max 200000 100000", c.Quota, c.MilliCPUs)
	}
	if c.CPUs != 20 || c.CPUSet != "0-19" {
		t.Errorf("CPUs, CPUSet = %d, %q, want 20, 0-19", c.CPUs, c.CPUSet)
	}
	if !c.PIDsController || c.PIDs != 0 {
		t.Errorf("PIDsController, PIDs = %t, %d, want true, 0 for max", c.PIDsController, c.PIDs)
	}
	if !c.Swap {
		t.Error("Swap = false, want true with memory.swap.max present")
	}
	if c.SubtreeWide {
		t.Error("SubtreeWide = true on cgroup v2")
	}
}

func TestOwnMemoryExcludesPageCache(t *testing.T) {
	t.Run("v2", func(t *testing.T) {
		c, err := Read(tree(t, v2Layout()))
		if err != nil {
			t.Fatal(err)
		}
		// anon 40 + file_mapped 50 + kernel_stack 1 + pagetables 2 + sock 1 +
		// slab_unreclaimable 6 MiB of /init; its 1 GiB of file left out.
		if c.OwnMemory != 100*mib {
			t.Errorf("OwnMemory = %d, want 100 MiB", c.OwnMemory)
		}
	})
	t.Run("v1", func(t *testing.T) {
		c, err := Read(tree(t, v1Layout()))
		if err != nil {
			t.Fatal(err)
		}
		// rss 70 + mapped_file 29 MiB; cache and the total_* figures left out.
		if c.OwnMemory != 99*mib {
			t.Errorf("OwnMemory = %d, want 99 MiB", c.OwnMemory)
		}
	})
	t.Run("missingKey", func(t *testing.T) {
		_, err := Read(tree(t, with(v2Layout(), map[string]string{"/sys/fs/cgroup/init/memory.stat": "anon 1\n"})))
		if !errors.Is(err, ErrCgroup) || !strings.Contains(err.Error(), "file_mapped") {
			t.Errorf("err = %v, want ErrCgroup naming file_mapped", err)
		}
	})
}

func TestReadV1MachineNamespace(t *testing.T) {
	c, err := Read(tree(t, v1Layout()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != V1 || c.Memory != 3*gib {
		t.Errorf("Version, Memory = %d, %d, want 1, 3 GiB at the controller's root", c.Version, c.Memory)
	}
	if c.Quota {
		t.Error("Quota = true on a kernel without cpu.cfs_quota_us")
	}
	if c.MilliCPUs != 2000 || c.CPUs != 2 || c.CPUSet != "0-1" {
		t.Errorf("MilliCPUs, CPUs, CPUSet = %d, %d, %q, want the cpuset's 2000, 2, 0-1", c.MilliCPUs, c.CPUs, c.CPUSet)
	}
	if c.PIDsController {
		t.Error("PIDsController = true with no pids controller mounted")
	}
	if !c.Swap || !c.SubtreeWide {
		t.Errorf("Swap, SubtreeWide = %t, %t, want true, true", c.Swap, c.SubtreeWide)
	}
}

func TestReadV1Quota(t *testing.T) {
	c, err := Read(tree(t, with(v1Layout(), map[string]string{
		"/sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "150000\n",
		"/sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Quota || c.MilliCPUs != 1500 {
		t.Errorf("Quota, MilliCPUs = %t, %d, want true, 1500", c.Quota, c.MilliCPUs)
	}
	if c.CPUs != 2 {
		t.Errorf("CPUs = %d, want the cpuset's 2", c.CPUs)
	}
}

func TestReadV1NoQuotaSet(t *testing.T) {
	c, err := Read(tree(t, with(v1Layout(), map[string]string{
		"/sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "-1\n",
		"/sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Quota || c.MilliCPUs != 2000 {
		t.Errorf("Quota, MilliCPUs = %t, %d, want true (CFS present), the cpuset's 2000", c.Quota, c.MilliCPUs)
	}
}

func TestReadV1RefusesNonHierarchicalMemory(t *testing.T) {
	_, err := Read(tree(t, with(v1Layout(), map[string]string{"/sys/fs/cgroup/memory/memory.use_hierarchy": "0\n"})))
	if !errors.Is(err, ErrCgroup) || !strings.Contains(err.Error(), "memory.use_hierarchy") {
		t.Errorf("err = %v, want ErrCgroup naming memory.use_hierarchy", err)
	}
}

func TestReadV1Unlimited(t *testing.T) {
	c, err := Read(tree(t, with(v1Layout(), map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.Memory != 0 {
		t.Errorf("Memory = %d, want 0 for PAGE_COUNTER_MAX", c.Memory)
	}
}

func TestReadRefusesACgroupOutsideItsMount(t *testing.T) {
	_, err := Read(tree(t, with(v1Layout(), map[string]string{
		"/proc/self/cgroup": "6:memory:/docker/other\n8:cpuset:/docker/other\n3:cpu:/docker/other\n",
	})))
	if !errors.Is(err, ErrCgroup) {
		t.Errorf("err = %v, want ErrCgroup", err)
	}
}

func TestOOMCount(t *testing.T) {
	t.Run("counted", func(t *testing.T) {
		c, err := Read(tree(t, v2Layout()))
		if err != nil {
			t.Fatal(err)
		}
		if !c.OOMCounted || c.OOMKills != 3 || c.OOMAbsent != "" {
			t.Errorf("OOMCounted, OOMKills, OOMAbsent = %t, %d, %q, want the limit's cgroup's 3", c.OOMCounted, c.OOMKills, c.OOMAbsent)
		}
	})
	t.Run("localEventsSuperOption", func(t *testing.T) {
		files := with(v2Layout(), map[string]string{"/proc/self/mountinfo": "" +
			"500 400 0:50 / / rw - overlay overlay rw\n" +
			"501 500 0:51 / /sys/fs/cgroup rw,nosuid - cgroup2 cgroup rw,nsdelegate,memory_localevents\n"})
		c, err := Read(tree(t, files))
		if err != nil {
			t.Fatal(err)
		}
		if c.OOMCounted || !strings.Contains(c.OOMAbsent, "memory_localevents") {
			t.Errorf("OOMCounted, OOMAbsent = %t, %q, want absent naming memory_localevents", c.OOMCounted, c.OOMAbsent)
		}
	})
	t.Run("localEventsMountOptionOnly", func(t *testing.T) {
		files := with(v2Layout(), map[string]string{"/proc/self/mountinfo": "" +
			"500 400 0:50 / / rw - overlay overlay rw\n" +
			"501 500 0:51 / /sys/fs/cgroup rw,memory_localevents - cgroup2 cgroup rw,nsdelegate\n"})
		c, err := Read(tree(t, files))
		if err != nil {
			t.Fatal(err)
		}
		if !c.OOMCounted {
			t.Errorf("OOMCounted = false (%q), want counted: the option is only among the mount options", c.OOMAbsent)
		}
	})
	t.Run("noLocalEvents", func(t *testing.T) {
		c, err := Read(tree(t, with(v2Layout(), map[string]string{"/sys/fs/cgroup/memory.events.local": ""})))
		if err != nil {
			t.Fatal(err)
		}
		if c.OOMCounted || !strings.Contains(c.OOMAbsent, "memory.events.local") {
			t.Errorf("OOMCounted, OOMAbsent = %t, %q, want absent naming memory.events.local", c.OOMCounted, c.OOMAbsent)
		}
	})
	t.Run("v1", func(t *testing.T) {
		c, err := Read(tree(t, v1Layout()))
		if err != nil {
			t.Fatal(err)
		}
		if c.OOMCounted || !strings.Contains(c.OOMAbsent, "cgroup v1") {
			t.Errorf("OOMCounted, OOMAbsent = %t, %q, want absent naming cgroup v1", c.OOMCounted, c.OOMAbsent)
		}
	})
}

func TestCheckPrivateNamespace(t *testing.T) {
	cases := map[string]struct {
		over map[string]string
		ok   bool
	}{
		"privateAtRoot":        {over: map[string]string{"/proc/self/cgroup": "0::/\n"}, ok: true},
		"machineNamespacePath": {over: map[string]string{"/proc/self/cgroup": "0::/docker/" + hostID + "\n"}},
		"otherMountRoot": {over: map[string]string{
			"/proc/self/cgroup": "0::/\n",
			"/proc/self/mountinfo": "500 400 0:50 / / rw - overlay overlay rw\n" +
				"501 500 0:51 /docker/" + hostID + " /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
		}},
		"hybrid": {ok: true, over: map[string]string{
			"/sys/fs/cgroup/cgroup.controllers": "",
			"/proc/self/cgroup":                 "0::/docker/" + hostID + "\n",
			"/proc/self/mountinfo": "500 400 0:50 / / rw - overlay overlay rw\n" +
				"501 500 0:51 / /sys/fs/cgroup/unified rw - cgroup2 cgroup rw\n",
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckPrivateNamespace(tree(t, with(v2Layout(), tc.over)))
			if tc.ok && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if !tc.ok && (!errors.Is(err, ErrCgroup) || !strings.Contains(err.Error(), "cgroup: private")) {
				t.Errorf("err = %v, want ErrCgroup naming cgroup: private", err)
			}
		})
	}
}

func TestHybridReadsAsV1(t *testing.T) {
	files := v1Layout()
	files["/proc/self/mountinfo"] += "630 599 0:90 / /sys/fs/cgroup/unified rw - cgroup2 cgroup rw\n"
	files["/proc/self/cgroup"] += "0::/docker/" + hostID + "\n"
	c, err := Read(tree(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != V1 || c.Memory != 3*gib {
		t.Errorf("Version, Memory = %d, %d, want 1, 3 GiB", c.Version, c.Memory)
	}
}

func TestParseCPUList(t *testing.T) {
	for s, want := range map[string]int{"0-3": 4, "0-3,8,10-11": 7, "5": 1, "0,1\n": 2} {
		got, err := ParseCPUList(s)
		if err != nil || len(got) != want {
			t.Errorf("ParseCPUList(%q) = %v, %v, want %d CPUs", s, got, err, want)
		}
	}
	for _, s := range []string{"", "3-1", "a", "-1", "0-x"} {
		if _, err := ParseCPUList(s); err == nil {
			t.Errorf("ParseCPUList(%q) = nil error, want a refusal", s)
		}
	}
}
