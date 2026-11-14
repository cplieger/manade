package platform

import (
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// tree writes files, each path absolute under the answered Root.
func tree(t *testing.T, files map[string]string) Root {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Root(dir)
}

// with answers base with over's entries added or replaced; an empty value
// removes the entry.
func with(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	for k, v := range over {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

const hostID = "71f25d718cdfdb7f9c6bfb20dfa09157efad331ea2abda5054d66390cd35f317"

// v2Layout is a cgroup v2 host in a private cgroup namespace after the dind
// script moved it into /init, as measured under Docker 28: a 2 GiB limit and
// a 2-CPU quota one level above /init, whose own limits read max.
func v2Layout() map[string]string {
	return map[string]string{
		"/proc/self/mountinfo": "" +
			"500 400 0:50 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w\n" +
			"501 500 0:51 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw,nsdelegate\n" +
			"502 500 0:52 /data/manade-host /var/lib/docker rw,relatime - zfs pool/manade-host rw,xattr\n" +
			"503 500 0:53 /etc/resolv.conf /etc/resolv.conf rw - zfs pool/ix rw\n",
		"/proc/self/cgroup":                          "0::/init\n",
		"/sys/fs/cgroup/cgroup.controllers":          "cpuset cpu io memory pids\n",
		"/sys/fs/cgroup/memory.max":                  "2147483648\n",
		"/sys/fs/cgroup/memory.swap.max":             "0\n",
		"/sys/fs/cgroup/cpu.max":                     "200000 100000\n",
		"/sys/fs/cgroup/pids.max":                    "max\n",
		"/sys/fs/cgroup/cpuset.cpus.effective":       "0-19\n",
		"/sys/fs/cgroup/memory.events":               "low 0\nhigh 0\nmax 4\noom 3\noom_kill 3\noom_group_kill 0\n",
		"/sys/fs/cgroup/memory.events.local":         "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n",
		"/sys/fs/cgroup/init/memory.max":             "max\n",
		"/sys/fs/cgroup/init/memory.swap.max":        "max\n",
		"/sys/fs/cgroup/init/cpu.max":                "max 100000\n",
		"/sys/fs/cgroup/init/pids.max":               "max\n",
		"/sys/fs/cgroup/init/cpuset.cpus.effective":  "0-19\n",
		"/sys/fs/cgroup/init/memory.events":          "oom_kill 0\n",
		"/sys/fs/cgroup/init/memory.events.local":    "oom_kill 0\n",
		"/sys/fs/cgroup/init/memory.stat":            v2Stat,
		"/sys/devices/system/cpu/online":             "0-19\n",
		"/proc/filesystems":                          "nodev\tproc\nnodev\toverlay\n\text4\n",
		"/sys/fs/cgroup/init/cgroup.subtree_control": "",
	}
}

// v2Stat is /init's memory.stat: 1 GiB of page cache ("file") beside the
// host's own figures, which sum to 100 MiB.
const v2Stat = "anon 41943040\nfile 1073741824\nkernel_stack 1048576\npagetables 2097152\n" +
	"sock 1048576\nshmem 0\nfile_mapped 52428800\nfile_dirty 0\nslab_reclaimable 8388608\n" +
	"slab_unreclaimable 6291456\nslab 14680064\ninactive_file 536870912\n"

// v1Layout is a cgroup v1 host in the machine's cgroup namespace, as
// measured under Docker 24 on Linux 4.4: each controller is mounted with
// the host's own cgroup as its root, there is no CFS quota and no pids
// controller, and a 3 GiB limit and a 2-CPU cpuset cap the host.
func v1Layout() map[string]string {
	id := "/docker/" + hostID
	m := func(n int, ctrl string) string {
		return strconv.Itoa(600+n) + " 500 0:" + strconv.Itoa(60+n) + " " + id + " /sys/fs/cgroup/" + ctrl + " rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw," + ctrl + "\n"
	}
	return map[string]string{
		"/proc/self/mountinfo": "" +
			"500 400 0:50 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w\n" +
			"599 500 0:59 / /sys/fs/cgroup rw,nosuid,nodev,noexec - tmpfs tmpfs rw,mode=755\n" +
			m(1, "cpu") + m(2, "cpuacct") + m(3, "blkio") + m(4, "memory") + m(5, "freezer") + m(6, "cpuset") + m(7, "devices") +
			"610 599 0:70 /system.slice/pkg-ContainerManager-dockerd.service /sys/fs/cgroup/synomonitor rw - cgroup cgroup rw,name=synomonitor\n" +
			"620 500 0:80 /@docker/volumes/manade /var/lib/docker rw,relatime - btrfs /dev/md2 rw,space_cache\n",
		"/proc/self/cgroup": "" +
			"9:devices:" + id + "\n8:cpuset:" + id + "\n7:freezer:" + id + "\n6:memory:" + id + "\n" +
			"5:blkio:" + id + "\n4:cpuacct:" + id + "\n3:cpu:" + id + "\n" +
			"2:name=synomonitor:/system.slice/pkg-ContainerManager-dockerd.service\n1:name=systemd:" + id + "\n",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes":       "3221225472\n",
		"/sys/fs/cgroup/memory/memory.memsw.limit_in_bytes": "3221225472\n",
		"/sys/fs/cgroup/memory/memory.use_hierarchy":        "1\n",
		"/sys/fs/cgroup/memory/memory.stat":                 v1Stat,
		"/sys/fs/cgroup/cpu/cpu.shares":                     "1024\n",
		"/sys/fs/cgroup/cpuset/cpuset.effective_cpus":       "0-1\n",
		"/sys/fs/cgroup/cpuset/cpuset.cpus":                 "0-1\n",
		"/sys/devices/system/cpu/online":                    "0-3\n",
		"/proc/filesystems":                                 "nodev\tproc\n\tbtrfs\n\text4\n",
	}
}

// v1Stat is the host cgroup's memory.stat: its own figures and the
// hierarchical total_* ones, with 1 GiB of page cache.
const v1Stat = "cache 1073741824\nrss 73400320\nrss_huge 0\nmapped_file 30408704\nswap 0\n" +
	"total_cache 2147483648\ntotal_rss 536870912\ntotal_mapped_file 104857600\n"
