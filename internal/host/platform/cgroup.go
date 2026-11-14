package platform

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Version is a cgroup layout.
type Version int

// The layouts. A hybrid machine, v1 controllers with a cgroup2 mount at
// /sys/fs/cgroup/unified, reads as V1.
const (
	V1 Version = 1
	V2 Version = 2
)

// cgroupRoot is where the kernel's cgroup filesystems are mounted.
const cgroupRoot = "/sys/fs/cgroup"

// unlimitedV1 is the floor of what cgroup v1 reports for no memory limit:
// PAGE_COUNTER_MAX pages, rounded to the page size.
const unlimitedV1 = 1 << 62

// ErrCgroup is a cgroup layout the host cannot read or must not run under.
var ErrCgroup = errors.New("platform: cgroup")

// Cgroup is what the host container's cgroup holds its jobs to, and its
// own figures, read afresh at each Read.
type Cgroup struct {
	// CPUSet is the effective cpuset as the kernel lists it.
	CPUSet string
	// OOMAbsent says why OOMKills is not counted; "" when it is.
	OOMAbsent string
	Version   Version
	// Memory is the smallest memory limit from the host's cgroup up; 0
	// when none is set.
	Memory int64
	// MilliCPUs is the CFS quota where one is set, else the cpuset's size.
	MilliCPUs int64
	// PIDs is the smallest pids limit; 0 when none is set or the kernel has
	// no pids controller.
	PIDs int64
	// OwnMemory is the host's own non-job memory: resident and file-mapped,
	// never unmapped page cache.
	OwnMemory int64
	// OOMKills is the kernel's OOM kill count of the cgroup holding the
	// memory limit and every cgroup below it.
	OOMKills uint64
	// CPUs is the number of CPUs in CPUSet.
	CPUs int
	// Quota is a kernel with CFS bandwidth control; without it a profile's
	// cpus are whole pinned CPUs.
	Quota bool
	// PIDsController, Swap: the kernel enforces a pids limit, a swap limit.
	PIDsController bool
	Swap           bool
	// SubtreeWide is cgroup v1, where one kill at the host's limit sets
	// every running job's OOM flag.
	SubtreeWide bool
	OOMCounted  bool
}

// IsV2 reports whether the machine runs the cgroup v2 layout, by the test
// the dind script makes.
func IsV2(r Root) bool { return r.exists(cgroupRoot + "/cgroup.controllers") }

// CheckPrivateNamespace refuses a cgroup v2 host in the machine's cgroup
// namespace, where the dind script would rewrite the machine's root cgroup
// and the jobs' cgroups would land outside the host's. It must run before
// the dind script, which moves the supervisor into /init.
func CheckPrivateNamespace(r Root) error {
	if !IsV2(r) {
		return nil
	}
	ms, procs, err := readLayout(r)
	if err != nil {
		return err
	}
	m, err := v2Mount(ms)
	if err != nil {
		return err
	}
	if p := procs[""]; p != "/" || m.Root != "/" {
		return fmt.Errorf("%w: the host runs in the machine's cgroup namespace (its cgroup reads %q, the cgroup2 mount's root %q); give the host service compose's cgroup: private",
			ErrCgroup, p, m.Root)
	}
	return nil
}

// Read reads the host's cgroup under r.
func Read(r Root) (*Cgroup, error) {
	ms, procs, err := readLayout(r)
	if err != nil {
		return nil, err
	}
	if IsV2(r) {
		return readV2(r, ms, procs)
	}
	return readV1(r, ms, procs)
}

// readLayout reads mountinfo and /proc/self/cgroup, the latter as each
// controller's path; the cgroup v2 line's controller is "".
func readLayout(r Root) ([]Mount, map[string]string, error) {
	ms, err := Mountinfo(r)
	if err != nil {
		return nil, nil, err
	}
	s, err := r.read("/proc/self/cgroup")
	if err != nil {
		return nil, nil, err
	}
	procs := map[string]string{}
	for line := range strings.SplitSeq(s, "\n") {
		_, rest, ok := strings.Cut(line, ":")
		ctrls, p, ok2 := strings.Cut(rest, ":")
		if !ok || !ok2 {
			continue
		}
		for c := range strings.SplitSeq(ctrls, ",") {
			procs[c] = p
		}
	}
	return ms, procs, nil
}

