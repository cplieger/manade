package pool

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

// errCapacity is a host's configured capacity above what its status says
// its kernel and volume give jobs.
var errCapacity = errors.New("pool: the host cannot give its configured capacity")

// admit refuses host h's capacity where it is above the CPU limit, the
// admissible memory (the cgroup limit less the host reserve) or the data
// volume's size the host reports, or is a fractional cpus on a host that
// pins CPUs. The error names each refused figure and what bounds it.
func admit(h *config.Host, st *hostapi.Status) error {
	c := &h.Capacity
	at := "hosts." + h.Name + ".capacity"
	var why []error
	refuse := func(format string, args ...any) {
		why = append(why, fmt.Errorf("%w: %s", errCapacity, fmt.Sprintf(format, args...)))
	}
	if c.MilliCPUs > st.Limits.MilliCPUs {
		refuse("%s.cpus %s is more than the host's CPU limit %s", at, cpus(c.MilliCPUs), cpus(st.Limits.MilliCPUs))
	}
	if st.Limits.CPUMode == hostapi.CPUPinning && c.MilliCPUs%1000 != 0 {
		refuse("%s.cpus %s is not whole CPUs, which a host pinning CPUs (its kernel has no CFS quota) needs", at, cpus(c.MilliCPUs))
	}
	switch m := &st.Memory; {
	case m.Limit == 0:
		refuse("%s.memory: the host's container has no memory limit, which admits no job memory; give it %s.memory plus its reserve of %d bytes",
			at, at, m.Reserve)
	case c.Memory > m.Admissible:
		refuse("%s.memory %d bytes is more than the %d bytes jobs may use: the host's memory limit of %d bytes less its reserve of %d bytes",
			at, c.Memory, m.Admissible, m.Limit, m.Reserve)
	}
	if c.Disk > 0 && uint64(c.Disk) > st.Data.Size {
		refuse("%s.disk %d bytes is more than the host's data volume holds (%d bytes)", at, c.Disk, st.Data.Size)
	}
	return errors.Join(why...)
}

func cpus(milli int64) string { return strconv.FormatFloat(float64(milli)/1000, 'f', -1, 64) }

// readiness answers, by profile index of cfg, whether host's status lists
// the profile ready. A profile the host refused, for a limit its kernel or
// volume cannot enforce, is absent from its status and so never ready.
func readiness(cfg *config.Config, host string, st *hostapi.Status) []bool {
	ready := make([]bool, len(cfg.Profiles))
	for i := range cfg.Profiles {
		pr := &cfg.Profiles[i]
		ready[i] = slices.Contains(pr.Hosts, host) && slices.ContainsFunc(st.Profiles, func(ps hostapi.ProfileStatus) bool {
			return ps.Name == pr.Name && ps.Ready
		})
	}
	return ready
}
