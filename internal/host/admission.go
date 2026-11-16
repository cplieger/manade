package host

import (
	"cmp"
	"slices"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// The host reserve: what the host's own processes may hold of its memory
// limit.
const (
	// reserveBase is about twice the 124 MiB the host's own processes
	// peaked at during a pull on the floor machine.
	reserveBase = 256 << 20
	// reserveShim is one container's containerd shim: each runner's held
	// 9 MiB there.
	reserveShim = 16 << 20
	// reserveProbe is the probe's two containers, each held to
	// jobspec.ProbeMemory, with their shims.
	reserveProbe     = 2 * (jobspec.ProbeMemory + reserveShim)
	reservePerRunner = reserveShim
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

// reserveRunners is how many runners the host's own limits hold at once
// serving profiles: its memory limit over the smallest profile memory and
// its CPUs over the smallest profile cpus, each counted only where set. The
// pool's capacity for the host stays inside those limits, so the reserve
// covers every runner the pool can place. A pinning host serves only
// whole-CPU profiles, so the division counts whole CPUs.
func reserveRunners(cg *platform.Cgroup, profiles []*config.Profile) int {
	if len(profiles) == 0 {
		return 0
	}
	minMem := slices.MinFunc(profiles, func(a, b *config.Profile) int { return cmp.Compare(a.Memory, b.Memory) }).Memory
	minCPU := slices.MinFunc(profiles, func(a, b *config.Profile) int { return cmp.Compare(a.MilliCPUs, b.MilliCPUs) }).MilliCPUs
	var counts []int
	if minMem > 0 && cg.Memory > 0 {
		counts = append(counts, int(cg.Memory/minMem))
	}
	if minCPU > 0 && cg.MilliCPUs > 0 {
		counts = append(counts, int(cg.MilliCPUs/minCPU))
	}
	if len(counts) == 0 {
		return 0
	}
	return slices.Min(counts)
}

// memoryStatus is what jobs may use of the host cgroup cg's memory limit (0
// for none, which admits nothing) serving profiles.
func memoryStatus(cg *platform.Cgroup, profiles []*config.Profile) hostapi.MemoryStatus {
	n := reserveRunners(cg, profiles)
	reserve := int64(reserveBase + reserveProbe + n*reservePerRunner)
	return hostapi.MemoryStatus{Limit: cg.Memory, Admissible: max(0, cg.Memory-reserve), Reserve: reserve, ReserveRunners: n, Own: cg.OwnMemory}
}