func v2Mount(ms []Mount) (*Mount, error) {
	for i := range ms {
		if ms[i].FSType == "cgroup2" && ms[i].Point == cgroupRoot {
			return &ms[i], nil
		}
	}
	return nil, fmt.Errorf("%w: no cgroup2 mount at %s", ErrCgroup, cgroupRoot)
}

// v1Mount answers the cgroup v1 mount of controller c.
func v1Mount(ms []Mount, c string) (*Mount, bool) {
	for i := range ms {
		if ms[i].FSType == "cgroup" && ms[i].HasSuper(c) {
			return &ms[i], true
		}
	}
	return nil, false
}

// walk answers the directories from the cgroup at p, a path in the
// namespace's view, up to m's mount point. The two are never joined
// directly: the mount's root may be p itself (cgroup v1 in the machine's
// namespace) or an ancestor of it (cgroup v2's /init).
func walk(m *Mount, p string) ([]string, error) {
	rel, ok := strings.CutPrefix(p, m.Root)
	if !ok || (rel != "" && m.Root != "/" && !strings.HasPrefix(rel, "/")) {
		return nil, fmt.Errorf("%w: cgroup %s lies outside the root %s of the mount at %s", ErrCgroup, p, m.Root, m.Point)
	}
	d := path.Join(m.Point, rel)
	dirs := []string{d}
	for d != m.Point {
		d = path.Dir(d)
		dirs = append(dirs, d)
	}
	return dirs, nil
}

// smallest answers the smallest limit file name holds along dirs, and the
// directory holding it; parse answers false for no limit.
func smallest(r Root, dirs []string, name string, parse func(string) (int64, bool, error)) (limit int64, at string, present bool, err error) {
	for _, d := range dirs {
		s, ok, err := r.lookup(d + "/" + name)
		if err != nil {
			return 0, "", false, err
		}
		if !ok {
			continue
		}
		present = true
		v, set, err := parse(s)
		if err != nil {
			return 0, "", false, fmt.Errorf("%w: %s/%s: %w", ErrCgroup, d, name, err)
		}
		if set && (at == "" || v < limit) {
			limit, at = v, d
		}
	}
	return limit, at, present, nil
}

// first answers the first of names present along dirs.
func first(r Root, dirs []string, names ...string) (s string, ok bool, err error) {
	for _, d := range dirs {
		for _, n := range names {
			if s, ok, err = r.lookup(d + "/" + n); err != nil || ok {
				return s, ok, err
			}
		}
	}
	return "", false, nil
}

func readV2(r Root, ms []Mount, procs map[string]string) (*Cgroup, error) {
	m, err := v2Mount(ms)
	if err != nil {
		return nil, err
	}
	p, ok := procs[""]
	if !ok {
		return nil, fmt.Errorf("%w: /proc/self/cgroup has no cgroup v2 line", ErrCgroup)
	}
	dirs, err := walk(m, p)
	if err != nil {
		return nil, err
	}
	c := &Cgroup{Version: V2}
	var memAt string
	if c.Memory, memAt, _, err = smallest(r, dirs, "memory.max", parseMax); err != nil {
		return nil, err
	}
	if c.MilliCPUs, _, c.Quota, err = smallest(r, dirs, "cpu.max", parseCPUMax); err != nil {
		return nil, err
	}
	if c.PIDs, _, c.PIDsController, err = smallest(r, dirs, "pids.max", parseMax); err != nil {
		return nil, err
	}
	_, c.Swap, err = first(r, dirs, "memory.swap.max")
	if err != nil {
		return nil, err
	}
	if serr := c.readCPUSet(r, dirs, "cpuset.cpus.effective"); serr != nil {
		return nil, serr
	}
	if c.OwnMemory, err = ownMemory(r, dirs[0], "anon", "file_mapped", "kernel_stack", "pagetables", "sock", "slab_unreclaimable"); err != nil {
		return nil, err
	}
	if memAt == "" {
		memAt = m.Point
	}
	if err := c.readOOM(r, m, memAt); err != nil {
		return nil, err
	}
	return c, nil
}

