package egress

import (
	"net/netip"
	"testing"
	"time"

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
		{func(h *config.Host) { h.Egress.Block6 = []netip.Prefix{pfx("fd00::/8")} }, "block6"},
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
	// A proof stands across a change outside the rules.
	for _, tc := range []struct {
		change func(*config.Host)
		name   string
	}{
		{func(h *config.Host) { h.ImageRefresh = 30 * time.Minute }, "imagesRefresh"},
		{func(h *config.Host) { h.Capacity.Memory = 1 << 30 }, "capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := hashHost()
			tc.change(h)
			if PolicyHash(h) != base {
				t.Error("the hash changed")
			}
		})
	}
}
