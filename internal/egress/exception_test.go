package egress

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/config"
)

var bothBackends = []Backend{NFTables, IPTables}

func scenarioInput(t *testing.T, name string) Input {
	t.Helper()
	for _, sc := range scenarios() {
		if sc.name == name {
			return sc.in
		}
	}
	t.Fatalf("no scenario %s", name)
	return Input{}
}

func chainOf(t *testing.T, rs *Ruleset, hook string) []rule {
	t.Helper()
	for _, c := range rs.chains {
		if c.hook == hook {
			return c.rules
		}
	}
	t.Fatalf("no chain %s", hook)
	return nil
}

// index answers the position of the first rule of rules that is no counter
// and matches, or -1.
func index(rules []rule, match func(*rule) bool) int {
	return slices.IndexFunc(rules, func(r rule) bool { return r.action != count && match(&r) })
}

var exceptionSets = []string{setCP4, setCP6, setLast4, setLast6, setJoin4, setJoin6}

// The forward chain drops the exception sets right after the established
// accept, ahead of the MANADE_BLOCK6 drops and every allow accept, so an
// allow entry covering the control plane's address never reopens it; the
// output chain accepts each target's marked dials right after the
// established accept, ahead of the IPv6 drop.
func TestGenerateExceptionOrder(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"enrolled", "enrolled-ipv6-target", "enrolled-ipv6"} {
		for _, be := range bothBackends {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				in := scenarioInput(t, name)
				rs, err := Generate(&in, be)
				if err != nil {
					t.Fatal(err)
				}
				fwd, out := chainOf(t, rs, hookForward), chainOf(t, rs, hookOutput)
				established := index(fwd, func(r *rule) bool { return r.ct == ctEstablished })
				firstAllow := index(fwd, func(r *rule) bool { return r.action == accept && r.ct == "" })
				lastSetDrop := -1
				for i, r := range fwd {
					if r.action == drop && r.daddrSet != "" {
						lastSetDrop = i
					}
				}
				if first := index(fwd, func(r *rule) bool { return r.action == drop && r.daddrSet != "" }); first != established+2 {
					t.Errorf("the first exception drop is at %d, want right after the established accept at %d and its counter", first, established)
				}
				if lastSetDrop > firstAllow {
					t.Errorf("an exception drop at %d follows the first allow accept at %d", lastSetDrop, firstAllow)
				}
				if b := index(fwd, func(r *rule) bool { return r.action == drop && len(r.daddr) > 0 }); b < lastSetDrop {
					t.Errorf("a prefix drop at %d precedes the last exception drop at %d", b, lastSetDrop)
				}
				outEst := index(out, func(r *rule) bool { return r.ct == ctEstablished })
				for i, r := range out {
					if r.mark && (r.action != accept || i <= outEst) {
						t.Errorf("output rule %d carries the mark: %s", i, r.nft())
					}
				}
				firstMark := index(out, func(r *rule) bool { return r.mark })
				if firstMark != outEst+1 {
					t.Errorf("the first marked accept is at %d, want %d", firstMark, outEst+1)
				}
				if v6drop := index(out, func(r *rule) bool { return r.family == ipv6 && r.action == drop }); v6drop >= 0 && v6drop < firstMark {
					t.Errorf("the IPv6 drop at %d precedes the marked accepts at %d", v6drop, firstMark)
				}
			})
		}
	}
}

// Every rule carrying the mark is a marked output accept of TCP to one
// exception set on its target's port, and no other rule carries it.
func TestGenerateMarkOnlyOnTheExceptionAccepts(t *testing.T) {
	t.Parallel()
	in := scenarioInput(t, "enrolled-ipv6-target")
	rs, err := Generate(&in, NFTables)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint16{setCP4: 7464, setCP6: 7464, setLast4: 7464, setLast6: 7464, setJoin4: 8443, setJoin6: 8443}
	got := map[string]uint16{}
	for _, c := range rs.chains {
		for _, r := range c.rules {
			if !r.mark {
				continue
			}
			if c.hook != hookOutput || r.action != accept || r.proto != config.ProtocolTCP || len(r.dports) != 1 || r.dports[0].First != r.dports[0].Last {
				t.Errorf("chain %s carries the mark on %s", c.hook, r.nft())
				continue
			}
			got[r.daddrSet] = r.dports[0].First
		}
	}
	for set, port := range want {
		if got[set] != port {
			t.Errorf("set %s is accepted on port %d, want %d", set, got[set], port)
		}
	}
	if len(got) != len(want) {
		t.Errorf("marked accepts %v, want %v", got, want)
	}
}

