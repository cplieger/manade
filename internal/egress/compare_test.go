package egress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/readback holds what Linux 6.12 listed in docker:29-dind after
// each golden was loaded twice: nft -j, iptables-save, ipset list -t.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "readback", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readBack(t *testing.T, sc string) ReadBack {
	t.Helper()
	return ReadBack{
		NFT:    readFixture(t, sc+".nft.json"),
		IPv4:   readFixture(t, sc+".v4.save"),
		IPv6:   readFixture(t, sc+".v6.save"),
		IPSets: readFixture(t, sc+".ipset.txt"),
	}
}

func generate(t *testing.T, name string, b Backend) *Ruleset {
	t.Helper()
	for _, sc := range scenarios() {
		if sc.name == name {
			rs, err := Generate(&sc.in, b)
			if err != nil {
				t.Fatal(err)
			}
			return rs
		}
	}
	t.Fatalf("no scenario %s", name)
	return nil
}

// edit replaces old with new in b exactly once, so a fixture edit that
// stops matching fails the test instead of passing it vacuously.
func edit(t *testing.T, b []byte, old, new string) []byte {
	t.Helper()
	if n := strings.Count(string(b), old); n != 1 {
		t.Fatalf("%q occurs %d times in the fixture, want 1", old, n)
	}
	return []byte(strings.Replace(string(b), old, new, 1))
}

func TestCompareAcceptsTheKernelsListing(t *testing.T) {
	t.Parallel()
	for _, sc := range scenarios() {
		for _, b := range []Backend{NFTables, IPTables} {
			t.Run(sc.name+"_"+map[Backend]string{NFTables: "nft", IPTables: "iptables"}[b], func(t *testing.T) {
				t.Parallel()
				if err := generate(t, sc.name, b).Compare(new(readBack(t, sc.name))); err != nil {
					t.Error(err)
				}
			})
		}
	}
}

func TestCompareNFT(t *testing.T) {
	t.Parallel()
	v6 := readFixture(t, "ipv6.nft.json")
	off := readFixture(t, "ipv6-internet-off.nft.json")
	forwardChain := `"name": "forward", "handle": 1, "type": "filter", "hook": "forward", "prio": -10, "policy": "accept"`
	for _, tc := range []struct {
		name, sc string
		data     []byte
		want     string
	}{
		{
			"priorityZero", "ipv6", edit(t, v6, forwardChain, strings.Replace(forwardChain, `"prio": -10`, `"prio": 0`, 1)),
			"chain forward is type filter hook forward priority 0 policy accept, want type filter hook forward priority -10",
		},
		{
			"otherHook", "ipv6", edit(t, v6, forwardChain, strings.Replace(forwardChain, `"hook": "forward"`, `"hook": "prerouting"`, 1)),
			"chain forward is type filter hook prerouting",
		},
		{
			"policyDrop", "ipv6", edit(t, v6, forwardChain, strings.Replace(forwardChain, `"policy": "accept"`, `"policy": "drop"`, 1)),
			"policy drop",
		},
		{"natTableMissing", "ipv6", stripNAT(t, v6, true), "table ip6 manade-nat is missing"},
		{"natRuleMissing", "ipv6", stripNAT(t, v6, false), "table ip6 manade-nat chain postrouting: rule 1 is missing"},
		{
			"ruleChanged", "ipv6", edit(t, v6, `"right": "1.1.1.1"`, `"right": "1.0.0.1"`),
			"table inet manade chain output: rule 4 is",
		},
		{
			"setTypeChanged", "ipv6-internet-off", edit(t, off, `"name": "manade-gh4", "table": "manade", "type": "ipv4_addr", "handle": 8, "size": 65536`,
				`"name": "manade-gh4", "table": "manade", "type": "ipv4_addr", "handle": 8, "size": 1024`),
			"set manade-gh4 is ipv4_addr flags [timeout] size 1024, want ipv4_addr flags [timeout] size 65536",
		},
		{"closedHostOpened", "ipv6", readFixture(t, "open.nft.json"), "table inet manade is missing"},
		{"openHostClosed", "open", v6, "table inet manade is loaded where none belongs"},
		{"notJSON", "ipv6", []byte("table inet manade {"), "reading nft's JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := generate(t, tc.sc, NFTables).Compare(&ReadBack{NFT: tc.data})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Compare = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	t.Run("setElementsAndCountersChanged", func(t *testing.T) {
		t.Parallel()
		data := edit(t, off, `{"elem": {"val": "140.82.112.3", "timeout": 3600, "expires": 3599}}, `, "")
		data = edit(t, data, `"val": "2606:50c0:8000::153", "timeout": 1200, "expires": 1199`, `"val": "2606:50c0:8000::154", "timeout": 600, "expires": 7`)
		data = []byte(strings.ReplaceAll(string(data), `"counter": {"packets": 0, "bytes": 0}`, `"counter": {"packets": 12, "bytes": 720}`))
		if err := generate(t, "ipv6-internet-off", NFTables).Compare(&ReadBack{NFT: data}); err != nil {
			t.Error(err)
		}
	})
	t.Run("closedThenOpen", func(t *testing.T) {
		t.Parallel()
		if err := generate(t, "open", NFTables).Compare(&ReadBack{NFT: readFixture(t, "ipv6-then-open.nft.json")}); err != nil {
			t.Error(err)
		}
	})
}

