package host

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// The host reserve: what the host's own processes may hold of its memory
// limit. The base is about twice the 124 MiB the host's own processes
// peaked at during a pull on the floor machine; each probe container is
// held to jobspec.ProbeMemory with 16 MiB for its shim; each runner's shim
// held 9 MiB.
const (
	reserveBase      = 256 << 20
	reserveProbe     = 2 * (jobspec.ProbeMemory + reservePerRunner)
	reservePerRunner = 16 << 20
)

// servedProfiles answers the profiles of cfg that list host, in config order.
func servedProfiles(cfg *config.Config, host string) []*config.Profile {
	var out []*config.Profile
	for i := range cfg.Profiles {
		if slices.Contains(cfg.Profiles[i].Hosts, host) {
			out = append(out, &cfg.Profiles[i])
		}
	}
	return out
}

// reserveRunners is how many runners host h can run at once serving
// profiles: the smallest of its max_runners, its memory over the smallest
// profile memory and its cpus over the smallest profile cpus. A pinning
// host serves only whole-CPU profiles, so the division counts whole CPUs.
func reserveRunners(h *config.Host, profiles []*config.Profile) int {
	if len(profiles) == 0 {
		return 0
	}
	minMem := slices.MinFunc(profiles, func(a, b *config.Profile) int { return cmp.Compare(a.Memory, b.Memory) }).Memory
	minCPU := slices.MinFunc(profiles, func(a, b *config.Profile) int { return cmp.Compare(a.MilliCPUs, b.MilliCPUs) }).MilliCPUs
	var counts []int
	if h.Capacity.MaxRunners > 0 {
		counts = append(counts, h.Capacity.MaxRunners)
	}
	if minMem > 0 {
		counts = append(counts, int(h.Capacity.Memory/minMem))
	}
	if minCPU > 0 {
		counts = append(counts, int(h.Capacity.MilliCPUs/minCPU))
	}
	if len(counts) == 0 {
		return 0
	}
	return slices.Min(counts)
}

// memoryStatus is what jobs may use of limit, the host cgroup's memory
// limit (0 for none, which admits nothing), for host h serving profiles.
func memoryStatus(h *config.Host, profiles []*config.Profile, limit, own int64) hostapi.MemoryStatus {
	n := reserveRunners(h, profiles)
	reserve := int64(reserveBase + reserveProbe + n*reservePerRunner)
	return hostapi.MemoryStatus{Admissible: max(0, limit-reserve), Reserve: reserve, ReserveRunners: n, Own: own}
}

// checkDisk refuses a capacity.disk above the data volume's size.
func checkDisk(h *config.Host, size uint64) error {
	if h.Capacity.Disk > 0 && uint64(h.Capacity.Disk) > size {
		return fmt.Errorf("hosts.%s.capacity.disk %d bytes is more than the data volume %s holds (%d bytes)", h.Name, h.Capacity.Disk, dataRoot, size)
	}
	return nil
}