// The neighbour discovery accept sits between the marked accepts and the
// IPv6 drop on a host with ipv6 off only when its namespace keeps IPv6 for
// a dial target.
func TestGenerateNeighbourDiscoveryForAnIPv6Target(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		keep bool
	}{{"kept", true}, {"turnedOff", false}} {
		for _, be := range bothBackends {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				in := scenarioInput(t, "enrolled-ipv6-target")
				in.KeepIPv6 = tc.keep
				rs, err := Generate(&in, be)
				if err != nil {
					t.Fatal(err)
				}
				out := chainOf(t, rs, hookOutput)
				nd := index(out, func(r *rule) bool { return r.nd })
				lastMark := -1
				for i, r := range out {
					if r.mark {
						lastMark = i
					}
				}
				v6drop := index(out, func(r *rule) bool { return r.family == ipv6 && r.action == drop })
				switch {
				case !tc.keep && nd >= 0:
					t.Errorf("output accepts neighbour discovery at %d with no IPv6 kept", nd)
				case tc.keep && (nd != lastMark+1 || v6drop != nd+1):
					t.Errorf("neighbour discovery at %d, marked accepts end at %d, IPv6 drop at %d", nd, lastMark, v6drop)
				}
				if be == IPTables && tc.keep {
					for _, l := range []string{"--icmpv6-type 135 -j RETURN", "--icmpv6-type 136 -j RETURN"} {
						if !strings.Contains(rs.IPv6, "-A MANADE-OUT -p ipv6-icmp -m icmp6 "+l) {
							t.Errorf("ip6tables payload lacks %s", l)
						}
					}
					if strings.Contains(rs.IPv6, "-A MANADE-OUT -p ipv6-icmp -j RETURN") {
						t.Error("ip6tables payload accepts all of ICMPv6")
					}
				}
			})
		}
	}
}

// A counter ahead of an exception drop keeps the set's match beside the
// probe's, so it counts only a probe packet to the control plane.
func TestGenerateExceptionCountersKeepTheSet(t *testing.T) {
	t.Parallel()
	in := scenarioInput(t, "enrolled")
	for _, tc := range []struct {
		payload func(*Ruleset) string
		want    string
		be      Backend
	}{
		{func(rs *Ruleset) string { return rs.NFT }, `iifname "manade*" ip daddr @manade-cp4 ip saddr @manade-probe4 ip daddr @manade-target4 counter`, NFTables},
		{func(rs *Ruleset) string { return rs.IPv4 }, "-A MANADE-FWD -i manade+ -m set --match-set manade-cp4 dst -m set --match-set manade-probe4 src -m set --match-set manade-target4 dst\n", IPTables},
	} {
		rs, err := Generate(&in, tc.be)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tc.payload(rs), tc.want) {
			t.Errorf("backend %d payload lacks %q", tc.be, tc.want)
		}
	}
}

// A socket host renders no exception set and no marked rule, and on
// iptables destroys any exception set the kernel still holds.
func TestGenerateSocketHostHasNoException(t *testing.T) {
	t.Parallel()
	in := scenarioInput(t, "internet")
	for _, be := range bothBackends {
		rs, err := Generate(&in, be)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range rs.sets {
			if slices.Contains(exceptionSets, s.name) {
				t.Errorf("backend %d declares %s", be, s.name)
			}
		}
		if strings.Contains(rs.NFT+rs.IPv4+rs.IPv6, "mark") {
			t.Errorf("backend %d renders a mark", be)
		}
		if be == IPTables {
			for _, name := range exceptionSets {
				if !slices.Contains(rs.StaleSets, name) {
					t.Errorf("stale sets %v lack %s", rs.StaleSets, name)
				}
			}
		}
	}
}

