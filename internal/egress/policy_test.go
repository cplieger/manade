package egress

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr  { return netip.MustParseAddr(s) }

func ports(rs ...[2]uint16) []config.PortRange {
	out := make([]config.PortRange, len(rs))
	for i, r := range rs {
		out[i] = config.PortRange{First: r[0], Last: r[1]}
	}
	return out
}

var learnedGitHub = []Learned{
	{Addr: addr("140.82.112.3"), Left: time.Hour},
	{Addr: addr("140.82.112.4"), Left: 30*time.Minute + 200*time.Millisecond},
	{Addr: addr("2606:50c0:8000::153"), Left: 20 * time.Minute},
	{Addr: addr("140.82.112.5")},
}

// scenario is one host shape the goldens cover.
type scenario struct {
	in   Input
	name string
}

func scenarios() []scenario {
	job6 := pfx("fdc6:6d61:6e61:6465::/64")
	learned := State{Self64: pfx("2a01:e0a:1259:2710::/64"), Own64: pfx("2a01:e0a:1259:2711::/64")}
	v6Host := func(internet bool) *config.Host {
		return &config.Host{
			Name: "v6",
			DNS:  []netip.Addr{addr("1.1.1.1"), addr("2606:4700:4700::1111")},
			Egress: config.Egress{
				Internet: internet, IPv6: true,
				Block6: []netip.Prefix{pfx("2a01:e0a:1::/48")},
				Allow: []config.Allow{
					{To: pfx("10.0.0.50/32"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{5432, 5432})},
					{To: pfx("fd00::5/128"), Protocol: config.ProtocolUDP, Ports: ports([2]uint16{53, 53})},
				},
			},
		}
	}
	offState := State{GitHub: learnedGitHub}
	off6State := State{Own64: learned.Own64, GitHub: learnedGitHub}
	return []scenario{
		{name: "internet", in: Input{Host: &config.Host{
			Name: "default",
			DNS:  config.DefaultDNS,
			Egress: config.Egress{Internet: true, Allow: []config.Allow{
				{To: pfx("10.0.0.50/32"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{5432, 5432})},
				{To: pfx("192.168.2.0/24"), Protocol: config.ProtocolBoth, Ports: ports([2]uint16{8000, 8100}, [2]uint16{9000, 9000})},
			}},
		}}},
		{name: "internet-off", in: Input{Host: &config.Host{
			Name:   "offline",
			DNS:    []netip.Addr{addr("192.168.1.53"), addr("1.1.1.1"), addr("192.168.1.53")},
			Egress: config.Egress{},
		}, State: offState}},
		{name: "ipv6", in: Input{Host: v6Host(true), Job6: job6, State: learned}},
		{name: "ipv6-unlearned", in: Input{Host: v6Host(true), Job6: job6}},
		{name: "ipv6-internet-off", in: Input{Host: v6Host(false), Job6: job6, State: off6State}},
		{name: "open", in: Input{Host: &config.Host{Name: "open", DNS: config.DefaultDNS, Egress: config.Egress{Open: true, Internet: true}}}},
	}
}

// iptGolden is the iptables backend's output in one file.
func iptGolden(rs *Ruleset) []byte {
	var b strings.Builder
	b.WriteString("# ipset restore -exist\n" + rs.IPSet)
	b.WriteString("# iptables-restore --noflush\n" + rs.IPv4)
	b.WriteString("# ip6tables-restore --noflush\n" + rs.IPv6)
	writeJumps := func(js []Jump) {
		for _, j := range js {
			if j.IPv6 {
				b.WriteString("ip6 ")
			}
			b.WriteString(j.Table + " " + j.Chain + " -j " + j.Target + "\n")
		}
	}
	b.WriteString("# jumps\n")
	writeJumps(rs.Jumps)
	b.WriteString("# ip6tables-restore --noflush, where a nat table lists\n" + rs.IPv6NATReset)
	b.WriteString("# stale jumps, the nat one where a nat table lists\n")
	writeJumps(rs.StaleJumps)
	b.WriteString("# stale ipsets, destroyed where the kernel holds them\n")
	for _, s := range rs.StaleSets {
		b.WriteString(s + "\n")
	}
	return []byte(b.String())
}