// stripNAT drops the masquerade rule from an nft listing, and with table
// its table and chain too.
func stripNAT(t *testing.T, data []byte, table bool) []byte {
	t.Helper()
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	kept := doc.Nftables[:0]
	dropped := 0
	for _, obj := range doc.Nftables {
		var hdr struct{ Family, Table, Name string }
		kind := ""
		for k, raw := range obj {
			kind = k
			_ = json.Unmarshal(raw, &hdr)
		}
		natTable := hdr.Family == "ip6" && (hdr.Table == nftNATTable || (kind == "table" && hdr.Name == nftNATTable))
		if natTable && (kind == "rule" || table) {
			dropped++
			continue
		}
		kept = append(kept, obj)
	}
	if want := map[bool]int{false: 1, true: 3}[table]; dropped != want {
		t.Fatalf("dropped %d objects from the fixture, want %d", dropped, want)
	}
	doc.Nftables = kept
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCompareIPTables(t *testing.T) {
	t.Parallel()
	rb := readBack(t, "ipv6")
	off := readBack(t, "ipv6-internet-off")
	for _, tc := range []struct {
		name, sc string
		rb       ReadBack
		want     string
	}{
		{
			"outputJumpMoved", "ipv6", withV4(rb, edit(t, rb.IPv4, "-A OUTPUT -j MANADE-OUT\n", "-A OUTPUT -o lo -j ACCEPT\n-A OUTPUT -j MANADE-OUT\n")),
			"iptables filter OUTPUT: the jump to MANADE-OUT is not its first rule",
		},
		{
			"forwardJumpMissing", "ipv6", withV4(rb, edit(t, rb.IPv4, "-A FORWARD -j DOCKER-USER\n", "")),
			"iptables filter FORWARD: the jump to DOCKER-USER is not its first rule",
		},
		{
			"dockerUserJumpMoved", "ipv6", withV4(rb, edit(t, rb.IPv4, "-A DOCKER-USER -j MANADE-FWD\n", "-A DOCKER-USER -j RETURN\n-A DOCKER-USER -j MANADE-FWD\n")),
			"iptables filter DOCKER-USER: the jump to MANADE-FWD is not its first rule",
		},
		{
			"ip6ForwardJumpMoved", "ipv6", withV6(rb, edit(t, rb.IPv6, "-A FORWARD -j MANADE-FWD\n", "-A FORWARD -j ACCEPT\n-A FORWARD -j MANADE-FWD\n")),
			"ip6tables filter FORWARD: the jump to MANADE-FWD is not its first rule",
		},
		{
			"natRuleMissing", "ipv6", withV6(rb, edit(t, rb.IPv6, "-A MANADE-NAT -s fdc6:6d61:6e61:6465::/64 ! -o manade+ -j MASQUERADE\n", "")),
			"ip6tables nat chain MANADE-NAT: rule 1 is missing",
		},
		{
			"natJumpMissing", "ipv6", withV6(rb, edit(t, rb.IPv6, "-A POSTROUTING -j MANADE-NAT\n", "")),
			"ip6tables nat POSTROUTING: the jump to MANADE-NAT is not its first rule",
		},
		{
			"ruleRemoved", "ipv6", withV4(rb, edit(t, rb.IPv4, "-A MANADE-IN -i manade+ -j DROP\n", "")),
			"iptables filter chain MANADE-IN: rule 4 is",
		},
		{
			"chainMissing", "ipv6", withV6(rb, []byte("*filter\n:INPUT ACCEPT [0:0]\nCOMMIT\n")),
			"ip6tables filter: chain MANADE-FWD is missing",
		},
		{
			"ipsetMissing", "ipv6-internet-off", withSets(off, edit(t, off.IPSets, "Name: manade-gh6", "Name: other")),
			"ipset manade-gh6 is missing",
		},
		{
			"ipsetMaxelem", "ipv6-internet-off", withSets(off, edit(t, off.IPSets, "family inet hashsize 1024 maxelem 65536 timeout 600", "family inet hashsize 1024 maxelem 1024 timeout 600")),
			"ipset manade-gh4 is hash:ip family inet maxelem 1024",
		},
		{"closedHostOpened", "ipv6", readBack(t, "open"), "chain MANADE-IN: rule 1 is missing"},
		{"openHostClosed", "open", rb, "iptables filter MANADE-FWD holds rules on an open host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := generate(t, tc.sc, IPTables).Compare(&tc.rb)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Compare = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	t.Run("setElementsAndCountersChanged", func(t *testing.T) {
		t.Parallel()
		sets := edit(t, off.IPSets, "Number of entries: 2", "Number of entries: 9")
		v4 := withCounters(off.IPv4)
		if err := generate(t, "ipv6-internet-off", IPTables).Compare(&ReadBack{IPv4: v4, IPv6: off.IPv6, IPSets: sets}); err != nil {
			t.Error(err)
		}
	})
	t.Run("closedThenOpen", func(t *testing.T) {
		t.Parallel()
		rb := ReadBack{IPv4: readFixture(t, "internet-then-open.v4.save"), IPv6: readFixture(t, "open.v6.save")}
		if err := generate(t, "open", IPTables).Compare(&rb); err != nil {
			t.Error(err)
		}
	})
}

func withV4(rb ReadBack, b []byte) ReadBack   { rb.IPv4 = b; return rb }
func withV6(rb ReadBack, b []byte) ReadBack   { rb.IPv6 = b; return rb }
func withSets(rb ReadBack, b []byte) ReadBack { rb.IPSets = b; return rb }

// withCounters rewrites a save as `iptables-save -c` writes it, each rule
// prefixed with nonzero counters.
func withCounters(b []byte) []byte {
	var out strings.Builder
	for line := range strings.Lines(string(b)) {
		if strings.HasPrefix(line, "-A ") {
			out.WriteString("[7:420] ")
		}
		out.WriteString(line)
	}
	return []byte(out.String())
}