func TestGenerateRefusesABadControlPlane(t *testing.T) {
	t.Parallel()
	host := &config.Host{Name: "remote", DNS: config.DefaultDNS, Egress: config.Egress{Internet: true}}
	for _, tc := range []struct {
		name string
		in   Input
		want string
	}{
		{"addressesWithNoPort", Input{Host: host, ControlPlane: &ControlPlane{
			Newest: Target{Addrs: []netip.Addr{addr("192.168.1.80")}, Port: 7464},
			Join:   Target{Addrs: []netip.Addr{addr("192.168.1.81")}},
		}}, "dial target 3 has addresses and no port"},
		{"ipv6KeptOnASocketHost", Input{Host: host, KeepIPv6: true}, "IPv6 kept for a dial target on a host with none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Generate(&tc.in, NFTables); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Generate = %v, want %q", err, tc.want)
			}
		})
	}
}

// A replacement flushes and fills each set in one nft transaction, and on
// iptables fills a temporary set and swaps it in, never flushing, deleting
// from or destroying the live set; it writes both families, so a target
// whose name moved to one family empties the other's set.
func TestReplaceTarget(t *testing.T) {
	t.Parallel()
	addrs := []netip.Addr{addr("2001:db8:7::20"), addr("203.0.113.20"), addr("::ffff:203.0.113.20"), addr("203.0.113.5")}
	for _, tc := range []struct {
		name  string
		addrs []netip.Addr
		want  string
		be    Backend
	}{
		{"nft", addrs, "flush set inet manade manade-cpjoin4\n" +
			"add element inet manade manade-cpjoin4 { 203.0.113.5, 203.0.113.20 }\n" +
			"flush set inet manade manade-cpjoin6\n" +
			"add element inet manade manade-cpjoin6 { 2001:db8:7::20 }\n", NFTables},
		{"nftEmptied", nil, "flush set inet manade manade-cpjoin4\nflush set inet manade manade-cpjoin6\n", NFTables},
		{"ipset", addrs, "create manade-cpjoin4-next hash:ip family inet maxelem 65536\nflush manade-cpjoin4-next\n" +
			"add manade-cpjoin4-next 203.0.113.5\nadd manade-cpjoin4-next 203.0.113.20\n" +
			"swap manade-cpjoin4-next manade-cpjoin4\ndestroy manade-cpjoin4-next\n" +
			"create manade-cpjoin6-next hash:ip family inet6 maxelem 65536\nflush manade-cpjoin6-next\n" +
			"add manade-cpjoin6-next 2001:db8:7::20\n" +
			"swap manade-cpjoin6-next manade-cpjoin6\ndestroy manade-cpjoin6-next\n", IPTables},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ReplaceTarget(tc.be, TargetJoin, tc.addrs)
			if got != tc.want {
				t.Errorf("payload:\n%s\nwant:\n%s", got, tc.want)
			}
			if tc.be != IPTables {
				return
			}
			for line := range strings.Lines(got) {
				f := strings.Fields(line)
				if f[0] != "swap" && (f[1] == setJoin4 || f[1] == setJoin6) {
					t.Errorf("the payload touches the live set: %s", line)
				}
			}
		})
	}
	for _, d := range dialTargets {
		set4, set6 := d.sets()
		got := ReplaceTarget(NFTables, d, []netip.Addr{addr("192.168.1.80")})
		if !strings.Contains(got, "manade "+set4+" { 192.168.1.80 }") || !strings.Contains(got, "flush set inet manade "+set6+"\n") {
			t.Errorf("target %d payload:\n%s", d, got)
		}
	}
}

// A full load of an exception set on iptables goes through the same swap,
// so a reload never empties a live exception set either.
func TestGenerateIPSetSwapsExceptionSets(t *testing.T) {
	t.Parallel()
	in := scenarioInput(t, "enrolled")
	rs, err := Generate(&in, IPTables)
	if err != nil {
		t.Fatal(err)
	}
	want := "create manade-cp4 hash:ip family inet maxelem 65536\n" +
		"create manade-cp4-next hash:ip family inet maxelem 65536\nflush manade-cp4-next\n" +
		"add manade-cp4-next 192.168.1.80\nswap manade-cp4-next manade-cp4\ndestroy manade-cp4-next\n"
	if !strings.Contains(rs.IPSet, want) {
		t.Errorf("ipset payload lacks\n%s\nin\n%s", want, rs.IPSet)
	}
	for line := range strings.Lines(rs.IPSet) {
		if f := strings.Fields(line); f[0] == "flush" && slices.Contains(exceptionSets, f[1]) {
			t.Errorf("the payload flushes a live exception set: %s", line)
		}
	}
}

