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
		capacity config.Capacity
		profiles []*config.Profile
		limit    int64
		runners  int
		reserve  int64
	}{
		// The CPU budget admits two 1-CPU runners where 3 GiB of 512 MiB
		// would admit six: 256 + 96 + 2 × 16.
		{"a 2-CPU host with a 512 MiB profile", config.Capacity{MilliCPUs: 2000, Memory: 3 << 30}, []*config.Profile{small, big}, 3584 * mib, 2, 384 * mib},
		// 8 CPUs admit eight, 24 GiB of 512 MiB would admit 48: 256 + 96 + 8 × 16.
		{"an 8-CPU quota host", config.Capacity{MilliCPUs: 8000, Memory: 24 << 30}, []*config.Profile{small}, 25 << 30, 8, 480 * mib},
		{"max_runners below both", config.Capacity{MilliCPUs: 8000, Memory: 24 << 30, MaxRunners: 3}, []*config.Profile{small}, 25 << 30, 3, 400 * mib},
		{"memory below cpus", config.Capacity{MilliCPUs: 8000, Memory: 1 << 30}, []*config.Profile{small}, 2 << 30, 2, 384 * mib},
		{"cpus that do not divide round the count down", config.Capacity{MilliCPUs: 2500, Memory: 24 << 30}, []*config.Profile{small}, 25 << 30, 2, 384 * mib},
		{"a quota host divides fractional cpus", config.Capacity{MilliCPUs: 2500, Memory: 24 << 30}, []*config.Profile{{MilliCPUs: 500, Memory: 512 * mib}}, 25 << 30, 5, 432 * mib},
		{"no profiles reserves the base and the probe", config.Capacity{MilliCPUs: 2000, Memory: 1 << 30}, nil, 2 << 30, 0, 352 * mib},
		{"no memory limit admits nothing", config.Capacity{MilliCPUs: 2000, Memory: 1 << 30}, []*config.Profile{small}, 0, 2, 384 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &config.Host{Name: "h", Capacity: tc.capacity}
			st := memoryStatus(h, tc.profiles, tc.limit, 99*mib)
			want := max(0, tc.limit-tc.reserve)
			if st.ReserveRunners != tc.runners || st.Reserve != tc.reserve || st.Admissible != want || st.Own != 99*mib || st.Limit != tc.limit {
				t.Errorf("status = %+v, want %d runners, reserve %d MiB, admissible %d MiB", st, tc.runners, tc.reserve/mib, want/mib)
			}
		})
	}
}

func TestNoMemoryLimitWarnsWithTheFix(t *testing.T) {
	var buf bytes.Buffer
	s := &supervisor{log: slog.New(slog.NewTextHandler(&buf, nil))}
	h := &config.Host{Name: "voyager", Capacity: config.Capacity{MilliCPUs: 2000, Memory: 3 << 30}}
	sv := &served{Profiles: []*config.Profile{{Name: "small", MilliCPUs: 1000, Memory: 512 * mib}}}
	s.warnNoMemoryLimit(h, sv, &platform.Cgroup{Memory: 2 << 30})
	if buf.Len() != 0 {
		t.Fatalf("a host with a limit warned: %s", buf.String())
	}
	s.warnNoMemoryLimit(h, sv, &platform.Cgroup{})
	// 3 GiB plus the 384 MiB reserve of two runners: 256 + 96 + 2 × 16.
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "limit_bytes=3623878656") {
		t.Errorf("log = %q, want one WARN naming the limit in bytes", out)
	}
}

func TestCheckDisk(t *testing.T) {
	h := &config.Host{Name: "nas", Capacity: config.Capacity{Disk: 100 << 30}}
	t.Run("a budget inside the volume", func(t *testing.T) {
		if err := checkDisk(h, 150<<30); err != nil {
			t.Error(err)
		}
	})
	t.Run("a budget above the volume names both", func(t *testing.T) {
		err := checkDisk(h, 50<<30)
		if err == nil || !strings.Contains(err.Error(), "hosts.nas.capacity.disk") || !strings.Contains(err.Error(), "53687091200") {
			t.Errorf("checkDisk = %v", err)
		}
	})
	t.Run("no disk budget", func(t *testing.T) {
		if err := checkDisk(&config.Host{Name: "nas"}, 1); err != nil {
			t.Error(err)
		}
	})
}
