package egress

import (
	"net/netip"
	"testing"
	"time"
)

func TestRefresh(t *testing.T) {
	ls := []Learned{
		{Addr: netip.MustParseAddr("::ffff:140.82.121.10"), Left: 1500 * time.Millisecond},
		{Addr: netip.MustParseAddr("140.82.121.11"), Left: 11 * time.Minute},
	}
	if got, want := Refresh(IPTables, ls), "add manade-gh4 140.82.121.10 timeout 2\nadd manade-gh4 140.82.121.11 timeout 660\n"; got != want {
		t.Errorf("Refresh(IPTables) = %q, want %q", got, want)
	}
	want := "add element inet manade manade-gh4 { 140.82.121.10 timeout 2s }\n" +
		"delete element inet manade manade-gh4 { 140.82.121.10 }\n" +
		"add element inet manade manade-gh4 { 140.82.121.10 timeout 2s }\n" +
		"add element inet manade manade-gh4 { 140.82.121.11 timeout 660s }\n" +
		"delete element inet manade manade-gh4 { 140.82.121.11 }\n" +
		"add element inet manade manade-gh4 { 140.82.121.11 timeout 660s }\n"
	if got := Refresh(NFTables, ls); got != want {
		t.Errorf("Refresh(NFTables) = %q, want %q", got, want)
	}
}

// An internet: false host never learns the machine's /64, so no IPv6
// address is learnable: one inside that /64 would open the LAN.
func TestLearnable(t *testing.T) {
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
		{"2606:50c0:8000::154", false},
		{"2a01:e0a:1259:2710::9", false},
		{"fd00::1", false},
		{"fe80::1", false},
		{"::1", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if got := Learnable(netip.MustParseAddr(tc.addr)); got != tc.want {
				t.Errorf("Learnable(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
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
