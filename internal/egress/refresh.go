package egress

import (
	"fmt"
	"net/netip"
	"strings"
)

// Refresh answers the payload that adds ls to a loaded rule set's learned
// sets, renewing the timeout of any already there: for NFTables an `nft -f`
// transaction, for IPTables an `ipset restore -exist` payload. Each address
// must have passed Learnable and have time left.
func Refresh(b Backend, ls []Learned) string {
	var w strings.Builder
	for _, l := range ls {
		a := l.Addr.Unmap()
		name := setGitHub4
		if b == IPTables {
			fmt.Fprintf(&w, "add %s %s timeout %d\n", name, a, seconds(l.Left))
			continue
		}
		// An nft add keeps an existing element's timeout and a delete of a
		// missing one fails the transaction: add, delete, add renews either way.
		el := fmt.Sprintf("inet %s %s { %s", nftTable, name, a)
		fmt.Fprintf(&w, "add element %s timeout %ds }\ndelete element %s }\nadd element %s timeout %ds }\n",
			el, seconds(l.Left), el, el, seconds(l.Left))
	}
	return w.String()
}

// The probe's set pairs, sources then target, by family.
var probeSets = [2][2]string{{setProbe4, setTarget4}, {setProbe6, setTarget6}}

// ProbeSources answers the payload that empties every probe set and makes
// addrs the probe's sources: for NFTables an `nft -f` transaction, for
// IPTables an `ipset restore` payload. ipv6 says the host has the IPv6
// sets; an IPv6 address on a host without them is left out.
func ProbeSources(b Backend, ipv6 bool, addrs []netip.Addr) string {
	return probePayload(b, ipv6, 0, addrs, true)
}

// ProbeTarget answers the payload that makes a the target alone, or
// empties the target sets for the zero Addr.
func ProbeTarget(b Backend, ipv6 bool, a netip.Addr) string {
	var addrs []netip.Addr
	if a.IsValid() {
		addrs = []netip.Addr{a}
	}
	return probePayload(b, ipv6, 1, addrs, false)
}

// probePayload flushes set kind (0 sources, 1 target) of each family, the
// other kind too when all is set, then adds addrs.
func probePayload(b Backend, ipv6 bool, kind int, addrs []netip.Addr, all bool) string {
	families := probeSets[:1]
	if ipv6 {
		families = probeSets[:]
	}
	var w strings.Builder
	for _, f := range families {
		for k, name := range f {
			if k == kind || all {
				w.WriteString(setCommand(b, "flush", name, netip.Addr{}))
			}
		}
	}
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is6() && !ipv6 {
			continue
		}
		w.WriteString(setCommand(b, "add", families[family6(a)][kind], a))
	}
	return w.String()
}

func family6(a netip.Addr) int {
	if a.Is6() {
		return 1
	}
	return 0
}

// setCommand is one flush or add of set name in b's payload syntax.
func setCommand(b Backend, op, name string, a netip.Addr) string {
	switch {
	case b == IPTables && op == "flush":
		return "flush " + name + "\n"
	case b == IPTables:
		return "add " + name + " " + a.String() + "\n"
	case op == "flush":
		return "flush set inet " + nftTable + " " + name + "\n"
	}
	return "add element inet " + nftTable + " " + name + " { " + a.String() + " }\n"
}
