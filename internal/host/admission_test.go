package host

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/platform"
)

const mib = 1 << 20

func TestMemoryStatus(t *testing.T) {
	small := &config.Profile{Name: "small", MilliCPUs: 1000, Memory: 512 * mib}
	big := &config.Profile{Name: "big", MilliCPUs: 4000, Memory: 8 << 30}
	for _, tc := range []struct {
		name     string
		cg       platform.Cgroup
		profiles []*config.Profile
		runners  int
		reserve  int64
	}{
		// 2 CPUs admit two 1-CPU runners where 3.5 GiB of 512 MiB would
		// admit seven: 256 + 96 + 2 × 16.
		{"a 2-CPU host with a 512 MiB profile", platform.Cgroup{MilliCPUs: 2000, Memory: 3584 * mib}, []*config.Profile{small, big}, 2, 384 * mib},
		// 8 CPUs admit eight, 25 GiB of 512 MiB would admit 50: 256 + 96 + 8 × 16.
		{"an 8-CPU quota host", platform.Cgroup{MilliCPUs: 8000, Memory: 25 << 30}, []*config.Profile{small}, 8, 480 * mib},
		{"memory below cpus", platform.Cgroup{MilliCPUs: 8000, Memory: 2 << 30}, []*config.Profile{small}, 4, 416 * mib},
		{"cpus that do not divide round the count down", platform.Cgroup{MilliCPUs: 2500, Memory: 25 << 30}, []*config.Profile{small}, 2, 384 * mib},
		{"a quota host divides fractional cpus", platform.Cgroup{MilliCPUs: 2500, Memory: 25 << 30}, []*config.Profile{{MilliCPUs: 500, Memory: 512 * mib}}, 5, 432 * mib},
		{"no profiles reserves the base and the probe", platform.Cgroup{MilliCPUs: 2000, Memory: 2 << 30}, nil, 0, 352 * mib},
		{"no memory limit counts the cpus and admits nothing", platform.Cgroup{MilliCPUs: 2000}, []*config.Profile{small}, 2, 384 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cg := tc.cg
			cg.OwnMemory = 99 * mib
			st := memoryStatus(&cg, tc.profiles)
			want := max(0, cg.Memory-tc.reserve)
			if st.ReserveRunners != tc.runners || st.Reserve != tc.reserve || st.Admissible != want || st.Own != 99*mib || st.Limit != cg.Memory {
				t.Errorf("status = %+v, want %d runners, reserve %d MiB, admissible %d MiB", st, tc.runners, tc.reserve/mib, want/mib)
			}
		})
	}
}

func TestNoMemoryLimitWarnsWithTheReserve(t *testing.T) {
	var buf bytes.Buffer
	s := &supervisor{log: slog.New(slog.NewTextHandler(&buf, nil))}
	sv := &served{Profiles: []*config.Profile{{Name: "small", MilliCPUs: 1000, Memory: 512 * mib}}}
	s.warnNoMemoryLimit(sv, &platform.Cgroup{MilliCPUs: 2000, Memory: 2 << 30})
	if buf.Len() != 0 {
		t.Fatalf("a host with a limit warned: %s", buf.String())
	}
	s.warnNoMemoryLimit(sv, &platform.Cgroup{MilliCPUs: 2000})
	// The 384 MiB reserve of two runners: 256 + 96 + 2 × 16.
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "reserve_bytes=402653184") {
		t.Errorf("log = %q, want one WARN naming the reserve in bytes", out)
	}
}
