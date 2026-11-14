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

func TestProbeSets(t *testing.T) {
	a4, a6 := netip.MustParseAddr("172.18.0.2"), netip.MustParseAddr("fd00::2")
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{
			"nftables sources on an ipv6 host", ProbeSources(NFTables, true, []netip.Addr{a4, a6}),
			"flush set inet manade manade-probe4\nflush set inet manade manade-target4\nflush set inet manade manade-probe6\nflush set inet manade manade-target6\n" +
				"add element inet manade manade-probe4 { 172.18.0.2 }\nadd element inet manade manade-probe6 { fd00::2 }\n",
		},
		{
			"ipset sources leave IPv6 out without ipv6", ProbeSources(IPTables, false, []netip.Addr{a4, a6}),
			"flush manade-probe4\nflush manade-target4\nadd manade-probe4 172.18.0.2\n",
		},
		{
			"a target alone", ProbeTarget(IPTables, true, netip.MustParseAddr("::ffff:10.0.0.1")),
			"flush manade-target4\nflush manade-target6\nadd manade-target4 10.0.0.1\n",
		},
		{
			"the zero target empties the target sets", ProbeTarget(NFTables, false, netip.Addr{}),
			"flush set inet manade manade-target4\n",
		},
		{"clearing", ProbeSources(IPTables, false, nil), "flush manade-probe4\nflush manade-target4\n"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s:\n%s\nwant\n%s", tc.name, tc.got, tc.want)
		}
	}
}
