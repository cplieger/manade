package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RunnerCgroups are the cgroups the host makes each runner's container
// under, <dir>/<runner name> in every hierarchy, which the daemon leaves
// when the container exits. runc puts an absolute cgroup parent at the
// same path under each hierarchy's mount, so that is where they are read.
type RunnerCgroups struct {
	root Root
	// memory and cpu hold the runners' cgroups in the hierarchies read for
	// the memory and CPU figures; all is every hierarchy's.
	memory, cpu string
	all         []string
	v2          bool
}

// RunnerEnd is what an ended runner's cgroup kept.
type RunnerEnd struct {
	// CPU is the CPU time the runner's processes took.
	CPU time.Duration
	// Watermark is the kernel's high mark of the cgroup's memory, page
	// cache included; 0 where the kernel keeps none (cgroup v2 before
	// Linux 5.19).
	Watermark int64
}

// OpenRunnerCgroups answers the runner cgroups under dir, ms being the
// host's mounts under r.
func OpenRunnerCgroups(r Root, ms []Mount, dir string) (*RunnerCgroups, error) {
	c := &RunnerCgroups{root: r, v2: isV2(r)}
	for i := range ms {
		m := &ms[i]
		if (m.FSType != "cgroup" && m.FSType != "cgroup2") || (m.Point != cgroupRoot && !strings.HasPrefix(m.Point, cgroupRoot+"/")) {
			continue
		}
		if d := path.Join(m.Point, dir); !slices.Contains(c.all, d) {
			c.all = append(c.all, d)
		}
	}
	if c.v2 {
		m, err := v2Mount(ms)
		if err != nil {
			return nil, err
		}
		c.memory = path.Join(m.Point, dir)
		c.cpu = c.memory
		return c, nil
	}
	mem, ok := v1Mount(ms, "memory")
	if !ok {
		return nil, fmt.Errorf("%w: no cgroup v1 memory controller is mounted", errCgroup)
	}
	cpu, ok := v1Mount(ms, "cpuacct")
	if !ok {
		return nil, fmt.Errorf("%w: no cgroup v1 cpuacct controller is mounted", errCgroup)
	}
	c.memory, c.cpu = path.Join(mem.Point, dir), path.Join(cpu.Point, dir)
	return c, nil
}

// WorkingSet answers runner name's memory in use less its inactive page
// cache, the figure docker stats shows; found is false with no cgroup.
func (c *RunnerCgroups) WorkingSet(name string) (ws int64, found bool, err error) {
	d := c.memory + "/" + name
	usage, inactive := "memory.usage_in_bytes", "total_inactive_file"
	if c.v2 {
		usage, inactive = "memory.current", "inactive_file"
	}
	s, found, err := c.root.lookup(d + "/" + usage)
	if err != nil || !found {
		return 0, false, err
	}
	used, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s/%s: %w", errCgroup, d, usage, err)
	}
	stat, err := c.root.read(d + "/memory.stat")
	if err != nil {
		return 0, false, err
	}
	cache, ok, err := statKey(stat, inactive)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s/memory.stat: %s: %w", errCgroup, d, inactive, err)
	}
	if !ok {
		return 0, false, fmt.Errorf("%w: %s/memory.stat has no %s", errCgroup, d, inactive)
	}
	return max(0, used-cache), true, nil
}

// Ended answers runner name's CPU time and memory watermark; found is
// false with no cgroup.
func (c *RunnerCgroups) Ended(name string) (e RunnerEnd, found bool, err error) {
	if c.v2 {
		return c.endedV2(name)
	}
	d := c.cpu + "/" + name
	s, found, err := c.root.lookup(d + "/cpuacct.usage")
	if err != nil || !found {
		return RunnerEnd{}, false, err
	}
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return RunnerEnd{}, false, fmt.Errorf("%w: %s/cpuacct.usage: %w", errCgroup, d, err)
	}
	e.CPU = time.Duration(ns)
	e.Watermark, err = c.watermark(name, "memory.max_usage_in_bytes")
	return e, true, err
}

func (c *RunnerCgroups) endedV2(name string) (e RunnerEnd, found bool, err error) {
	d := c.cpu + "/" + name
	s, found, err := c.root.lookup(d + "/cpu.stat")
	if err != nil || !found {
		return RunnerEnd{}, false, err
	}
	us, ok, err := statKey(s, "usage_usec")
	if err != nil {
		return RunnerEnd{}, false, fmt.Errorf("%w: %s/cpu.stat: usage_usec: %w", errCgroup, d, err)
	}
	if !ok {
		return RunnerEnd{}, false, fmt.Errorf("%w: %s/cpu.stat has no usage_usec", errCgroup, d)
	}
	e.CPU = time.Duration(us) * time.Microsecond
	e.Watermark, err = c.watermark(name, "memory.peak")
	return e, true, err
}

// watermark reads file of runner name's memory cgroup, 0 when absent.
func (c *RunnerCgroups) watermark(name, file string) (int64, error) {
	p := c.memory + "/" + name + "/" + file
	s, ok, err := c.root.lookup(p)
	if err != nil || !ok {
		return 0, err
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", errCgroup, p, err)
	}
	return v, nil
}

// Remove removes runner name's cgroup from every hierarchy holding it. The
// kernel refuses one that still holds a process (EBUSY).
func (c *RunnerCgroups) Remove(name string) error {
	var errs []error
	for _, d := range c.all {
		if err := syscall.Rmdir(c.root.Path(d + "/" + name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing %s/%s: %w", d, name, err))
		}
	}
	return errors.Join(errs...)
}

// Names answers every runner any hierarchy holds a cgroup of.
func (c *RunnerCgroups) Names() ([]string, error) {
	var names []string
	for _, d := range c.all {
		es, err := os.ReadDir(c.root.Path(d))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if e.IsDir() && !slices.Contains(names, e.Name()) {
				names = append(names, e.Name())
			}
		}
	}
	slices.Sort(names)
	return names, nil
}
