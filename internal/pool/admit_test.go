package pool

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

// status is a host whose cgroup gives 8 CPUs by quota and 8 GiB, of which
// a 384 MiB reserve leaves 7808 MiB to jobs, on a 100 GiB data volume.
func status() *hostapi.Status {
	return &hostapi.Status{
		Limits: hostapi.HostLimits{CPUMode: hostapi.CPUQuota, MilliCPUs: 8000, Memory: 8 << 30},
		Memory: hostapi.MemoryStatus{Limit: 8 << 30, Admissible: 8<<30 - 384<<20, Reserve: 384 << 20},
		Data:   hostapi.DataStatus{Size: 100 << 30},
	}
}

func TestAdmit(t *testing.T) {
	t.Parallel()
	host := func(c config.Capacity) *config.Host { return &config.Host{Name: "nas", Capacity: c} }
	fits := config.Capacity{MilliCPUs: 8000, Memory: 8<<30 - 384<<20, Disk: 100 << 30}
	t.Run("withinEveryLimit", func(t *testing.T) {
		t.Parallel()
		if err := admit(host(fits), status()); err != nil {
			t.Errorf("admit = %v, want nil", err)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(c *config.Capacity, st *hostapi.Status)
		want []string
	}{
		{
			"cpusAboveTheLimit", func(c *config.Capacity, _ *hostapi.Status) { c.MilliCPUs = 8500 },
			[]string{"hosts.nas.capacity.cpus 8.5 is more than the host's CPU limit 8"},
		},
		{
			"memoryEqualToTheCgroupLimit", func(c *config.Capacity, _ *hostapi.Status) { c.Memory = 8 << 30 },
			[]string{"hosts.nas.capacity.memory 8589934592 bytes is more than the 8187281408 bytes jobs may use", "memory limit of 8589934592 bytes less its reserve of 402653184 bytes"},
		},
		{
			"noMemoryLimit", func(_ *config.Capacity, st *hostapi.Status) { st.Memory = hostapi.MemoryStatus{Reserve: 384 << 20} },
			[]string{"has no memory limit, which admits no job memory", "reserve of 402653184 bytes"},
		},
		{
			"diskAboveTheVolume", func(c *config.Capacity, _ *hostapi.Status) { c.Disk = 101 << 30 },
			[]string{"hosts.nas.capacity.disk 108447924224 bytes is more than the host's data volume holds (107374182400 bytes)"},
		},
		{"fractionalOnAPinningHost", func(c *config.Capacity, st *hostapi.Status) {
			c.MilliCPUs = 1500
			st.Limits.CPUMode = hostapi.CPUPinning
		}, []string{"hosts.nas.capacity.cpus 1.5 is not whole CPUs"}},
		{
			"everyRefusalAtOnce", func(c *config.Capacity, _ *hostapi.Status) { c.MilliCPUs, c.Memory = 9000, 9<<30 },
			[]string{"capacity.cpus 9", "capacity.memory 9663676416 bytes"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, st := fits, status()
			tc.edit(&c, st)
			err := admit(host(c), st)
			if !errors.Is(err, errCapacity) {
				t.Fatalf("admit = %v, want errCapacity", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("admit = %q, want it to contain %q", err, w)
				}
			}
		})
	}
	t.Run("wholeCPUsOnAPinningHost", func(t *testing.T) {
		t.Parallel()
		st := status()
		st.Limits.CPUMode = hostapi.CPUPinning
		if err := admit(host(fits), st); err != nil {
			t.Errorf("admit = %v, want nil", err)
		}
	})
}

func TestReadiness(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "nas", cpus: 2, mem: 3}, {name: "default", cpus: 8, mem: 24}},
		[]profileSpec{{name: "ready", hosts: []string{"nas"}}, {name: "pulling", hosts: []string{"nas"}}, {name: "refused", hosts: []string{"nas"}}, {name: "elsewhere", hosts: []string{"default"}}})
	st := &hostapi.Status{Profiles: []hostapi.ProfileStatus{
		{Name: "ready", Ready: true}, {Name: "pulling", Reason: hostapi.ReasonPulling}, {Name: "elsewhere", Ready: true},
	}}
	if got := readiness(cfg, "nas", st); !slices.Equal(got, []bool{true, false, false, false}) {
		t.Errorf("readiness = %v, want ready alone: pulling is not ready, refused is absent and elsewhere does not list nas", got)
	}
}