// testdata/readback/enrolled-replaced.* is what the kernel listed after the
// enrolled scenario was loaded and manade-cp4 and manade-cp6 were replaced
// 300 times, alternating two element sets, while a reader listed the live
// set and never found it empty: their elements, counters and ipset's Size in
// memory differ from the load's listing, and Compare reads the same rules.
func TestCompareReadsAReplacementAsUnchanged(t *testing.T) {
	t.Parallel()
	loaded, replaced := readBack(t, "enrolled"), ReadBack{
		NFT:    readFixture(t, "enrolled-replaced.nft.json"),
		IPv4:   readFixture(t, "enrolled-replaced.v4.save"),
		IPv6:   readFixture(t, "enrolled.v6.save"),
		IPSets: readFixture(t, "enrolled-replaced.ipset.txt"),
	}
	replaced.NFT = edit(t, replaced.NFT, `"right": "@manade-cp4"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "saddr"}}, "right": "@manade-probe4"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "@manade-target4"}}, {"counter": {"packets": 0, "bytes": 0}}`,
		`"right": "@manade-cp4"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "saddr"}}, "right": "@manade-probe4"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "@manade-target4"}}, {"counter": {"packets": 7, "bytes": 420}}`)
	for _, differs := range []struct {
		name           string
		loaded, change []byte
		was, now       string
	}{
		{"nftElements", loaded.NFT, replaced.NFT, `"elem": ["192.168.1.80"]`, `"192.168.1.81"`},
		{"ipsetElements", loaded.IPSets, replaced.IPSets, "Number of entries: 1", "Number of entries: 2"},
		{"ipsetSize", loaded.IPSets, replaced.IPSets, "Size in memory: 256", "Size in memory: 296"},
	} {
		if !strings.Contains(string(differs.loaded), differs.was) || !strings.Contains(string(differs.change), differs.now) {
			t.Fatalf("%s: the fixtures no longer differ as this test needs", differs.name)
		}
	}
	for _, be := range bothBackends {
		if err := generate(t, "enrolled", be).Compare(&replaced); err != nil {
			t.Errorf("backend %d: %v", be, err)
		}
	}
}

// A listing that lost an exception set, or whose marked accept moved off
// its port, differs.
func TestCompareNoticesAnExceptionChange(t *testing.T) {
	t.Parallel()
	rb := readBack(t, "enrolled")
	for _, tc := range []struct {
		name string
		rb   ReadBack
		be   Backend
		want string
	}{
		{"nftSetMissing", ReadBack{NFT: edit(t, rb.NFT, `{"set": {"family": "inet", "name": "manade-cpjoin6", "table": "manade", "type": "ipv6_addr", "handle": 11}}, `, "")}, NFTables, "set manade-cpjoin6 is missing"},
		{"nftMarkedPort", ReadBack{NFT: edit(t, rb.NFT, `"right": "@manade-cplast6"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": 7464}}`,
			`"right": "@manade-cplast6"}}, {"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": 7465}}`)}, NFTables, "chain output: rule 6 is"},
		{"nftUnmarked", ReadBack{NFT: edit(t, rb.NFT, `{"match": {"op": "==", "left": {"meta": {"key": "mark"}}, "right": 19790}}, {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "@manade-cp4"}}`,
			`{"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "@manade-cp4"}}`)}, NFTables, "chain output: rule 3 is"},
		{"ipsetMissing", ReadBack{IPv4: rb.IPv4, IPv6: rb.IPv6, IPSets: []byte(strings.Replace(string(rb.IPSets), "Name: manade-cpjoin4", "Name: manade-other", 1))}, IPTables, "ipset manade-cpjoin4 is missing"},
		{"iptMarkDropped", ReadBack{IPv4: edit(t, rb.IPv4, "--dport 7464 -m mark --mark 0x4d4e -m set --match-set manade-cp4 dst", "--dport 7464 -m set --match-set manade-cp4 dst"), IPv6: rb.IPv6, IPSets: rb.IPSets}, IPTables, "chain MANADE-OUT: rule 3 is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := generate(t, "enrolled", tc.be).Compare(&tc.rb); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Compare = %v, want %q", err, tc.want)
			}
		})
	}
}
