package egress

import (
	"net/netip"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
)

func TestRefresh(t *testing.T) {
	ls := []Learned{
		{Addr: netip.MustParseAddr("::ffff:140.82.121.10"), Left: 1500 * time.Millisecond},
		{Addr: netip.MustParseAddr("2606:50c0:8000::154"), Left: 11 * time.Minute},
	}
	if got, want := Refresh(IPTables, ls), "add manade-gh4 140.82.121.10 timeout 2\nadd manade-gh6 2606:50c0:8000::154 timeout 660\n"; got != want {
		t.Errorf("Refresh(IPTables) = %q, want %q", got, want)
	}
	want := "add element inet manade manade-gh4 { 140.82.121.10 timeout 2s }\n" +
		"delete element inet manade manade-gh4 { 140.82.121.10 }\n" +
		"add element inet manade manade-gh4 { 140.82.121.10 timeout 2s }\n" +
		"add element inet manade manade-gh6 { 2606:50c0:8000::154 timeout 660s }\n" +
		"delete element inet manade manade-gh6 { 2606:50c0:8000::154 }\n" +
		"add element inet manade manade-gh6 { 2606:50c0:8000::154 timeout 660s }\n"
	if got := Refresh(NFTables, ls); got != want {
		t.Errorf("Refresh(NFTables) = %q, want %q", got, want)
	}
}

func TestLearnable(t *testing.T) {
	h := &config.Host{Name: "closed", DNS: config.DefaultDNS, Egress: config.Egress{
		IPv6: true, Block6: []netip.Prefix{netip.MustParsePrefix("2a01:e0a::/32")},
	}}
	ok := (&Input{Host: h, State: State{Own64: netip.MustParsePrefix("2a02:8428:1:2::/64")}}).Learnable()
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"140.82.121.10", true},
		{"::ffff:140.82.121.10", true},
		{"10.1.2.3", false},
		{"100.64.0.1", false},
		{"169.254.169.254", false},
		{"172.20.0.1", false},
		{"192.168.1.1", false},
		{"127.0.0.1", false},
		{"224.0.0.1", false},
		{"0.0.0.0", false},
		{"2606:50c0:8000::154", true},
		{"fd00::1", false},
		{"fe80::1", false},
		{"ff02::1", false},
		{"64:ff9b::808:808", false},
		{"2001:db8::1", false},
		{"2002:c000:204::1", false},
		{"2a01:e0a:1:2::3", false},
		{"2a02:8428:1:2::9", false},
		{"::1", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if got := ok(netip.MustParseAddr(tc.addr)); got != tc.want {
				t.Errorf("Learnable(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
	h.Egress.IPv6 = false
	if (&Input{Host: h}).Learnable()(netip.MustParseAddr("2606:50c0:8000::154")) {
		t.Error("an IPv6 address is learnable on a host without ipv6, which has no IPv6 set")
	}
}
