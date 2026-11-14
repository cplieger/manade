package netsetup

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
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
	// Gateway4 and Gateway6 are the default routes' gateways, the zero Addr
	// for a family with none.
	Gateway4, Gateway6 netip.Addr
	// OnLink are the routes with a destination and no gateway, of both
	// families; the default route and the namespace's own addresses are
	// never among them.
	OnLink []netip.Prefix
}

// ReadRoutes reads /proc/net/route and /proc/net/ipv6_route; a kernel
// without IPv6 has no ipv6_route.
func ReadRoutes(r platform.Root) (Routes, error) {
	var out Routes
	v4, _, err := read(r, "/proc/net/route")
	if err != nil {
		return out, err
	}
	if out.OnLink, out.Gateway4, err = parseRoute4(v4); err != nil {
		return out, err
	}
	v6, _, err := read(r, "/proc/net/ipv6_route")
	if err != nil {
		return out, err
	}
	onLink6, gw6, err := parseRoute6(v6)
	out.OnLink = append(out.OnLink, onLink6...)
	out.Gateway6 = gw6
	return out, err
}

// parseRoute4 reads /proc/net/route, whose addresses are 32-bit words the
// kernel prints in host byte order.
func parseRoute4(s string) (onLink []netip.Prefix, gw netip.Addr, err error) {
	var def defaultRoute
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 || f[0] == ifaceLoopback {
			continue
		}
		dst, err1 := word4(f[1])
		next, err2 := word4(f[2])
		flags, err3 := strconv.ParseUint(f[3], 16, 32)
		metric, err5 := strconv.ParseUint(f[6], 10, 32)
		mask, err4 := word4(f[7])
		if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
			return nil, netip.Addr{}, fmt.Errorf("/proc/net/route line %d: %q", i+1, line)
		}
		ones := bits.OnesCount32(binary.BigEndian.Uint32(mask[:]))
		switch {
		case flags&rtfUp == 0:
		case ones == 0 && flags&rtfGateway != 0:
			def.offer(netip.AddrFrom4(next), metric)
		case flags&rtfGateway == 0 && ones != 0:
			onLink = append(onLink, netip.PrefixFrom(netip.AddrFrom4(dst), ones).Masked())
		}
	}
	return onLink, def.gw, nil
}

// defaultRoute keeps the default route the kernel prefers: the lowest
// metric, the first listed among equals.
type defaultRoute struct {
	gw     netip.Addr
	metric uint64
}

func (d *defaultRoute) offer(gw netip.Addr, metric uint64) {
	if !d.gw.IsValid() || metric < d.metric {
		d.gw, d.metric = gw, metric
	}
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
func parseRoute6(s string) (onLink []netip.Prefix, gw netip.Addr, err error) {
	var def defaultRoute
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[9] == ifaceLoopback {
			continue
		}
		dst, err1 := addr6(f[0])
		n, err2 := strconv.ParseUint(f[1], 16, 8)
		next, err3 := addr6(f[4])
		metric, err5 := strconv.ParseUint(f[5], 16, 32)
		flags, err4 := strconv.ParseUint(f[8], 16, 32)
		if errors.Join(err1, err2, err3, err4, err5) != nil || n > 128 {
			return nil, netip.Addr{}, fmt.Errorf("/proc/net/ipv6_route line %d: %q", i+1, line)
		}
		switch {
		case flags&rtfUp == 0, flags&rtfLocal != 0:
		case n == 0:
			if flags&rtfGateway != 0 {
				def.offer(next, metric)
			}
		case flags&rtfGateway == 0:
			onLink = append(onLink, netip.PrefixFrom(dst, int(n)).Masked())
		}
	}
	return onLink, def.gw, nil
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
