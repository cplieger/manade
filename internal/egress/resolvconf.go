package egress

import (
	"net/netip"
	"slices"
	"strings"
)

// dockerResolver is Docker's embedded resolver, which a container's
// resolv.conf names on a user-defined network.
var dockerResolver = netip.MustParseAddr("127.0.0.11")

// Resolvers are the resolvers a host container's resolv.conf named before
// the host rewrote it.
type Resolvers struct {
	// Addrs are the nameservers the file named, then the upstreams its
	// ExtServers comment lists, each once and in that order, with no zone
	// and Docker's embedded resolver left out.
	Addrs []netip.Addr
	// Unknown is true when Addrs is empty: the file named only Docker's
	// embedded resolver, or nothing, and no ExtServers comment listed its
	// upstreams, as Docker 24 writes it.
	Unknown bool
}

// ParseResolvConf reads the original resolvers out of a resolv.conf.
// Lines it cannot read are skipped, as the C library skips them.
func ParseResolvConf(data []byte) Resolvers {
	var named, ext []netip.Addr
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		// Docker writes "# ExtServers: [host(192.168.1.87) 8.8.8.8]", host()
		// marking an address reached from the machine's namespace
		// (https://github.com/moby/moby/blob/docker-v29.0.0/daemon/libnetwork/internal/resolvconf/resolvconf.go).
		if list, ok := strings.CutPrefix(line, "# ExtServers:"); ok {
			ext = append(ext, extServers(list)...)
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil {
			named = append(named, a)
		}
	}
	var out []netip.Addr
	for _, a := range slices.Concat(named, ext) {
		// The rules and the probe key on addresses; a zone names an
		// interface of the machine's that neither has.
		a = a.WithZone("")
		if a != dockerResolver && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return Resolvers{Addrs: out, Unknown: len(out) == 0}
}

func extServers(list string) []netip.Addr {
	list = strings.TrimSpace(list)
	list = strings.TrimSuffix(strings.TrimPrefix(list, "["), "]")
	var out []netip.Addr
	for w := range strings.FieldsSeq(list) {
		if inner, ok := strings.CutPrefix(w, "host("); ok {
			w = strings.TrimSuffix(inner, ")")
		}
		if a, err := netip.ParseAddr(w); err == nil {
			out = append(out, a)
		}
	}
	return out
}