func TestGenerateGolden(t *testing.T) {
	t.Parallel()
	for _, sc := range scenarios() {
		for _, b := range []struct {
			render func(*Ruleset) []byte
			ext    string
			be     Backend
		}{
			{func(rs *Ruleset) []byte { return []byte(rs.NFT) }, "nft", NFTables},
			{iptGolden, "iptables", IPTables},
		} {
			t.Run(sc.name+"_"+b.ext, func(t *testing.T) {
				t.Parallel()
				rs, err := Generate(&sc.in, b.be)
				if err != nil {
					t.Fatal(err)
				}
				checkGolden(t, filepath.Join("testdata", "golden", sc.name+"."+b.ext+".golden"), b.render(rs))
			})
		}
	}
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; regenerate with UPDATE_GOLDEN=1 go test ./internal/egress/ -run TestGenerateGolden", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs; regenerate with UPDATE_GOLDEN=1 go test ./internal/egress/ -run TestGenerateGolden\ngot:\n%s", path, got)
	}
}

func TestGenerateLearnedTimeoutCeiling(t *testing.T) {
	t.Parallel()
	// IPSET_MAX_TIMEOUT is (UINT_MAX >> 1) / MSEC_PER_SEC seconds; a DNS
	// TTL may reach 2^31-1 s (RFC 2181 section 8).
	in := Input{
		Host: &config.Host{Name: "offline", DNS: config.DefaultDNS},
		State: State{GitHub: []Learned{
			{Addr: addr("140.82.112.3"), Left: 2147483 * time.Second},
			{Addr: addr("140.82.112.4"), Left: 2147483*time.Second + time.Millisecond},
			{Addr: addr("140.82.112.5"), Left: (1<<31 - 1) * time.Second},
		}},
	}
	for _, tc := range []struct {
		payload func(*Ruleset) string
		name    string
		be      Backend
		want    []string
	}{
		{func(rs *Ruleset) string { return rs.NFT }, "nftables", NFTables, []string{
			"140.82.112.3 timeout 2147483s, 140.82.112.4 timeout 2147483s, 140.82.112.5 timeout 2147483s",
		}},
		{func(rs *Ruleset) string { return rs.IPSet }, "iptables", IPTables, []string{
			"add manade-gh4 140.82.112.3 timeout 2147483\n",
			"add manade-gh4 140.82.112.4 timeout 2147483\n",
			"add manade-gh4 140.82.112.5 timeout 2147483\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs, err := Generate(&in, tc.be)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(tc.payload(rs), w) {
					t.Errorf("payload lacks %q:\n%s", w, tc.payload(rs))
				}
			}
		})
	}
}

// A GitHub name has no private answer, and the learned set's accept comes
// before the private and IPv6 drops, so such an answer must not open them;
// no IPv6 address enters, the machine's /64 being unknown on such a host.
func TestGenerateLearnedLeavesBlockedOut(t *testing.T) {
	t.Parallel()
	const hour = time.Hour
	in := Input{
		Host: &config.Host{Name: "offline", DNS: config.DefaultDNS, Egress: config.Egress{
			IPv6: true, Block6: []netip.Prefix{pfx("2a01:e0a:1::/48")},
		}},
		Job6:  pfx("fdc6:6d61:6e61:6465::/64"),
		State: State{Own64: pfx("2a01:e0a:1259:2711::/64")},
	}
	kept := []string{"140.82.112.3"}
	left := []string{
		"2606:50c0:8000::153", "2a01:e0a:1259:2710::9",
		"10.1.2.3", "192.168.0.9", "172.16.5.5", "100.64.0.1", "169.254.169.254",
		"224.0.0.251", "239.255.255.250", "255.255.255.255", "127.0.0.1", "0.0.0.0",
		"fc00::1", "fd12::5", "fe80::1", "2001:db8::1", "64:ff9b::a01:203",
		"2a01:e0a:1:5::1", "2a01:e0a:1259:2711::9", "::1",
	}
	for _, a := range slices.Concat(kept, left) {
		in.State.GitHub = append(in.State.GitHub, Learned{Addr: addr(a), Left: hour})
	}
	in.State.GitHub = append(in.State.GitHub, Learned{Addr: addr("::ffff:10.9.9.9"), Left: hour})
	left = append(left, "10.9.9.9")
	for _, tc := range []struct {
		payload func(*Ruleset) string
		name    string
		be      Backend
	}{
		{func(rs *Ruleset) string { return rs.NFT }, "nftables", NFTables},
		{func(rs *Ruleset) string { return rs.IPSet }, "iptables", IPTables},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs, err := Generate(&in, tc.be)
			if err != nil {
				t.Fatal(err)
			}
			got := tc.payload(rs)
			for _, a := range kept {
				if !strings.Contains(got, " "+a+" timeout") {
					t.Errorf("payload lacks the learned %s:\n%s", a, got)
				}
			}
			for _, a := range left {
				if strings.Contains(got, " "+a+" timeout") {
					t.Errorf("payload holds the blocked %s:\n%s", a, got)
				}
			}
		})
	}
}
