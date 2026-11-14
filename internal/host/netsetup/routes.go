package netsetup

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"net/netip"
	"strconv"
	"strings"

	"github.com/cplieger/manade/internal/host/platform"
)

// Route flags, from include/uapi/linux/route.h.
const (
	rtfUp      = 0x1
	rtfGateway = 0x2
	rtfLocal   = 0x80000000
)

// ifaceLoopback is skipped: its routes are the namespace's own addresses.
const ifaceLoopback = "lo"

// Routes are what the namespace's routing tables show.
type Routes struct {
	// OnLink are the routes with a destination and no gateway, of both
	// families; the default route and the namespace's own addresses are
	// never among them.
	OnLink []netip.Prefix
	// Default6 is an IPv6 default route through a gateway.
	Default6 bool
}

// ReadRoutes reads /proc/net/route and /proc/net/ipv6_route; a kernel
// without IPv6 has no ipv6_route.
func ReadRoutes(r platform.Root) (Routes, error) {
	var out Routes
	v4, _, err := read(r, "/proc/net/route")
	if err != nil {
		return out, err
	}
	if out.OnLink, err = parseRoute4(v4); err != nil {
		return out, err
	}
	v6, _, err := read(r, "/proc/net/ipv6_route")
	if err != nil {
		return out, err
	}
	onLink6, def6, err := parseRoute6(v6)
	out.OnLink = append(out.OnLink, onLink6...)
	out.Default6 = def6
	return out, err
}

// parseRoute4 reads /proc/net/route, whose addresses are 32-bit words the
// kernel prints in host byte order.
func parseRoute4(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 || f[0] == ifaceLoopback {
			continue
		}
		dst, err1 := word4(f[1])
		flags, err2 := strconv.ParseUint(f[3], 16, 32)
		mask, err3 := word4(f[7])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("/proc/net/route line %d: %q", i+1, line)
		}
		ones := bits.OnesCount32(binary.BigEndian.Uint32(mask[:]))
		if flags&rtfUp == 0 || flags&rtfGateway != 0 || ones == 0 {
			continue
		}
		out = append(out, netip.PrefixFrom(netip.AddrFrom4(dst), ones).Masked())
	}
	return out, nil
}

func word4(s string) ([4]byte, error) {
	var b [4]byte
	v, err := strconv.ParseUint(s, 16, 32)
	binary.NativeEndian.PutUint32(b[:], uint32(v))
	return b, err
}

// parseRoute6 reads /proc/net/ipv6_route: destination, its length, source,
// its length, next hop, metric, references, use, flags and interface, the
// addresses in network order.
func parseRoute6(s string) (onLink []netip.Prefix, def bool, err error) {
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[9] == ifaceLoopback {
			continue
		}
		dst, err1 := addr6(f[0])
		n, err2 := strconv.ParseUint(f[1], 16, 8)
		flags, err3 := strconv.ParseUint(f[8], 16, 32)
		if err1 != nil || err2 != nil || err3 != nil || n > 128 {
			return nil, false, fmt.Errorf("/proc/net/ipv6_route line %d: %q", i+1, line)
		}
		switch {
		case flags&rtfUp == 0, flags&rtfLocal != 0:
		case n == 0:
			def = def || flags&rtfGateway != 0
		case flags&rtfGateway == 0:
			onLink = append(onLink, netip.PrefixFrom(dst, int(n)).Masked())
		}
	}
	return onLink, def, nil
}

func addr6(s string) (netip.Addr, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return netip.Addr{}, fmt.Errorf("not an IPv6 address: %q", s)
	}
	return netip.AddrFrom16([16]byte(b)), nil
}

// IPv6 address flags and scopes, from include/uapi/linux/if_addr.h and
// net/ipv6/addrconf.c.
const (
	ifaDADFailed = 0x08
	scopeGlobal  = 0x00
)

// GlobalAddrs6 answers the namespace's IPv6 addresses of global scope, ULA
// included, from /proc/net/if_inet6, leaving out loopback and an address
// whose duplicate detection failed.
func GlobalAddrs6(r platform.Root) ([]netip.Addr, error) {
	s, _, err := read(r, "/proc/net/if_inet6")
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[5] == ifaceLoopback {
			continue
		}
		a, err1 := addr6(f[0])
		scope, err2 := strconv.ParseUint(f[3], 16, 8)
		flags, err3 := strconv.ParseUint(f[4], 16, 32)
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("/proc/net/if_inet6 line %d: %q", i+1, line)
		}
		if scope == scopeGlobal && flags&ifaDADFailed == 0 {
			out = append(out, a)
		}
	}
	return out, nil
}