func readV1(r Root, ms []Mount, procs map[string]string) (*Cgroup, error) {
	c := &Cgroup{Version: V1, SubtreeWide: true, OOMAbsent: "cgroup v1 counts a cgroup's own OOM kills only, never its jobs'"}
	mem, err := v1Dirs(ms, procs, "memory")
	if err != nil {
		return nil, err
	}
	if mem == nil {
		return nil, fmt.Errorf("%w: no cgroup v1 memory controller is mounted", ErrCgroup)
	}
	h, _, err := r.lookup(mem[0] + "/memory.use_hierarchy")
	if err != nil {
		return nil, err
	}
	if h != "1" {
		return nil, fmt.Errorf("%w: %s/memory.use_hierarchy is %q, not 1, so the host's memory limit would not hold its jobs", ErrCgroup, mem[0], h)
	}
	if c.Memory, _, _, err = smallest(r, mem, "memory.limit_in_bytes", parseV1Memory); err != nil {
		return nil, err
	}
	_, c.Swap, err = r.lookup(mem[0] + "/memory.memsw.limit_in_bytes")
	if err != nil {
		return nil, err
	}
	if c.OwnMemory, err = ownMemory(r, mem[0], "rss", "mapped_file"); err != nil {
		return nil, err
	}
	if cerr := c.readV1CPU(r, ms, procs); cerr != nil {
		return nil, cerr
	}
	pids, err := v1Dirs(ms, procs, "pids")
	if err != nil {
		return nil, err
	}
	if c.PIDs, _, c.PIDsController, err = smallest(r, pids, "pids.max", parseMax); err != nil {
		return nil, err
	}
	return c, nil
}

// readV1CPU reads the CFS quota and the cpuset, each controller optional.
func (c *Cgroup) readV1CPU(r Root, ms []Mount, procs map[string]string) error {
	cpu, err := v1Dirs(ms, procs, "cpu")
	if err != nil {
		return err
	}
	var quota, period int64
	var at string
	if quota, at, c.Quota, err = smallest(r, cpu, "cpu.cfs_quota_us", parseQuotaUS); err != nil {
		return err
	}
	if at != "" {
		ps, perr := r.read(at + "/cpu.cfs_period_us")
		if perr != nil {
			return perr
		}
		if period, perr = strconv.ParseInt(ps, 10, 64); perr != nil || period <= 0 {
			return fmt.Errorf("%w: %s/cpu.cfs_period_us: %q", ErrCgroup, at, ps)
		}
		c.MilliCPUs = quota * 1000 / period
	}
	set, err := v1Dirs(ms, procs, "cpuset")
	if err != nil {
		return err
	}
	return c.readCPUSet(r, set, "cpuset.effective_cpus", "cpuset.cpus")
}

// v1Dirs answers the walk of controller c's cgroup, nil when the
// controller is not mounted.
func v1Dirs(ms []Mount, procs map[string]string, c string) ([]string, error) {
	m, ok := v1Mount(ms, c)
	if !ok {
		return nil, nil
	}
	p, ok := procs[c]
	if !ok {
		return nil, fmt.Errorf("%w: /proc/self/cgroup has no %s line", ErrCgroup, c)
	}
	return walk(m, p)
}

// readCPUSet reads the first of names along dirs, else the machine's
// online CPUs, and makes it the CPU figure where no quota is set.
func (c *Cgroup) readCPUSet(r Root, dirs []string, names ...string) error {
	s, ok, err := first(r, dirs, names...)
	if err != nil {
		return err
	}
	if !ok {
		if s, err = r.read("/sys/devices/system/cpu/online"); err != nil {
			return err
		}
	}
	cpus, err := ParseCPUList(s)
	if err != nil {
		return fmt.Errorf("%w: cpuset %q: %w", ErrCgroup, s, err)
	}
	c.CPUSet, c.CPUs = s, len(cpus)
	if c.MilliCPUs == 0 {
		c.MilliCPUs = int64(len(cpus)) * 1000
	}
	return nil
}

