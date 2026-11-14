package netsetup

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// errNoIPv6 is a host with egress.ipv6 whose namespace or kernel cannot
// carry IPv6 jobs.
var errNoIPv6 = errors.New("netsetup: egress.ipv6 is true, but")

// CheckIPv6 checks egress.ipv6's prerequisites, an IPv6 address and default
// route on the host's own network and the kernel's IPv6 nat table, and
// answers the /64 of the host's own address when that address is global
// unicast, or the zero Prefix.
func CheckIPv6(addrs []netip.Addr, rt Routes, natTable bool) (netip.Prefix, error) {
	var missing []string
	if len(addrs) == 0 {
		missing = append(missing, "the host container's own network gives it no IPv6 address")
	}
	if !rt.Gateway6.IsValid() {
		missing = append(missing, "it has no IPv6 default route")
	}
	if !natTable {
		missing = append(missing, "the kernel has no IPv6 nat table")
	}
	if len(missing) > 0 {
		return netip.Prefix{}, fmt.Errorf("%w %s; put the host on a network with IPv6 on a kernel with the IPv6 nat table, or set egress.ipv6 false",
			errNoIPv6, strings.Join(missing, ", "))
	}
	for _, a := range addrs {
		if globalUnicast6.Contains(a) {
			return netip.PrefixFrom(a, 64).Masked(), nil
		}
	}
	return netip.Prefix{}, nil
}
