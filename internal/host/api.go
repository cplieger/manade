package host

import (
	"context"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/hostapi"
)

// backend serves the host API from the supervisor.
type backend struct {
	s *supervisor
}

var _ hostapi.Backend = (*backend)(nil)

func (b *backend) Status(ctx context.Context) hostapi.Status { return b.s.status(ctx) }

func (b *backend) CreateRunner(ctx context.Context, req *hostapi.RunnerRequest) (hostapi.Runner, error) {
	return b.s.runners.accept(ctx, req)
}

func (b *backend) Runners(ctx context.Context) ([]hostapi.Runner, error) {
	return b.s.runners.list(ctx)
}

func (b *backend) DeleteRunner(ctx context.Context, name string, diag bool) ([]byte, error) {
	return b.s.runners.remove(ctx, name, diag)
}

func (b *backend) Logs(ctx context.Context, name string, tail int) ([]byte, error) {
	return b.s.runners.logs(ctx, name, tail)
}

func (b *backend) Diag(ctx context.Context, name string) ([]byte, error) {
	return b.s.runners.diag(ctx, name)
}

func (b *backend) Subscribe(ctx context.Context) <-chan hostapi.Runner {
	return b.s.runners.subscribe(ctx)
}

func (b *backend) Probe(ctx context.Context) (hostapi.ProbeReport, error) {
	return b.s.prober.Run(ctx)
}

func (b *backend) ProbeDeadline() time.Duration { return b.s.prober.Deadline() }

// The daemon calls whose latency a runner start reports.
var latencyCalls = []string{"create", "start"}

// latencies holds one histogram per call in latencyCalls.
type latencies struct {
	h  map[string]*hostapi.CallLatency
	mu sync.Mutex
}

func newLatencies() *latencies {
	l := &latencies{h: map[string]*hostapi.CallLatency{}}
	for _, c := range latencyCalls {
		l.h[c] = &hostapi.CallLatency{Call: c, Buckets: make([]uint64, len(hostapi.LatencyBuckets))}
	}
	return l
}

// observe is the daemon client's observer: it counts each create and start
// call's latency.
func (l *latencies) observe(op string, took time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.h[op]
	if !ok {
		return
	}
	sec := took.Seconds()
	for i, b := range hostapi.LatencyBuckets {
		if sec <= b {
			h.Buckets[i]++
		}
	}
	h.Count++
	h.Sum += sec
}

func (l *latencies) snapshot() []hostapi.CallLatency {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]hostapi.CallLatency, 0, len(latencyCalls))
	for _, c := range latencyCalls {
		h := *l.h[c]
		h.Buckets = append([]uint64(nil), h.Buckets...)
		out = append(out, h)
	}
	return out
}

// status is the host's account of itself, its cgroup and data volume read
// afresh.
func (s *supervisor) status(ctx context.Context) hostapi.Status {
	sv := s.served.Load()
	h := s.host()
	n := s.net
	st := hostapi.Status{
		Version: s.opts.Version, PolicyHash: egress.PolicyHash(h), ProfileHash: egress.ProfileHash(sv.Config.Profiles, h.Name),
		Daemon:        hostapi.DaemonStatus{Version: s.info.ServerVersion, StorageDriver: s.info.Driver},
		Rules:         hostapi.RulesStatus{State: hostapi.RulesLoaded, Backend: backendName(n.Kernel.Backend), Restored: s.restored.Load(), RawOff: n.Kernel.RawOff},
		DNS:           hostapi.DNSStatus{Configured: h.DNS, Original: n.Original.Addrs, OriginalUnknown: n.Original.Unknown},
		Profiles:      s.runners.profileStatus(),
		StartLatency:  s.lat.snapshot(),
		Outward:       s.outward,
		IPv6:          n.iPv6Status(),
		GitHub:        n.gitHubStatus(),
		ProbeDeadline: s.prober.Deadline(),
	}
	if msg := s.configErr.Load(); msg != nil {
		st.Rules.State, st.Rules.ConfigError = hostapi.RulesKept, *msg
	}
	if u, err := platform.DataUsage(dataRoot); err == nil {
		st.Data = hostapi.DataStatus{Size: u.Size, Free: u.Free}
	} else {
		s.log.Warn("Reading the data volume's usage failed", "error", err)
	}
	cg, err := platform.Read(platform.System)
	if err != nil {
		s.log.Warn("Reading the host's cgroup failed", "error", err)
		return st
	}
	st.Limits = hostapi.HostLimits{
		CPUMode: hostapi.CPUQuota, CPUSet: cg.CPUSet, MilliCPUs: cg.MilliCPUs, Memory: cg.Memory, PIDs: cg.PIDs, Disk: s.plat.Disk,
	}
	if !cg.Quota {
		st.Limits.CPUMode = hostapi.CPUPinning
	}
	st.Memory = memoryStatus(h, sv.Profiles, cg.Memory, cg.OwnMemory)
	st.OOM = s.oomStatus(ctx, cg)
	return st
}

// oomStatus is the host's OOM kill total across its restarts, or why the
// kernel gives none.
func (s *supervisor) oomStatus(ctx context.Context, cg *platform.Cgroup) hostapi.OOMStatus {
	st := hostapi.OOMStatus{SubtreeWide: cg.SubtreeWide, Absent: cg.OOMAbsent}
	if !cg.OOMCounted {
		return st
	}
	total, err := s.oom.Observe(ctx, cg.OOMKills)
	if err != nil {
		s.log.Warn("Saving the OOM kill total failed; the next status saves it", "error", err)
	}
	st.Kills = &total
	return st
}

func backendName(b egress.Backend) string {
	if b == egress.NFTables {
		return "nftables"
	}
	return "iptables"
}
