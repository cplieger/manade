package netsetup

import (
	"net/netip"
	"slices"
	"testing"
)

// route4 is /proc/net/route on a little-endian machine with a default route
// via 172.20.0.1, the on-link 172.20.0.0/16, a loopback entry, a route that
// is down and 10.0.0.0/8 via a gateway.
const route4 = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
	"eth0\t00000000\t010014AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
	"eth0\t000014AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
	"lo\t0000007F\t00000000\t0001\t0\t0\t0\t000000FF\t0\t0\t0\n" +
	"eth1\t0000A8C0\t00000000\t0000\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
	"eth0\t0000000A\t010014AC\t0003\t0\t0\t0\t000000FF\t0\t0\t0\n"

// route6 is /proc/net/ipv6_route with the on-link fd00:c8::/64, a link-local
// /64, a local route for the host's own address, multicast and a default
// route via fd00:c8::1.
const route6 = "" +
	"fd0000c8000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0\n" +
	"fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0\n" +
	"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fd0000c8000000000000000000000001 00000400 00000001 00000000 00000003     eth0\n" +
	"fd0000c8000000000000000000000002 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001     eth0\n" +
	"00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001       lo\n" +
	"ff000000000000000000000000000000 08 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000002 00000000 00000001     eth0\n"

func TestReadRoutes(t *testing.T) {
	t.Run("both families", func(t *testing.T) {
		rt, err := ReadRoutes(tree(t, map[string]string{"/proc/net/route": route4, "/proc/net/ipv6_route": route6}))
		if err != nil {
			t.Fatal(err)
		}
		want := []netip.Prefix{pfx("172.20.0.0/16"), pfx("fd00:c8::/64"), pfx("fe80::/64"), pfx("ff00::/8")}
		if !slices.Equal(rt.OnLink, want) {
			t.Errorf("OnLink = %v, want %v", rt.OnLink, want)
		}
		if !rt.Default6 {
			t.Error("Default6 = false, want the route via fd00:c8::1")
		}
	})
	t.Run("a kernel without IPv6", func(t *testing.T) {
		rt, err := ReadRoutes(tree(t, map[string]string{"/proc/net/route": route4}))
		if err != nil || len(rt.OnLink) != 1 || rt.Default6 {
			t.Errorf("ReadRoutes = %+v, %v, want the one IPv4 on-link route", rt, err)
		}
	})
	t.Run("a malformed line", func(t *testing.T) {
		bad := route4 + "eth2\tzz\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
		if _, err := ReadRoutes(tree(t, map[string]string{"/proc/net/route": bad})); err == nil {
			t.Error("ReadRoutes took a malformed destination")
		}
	})
	t.Run("an IPv6 prefix length past 128", func(t *testing.T) {
		bad := "fd0000c8000000000000000000000000 81 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0\n"
		if _, err := ReadRoutes(tree(t, map[string]string{"/proc/net/route": route4, "/proc/net/ipv6_route": bad})); err == nil {
			t.Error("ReadRoutes took a /129")
		}
	})
}

func TestGlobalAddrs6(t *testing.T) {
	inet6 := "" +
		"fd0000c8000000000000000000000002 07 40 00 80     eth0\n" +
		"fe80000000000000004242fffeac1402 07 40 20 80     eth0\n" +
		"00000000000000000000000000000001 01 80 10 80       lo\n" +
		"2a010e0a1259271000000000000000aa 07 40 00 08     eth0\n" +
		"2a010e0a12592710000000000000000b 07 40 00 00     eth0\n"
	got, err := GlobalAddrs6(tree(t, map[string]string{"/proc/net/if_inet6": inet6}))
	want := []netip.Addr{addr("fd00:c8::2"), addr("2a01:e0a:1259:2710::b")}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("GlobalAddrs6 = %v, %v, want %v (link-local, loopback and a failed duplicate check left out)", got, err, want)
	}
	if got, err := GlobalAddrs6(tree(t, map[string]string{"/proc/net/route": route4})); err != nil || len(got) != 0 {
		t.Errorf("without IPv6: %v, %v", got, err)
	}
}