// readOOM reads memory.events at dir, the cgroup holding the memory limit,
// where its count is hierarchical: cgroup v2 with memory.events.local
// beside it (Linux 5.2 and later) and without the memory_localevents mount
// option, which keeps memory.events local (Documentation/admin-guide/cgroup-v2.rst).
func (c *Cgroup) readOOM(r Root, m *Mount, dir string) error {
	switch {
	case m.HasSuper("memory_localevents"):
		c.OOMAbsent = "the cgroup2 mount carries memory_localevents, so memory.events counts the host cgroup's own OOM kills only"
		return nil
	case !r.exists(dir + "/memory.events.local"):
		c.OOMAbsent = "the kernel has no memory.events.local, so memory.events counts the host cgroup's own OOM kills only"
		return nil
	}
	s, err := r.read(dir + "/memory.events")
	if err != nil {
		return err
	}
	v, ok, err := statKey(s, "oom_kill")
	if err != nil {
		return fmt.Errorf("%w: %s/memory.events: %w", ErrCgroup, dir, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s/memory.events has no oom_kill count", ErrCgroup, dir)
	}
	c.OOMKills, c.OOMCounted = uint64(v), true //nolint:gosec // statKey parses unsigned counts.
	return nil
}

// ownMemory sums keys of memory.stat at dir, the cgroup holding the host's
// own processes: its own figures, never the hierarchical total_* ones.
func ownMemory(r Root, dir string, keys ...string) (int64, error) {
	s, err := r.read(dir + "/memory.stat")
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, k := range keys {
		v, ok, err := statKey(s, k)
		if err != nil {
			return 0, fmt.Errorf("%w: %s/memory.stat: %s: %w", ErrCgroup, dir, k, err)
		}
		if !ok {
			return 0, fmt.Errorf("%w: %s/memory.stat has no %s", ErrCgroup, dir, k)
		}
		sum += v
	}
	return sum, nil
}

// statKey answers the value of key in a "key value" per line file.
func statKey(s, key string) (v int64, ok bool, err error) {
	for line := range strings.SplitSeq(s, "\n") {
		k, val, found := strings.Cut(line, " ")
		if found && k == key {
			n, perr := strconv.ParseUint(strings.TrimSpace(val), 10, 63)
			return int64(n), perr == nil, perr
		}
	}
	return 0, false, nil
}

// parseMax parses a cgroup v2 limit, "max" for none.
func parseMax(s string) (v int64, set bool, err error) {
	if s == "max" {
		return 0, false, nil
	}
	v, err = strconv.ParseInt(s, 10, 64)
	return v, err == nil, err
}

// parseCPUMax parses cpu.max, "quota period", as millicpus.
func parseCPUMax(s string) (milli int64, set bool, err error) {
	q, p, ok := strings.Cut(s, " ")
	if !ok {
		return 0, false, fmt.Errorf("not quota and period: %q", s)
	}
	period, err := strconv.ParseInt(p, 10, 64)
	if err != nil || period <= 0 {
		return 0, false, fmt.Errorf("period %q", p)
	}
	quota, set, err := parseMax(q)
	if err != nil || !set {
		return 0, false, err
	}
	return quota * 1000 / period, true, nil
}

// parseV1Memory parses memory.limit_in_bytes; at or above unlimitedV1 it
// is no limit.
func parseV1Memory(s string) (v int64, set bool, err error) {
	v, err = strconv.ParseInt(s, 10, 64)
	return v, err == nil && v < unlimitedV1, err
}

// parseQuotaUS parses cpu.cfs_quota_us, -1 for none, kept in microseconds
// for readV1CPU to divide by its own period.
func parseQuotaUS(s string) (v int64, set bool, err error) {
	v, err = strconv.ParseInt(s, 10, 64)
	return v, err == nil && v > 0, err
}

// ParseCPUList parses the kernel's CPU list format ("0-3,8,10-11").
func ParseCPUList(s string) ([]int, error) {
	var out []int
	for part := range strings.SplitSeq(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil, fmt.Errorf("cpu %q", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("cpu range %q", part)
			}
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no CPU")
	}
	return out, nil
}
