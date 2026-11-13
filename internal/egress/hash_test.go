package egress

import (
	"net/netip"
	"testing"

	"github.com/cplieger/manade/internal/config"
)

func hashHost() *config.Host {
	return &config.Host{
		Name:   "nas",
		DNS:    []netip.Addr{addr("10.0.0.53"), addr("10.0.0.54")},
		Egress: config.Egress{Internet: true, Allow: []config.Allow{{To: pfx("10.0.0.50/32"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{5432, 5432})}}},
	}
}

func TestPolicyHash(t *testing.T) {
	t.Parallel()
	base := PolicyHash(hashHost())
	t.Run("stable", func(t *testing.T) {
		t.Parallel()
		if got := PolicyHash(hashHost()); got != base {
			t.Errorf("two equal sections hash %s and %s", base, got)
		}
		if len(base) != 64 {
			t.Errorf("hash %q is not a hex SHA-256", base)
		}
	})
	for _, tc := range []struct {
		change func(*config.Host)
		name   string
	}{
		{func(h *config.Host) { h.DNS = h.DNS[:1] }, "dnsDropped"},
		{func(h *config.Host) { h.DNS[0], h.DNS[1] = h.DNS[1], h.DNS[0] }, "dnsReordered"},
		{func(h *config.Host) { h.Egress.Internet = false }, "internet"},
		{func(h *config.Host) { h.Egress.Allow[0].Ports[0].Last = 5433 }, "allowPorts"},
		{func(h *config.Host) { h.Subnet = pfx("10.200.0.0/24") }, "subnet"},
		{func(h *config.Host) { h.Capacity.Memory = 1 << 30 }, "capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := hashHost()
			tc.change(h)
			if PolicyHash(h) == base {
				t.Error("the hash did not change")
			}
		})
	}
}

func TestProfileHash(t *testing.T) {
	t.Parallel()
	profiles := func() []config.Profile {
		return []config.Profile{
			{Name: "build", RunnerImage: "build", Hosts: []string{"default"}, Memory: 4 << 30},
			{Name: "small", RunnerImage: "base", Hosts: []string{"nas", "default"}, Memory: 1 << 30},
		}
	}
	base := ProfileHash(profiles(), "default")
	t.Run("stable", func(t *testing.T) {
		t.Parallel()
		if got := ProfileHash(profiles(), "default"); got != base {
			t.Errorf("two equal lists hash %s and %s", base, got)
		}
	})
	t.Run("aProfileOfAnotherHostChanges", func(t *testing.T) {
		t.Parallel()
		ps := append(profiles(), config.Profile{Name: "lan", RunnerImage: "base", Hosts: []string{"nas"}})
		if got := ProfileHash(ps, "default"); got != base {
			t.Error("a profile default does not serve changed default's hash")
		}
		if ProfileHash(ps, "nas") == ProfileHash(profiles(), "nas") {
			t.Error("a profile nas serves did not change nas's hash")
		}
	})
	t.Run("aServedProfileChanges", func(t *testing.T) {
		t.Parallel()
		ps := profiles()
		ps[1].Memory = 2 << 30
		if ProfileHash(ps, "default") == base {
			t.Error("the hash did not change")
		}
	})
	t.Run("hostsDiffer", func(t *testing.T) {
		t.Parallel()
		if ProfileHash(profiles(), "nas") == base {
			t.Error("nas, serving one profile, hashes as default, serving two")
		}
	})
}
