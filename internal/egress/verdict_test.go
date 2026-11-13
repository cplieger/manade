package egress

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// withSaveCounters writes rb's iptables-save listings as -c lists them, each
// rule at 0 packets but those of counts, keyed by the line after "-A ",
// each of which must occur once across both listings.
func withSaveCounters(t *testing.T, rb *ReadBack, counts map[string]uint64) {
	t.Helper()
	seen := map[string]int{}
	write := func(listing []byte) []byte {
		var b strings.Builder
		for line := range strings.Lines(string(listing)) {
			rest, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "-A ")
			if !ok {
				b.WriteString(line)
				continue
			}
			n, ok := counts[rest]
			if ok {
				seen[rest]++
			}
			b.WriteString("[" + strconv.FormatUint(n, 10) + ":" + strconv.FormatUint(n*60, 10) + "] -A " + rest + "\n")
		}
		return []byte(b.String())
	}
	rb.IPv4, rb.IPv6 = write(rb.IPv4), write(rb.IPv6)
	for k := range counts {
		if seen[k] != 1 {
			t.Fatalf("%q occurs %d times in the listings, want 1", k, seen[k])
		}
	}
}

// nftCount is a counter to set in an nft listing: on the one rule of chain
// that holds a counter and whose expressions hold every string of has.
type nftCount struct {
	chain   string
	has     []string
	packets uint64
}

func withNFTCounters(t *testing.T, listing []byte, counts ...nftCount) []byte {
	t.Helper()
	var doc struct {
		Nftables []map[string]map[string]any `json:"nftables"`
	}
	if err := json.Unmarshal(listing, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range counts {
		n := 0
		for _, obj := range doc.Nftables {
			r, ok := obj["rule"]
			if !ok || r["chain"] != c.chain {
				continue
			}
			text, _ := json.Marshal(r["expr"])
			if !strings.Contains(string(text), `"counter"`) || !containsAll(string(text), c.has) {
				continue
			}
			for _, e := range r["expr"].([]any) {
				if ctr, ok := e.(map[string]any)["counter"]; ok {
					ctr.(map[string]any)["packets"] = c.packets
					ctr.(map[string]any)["bytes"] = c.packets * 60
				}
			}
			n++
		}
		if n != 1 {
			t.Fatalf("%v matches %d counting rules of chain %s, want 1", c.has, n, c.chain)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// The probe's counting rules in the iptables listings, as the kernel lists
// them (testdata/readback).
const (
	iptFwdBridge4  = "MANADE-FWD -i manade+ -o manade+ -m set --match-set manade-probe4 src -m set --match-set manade-target4 dst"
	iptFwdPrivate4 = "MANADE-FWD -d 192.168.0.0/16 -i manade+ -m set --match-set manade-probe4 src -m set --match-set manade-target4 dst"
	iptFwdAfter4   = "MANADE-FWD -m set --match-set manade-probe4 src -m set --match-set manade-target4 dst"
	iptInBridge4   = "MANADE-IN -i manade+ -m set --match-set manade-probe4 src -m set --match-set manade-target4 dst"
	iptOutAfter4   = "MANADE-OUT -m set --match-set manade-probe4 src -m set --match-set manade-target4 dst"
	iptFwdSelf64   = "MANADE-FWD -d 2a01:e0a:1259:2710::/64 -i manade+ -m set --match-set manade-probe6 src -m set --match-set manade-target6 dst"
	iptFwdAfter6   = "MANADE-FWD -m set --match-set manade-probe6 src -m set --match-set manade-target6 dst"
	// Rules that are not the probe's, which the host's own traffic counts.
	iptFwdPrivDrop = "MANADE-FWD -d 192.168.0.0/16 -i manade+ -j DROP"
	iptOutDrop     = "MANADE-OUT -d 192.168.0.0/16 ! -o manade+ -j DROP"
	iptOutResolver = "MANADE-OUT -d 1.1.1.1/32 -p udp -m udp --dport 53 -j RETURN"
	iptOutAllow    = "MANADE-OUT -d 10.0.0.50/32 -p tcp -m tcp --dport 5432 -j RETURN"
)

// The same rules in the nft listings, by what their expressions hold.
var (
	nftFwdBridge4 = nftCount{chain: "forward", has: []string{`"iifname"`, `"oifname"`, "@manade-probe4"}}
	nftFwdAfter4  = nftCount{chain: "forward", has: []string{`[{"match":{"left":{"payload":{"field":"saddr","protocol":"ip"}}`, "@manade-probe4"}}
	nftFwdPrivate = nftCount{chain: "forward", has: []string{`"iifname"`, "192.168.0.0", "@manade-probe4"}}
	nftInBridge4  = nftCount{chain: "input", has: []string{`"iifname"`, "@manade-probe4"}}
	nftFwdSelf64  = nftCount{chain: "forward", has: []string{"2a01:e0a:1259:2710::", "@manade-probe6"}}
	nftFwdAfter6  = nftCount{chain: "forward", has: []string{`[{"match":{"left":{"payload":{"field":"saddr","protocol":"ip6"}}`, "@manade-probe6"}}
)

func counting(c nftCount, packets uint64) nftCount {
	c.packets = packets
	return c
}

func TestProbeCounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, scenario string
		ipt            map[string]uint64
		nft            []nftCount
		want           ProbeCounts
	}{
		{
			// The job-to-job check: manade's bridge-to-bridge drop counts
			// the packet ahead of Docker's inter-container drop.
			name: "a bridge-to-bridge drop", scenario: "internet",
			ipt:  map[string]uint64{iptFwdBridge4: 3},
			nft:  []nftCount{counting(nftFwdBridge4, 3)},
			want: ProbeCounts{Dropped: 3},
		},
		{
			name: "a private destination", scenario: "internet",
			ipt:  map[string]uint64{iptFwdPrivate4: 1},
			nft:  []nftCount{counting(nftFwdPrivate, 1)},
			want: ProbeCounts{Dropped: 1},
		},
		{
			name: "the job network's gateway", scenario: "internet",
			ipt:  map[string]uint64{iptInBridge4: 2},
			nft:  []nftCount{counting(nftInBridge4, 2)},
			want: ProbeCounts{Dropped: 2},
		},
		{
			name: "a packet past every drop", scenario: "internet",
			ipt:  map[string]uint64{iptFwdAfter4: 1, iptOutAfter4: 2},
			nft:  []nftCount{counting(nftFwdAfter4, 3)},
			want: ProbeCounts{Passed: 3},
		},
		{
			name: "an IPv6 block counted through the IPv6 sets", scenario: "ipv6",
			ipt:  map[string]uint64{iptFwdSelf64: 4},
			nft:  []nftCount{counting(nftFwdSelf64, 4)},
			want: ProbeCounts{Dropped: 4},
		},
		{
			name: "both families", scenario: "ipv6",
			ipt:  map[string]uint64{iptFwdBridge4: 1, iptFwdSelf64: 2, iptFwdAfter6: 5},
			nft:  []nftCount{counting(nftFwdBridge4, 1), counting(nftFwdSelf64, 2), counting(nftFwdAfter6, 5)},
			want: ProbeCounts{Dropped: 3, Passed: 5},
		},
		{
			// The host's own traffic during a check (a pull, a lookup, a
			// job's reply) counts the drops and accepts themselves, never
			// a probe counter, which matches only the probe's sets.
			name: "the host's own traffic", scenario: "internet",
			ipt: map[string]uint64{
				iptFwdBridge4: 3, iptFwdPrivDrop: 40, iptOutDrop: 7, iptOutResolver: 12, iptOutAllow: 900,
			},
			nft:  []nftCount{counting(nftFwdBridge4, 3)},
			want: ProbeCounts{Dropped: 3},
		},
		{name: "nothing counted", scenario: "internet-off", ipt: map[string]uint64{}},
	} {
		t.Run(tc.name+"_iptables", func(t *testing.T) {
			t.Parallel()
			rb := readBack(t, tc.scenario)
			withSaveCounters(t, &rb, tc.ipt)
			got, err := generate(t, tc.scenario, IPTables).ProbeCounts(&rb)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ProbeCounts = %+v, want %+v", got, tc.want)
			}
		})
		t.Run(tc.name+"_nft", func(t *testing.T) {
			t.Parallel()
			rb := readBack(t, tc.scenario)
			rb.NFT = withNFTCounters(t, rb.NFT, tc.nft...)
			got, err := generate(t, tc.scenario, NFTables).ProbeCounts(&rb)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ProbeCounts = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProbeCountsRefusesRulesThatDiffer(t *testing.T) {
	t.Parallel()
	t.Run("iptables", func(t *testing.T) {
		t.Parallel()
		rb := readBack(t, "internet")
		rb.IPv4 = edit(t, rb.IPv4, "-A "+iptFwdAfter4+"\n", "")
		if _, err := generate(t, "internet", IPTables).ProbeCounts(&rb); err == nil {
			t.Error("a listing without a counting rule was read")
		}
	})
	t.Run("nft", func(t *testing.T) {
		t.Parallel()
		rb := readBack(t, "internet")
		rb.NFT = []byte(`{"nftables": []}`)
		if _, err := generate(t, "internet", NFTables).ProbeCounts(&rb); err == nil {
			t.Error("a listing without manade's table was read")
		}
	})
	t.Run("open", func(t *testing.T) {
		t.Parallel()
		rb := readBack(t, "open")
		if _, err := generate(t, "open", NFTables).ProbeCounts(&rb); err == nil {
			t.Error("an open host's counters were read")
		}
	})
}

func TestProbeCountsSince(t *testing.T) {
	t.Parallel()
	before := ProbeCounts{Dropped: 10, Passed: 4}
	if got, want := (ProbeCounts{Dropped: 13, Passed: 4}).Since(before), (ProbeCounts{Dropped: 3}); got != want {
		t.Errorf("Since = %+v, want %+v", got, want)
	}
	// A restore in between zeroed the counters: what they hold now is all
	// the check counted.
	if got, want := (ProbeCounts{Dropped: 2, Passed: 1}).Since(before), (ProbeCounts{Dropped: 2, Passed: 1}); got != want {
		t.Errorf("Since across a restore = %+v, want %+v", got, want)
	}
}

func TestJudge(t *testing.T) {
	t.Parallel()
	check := func(k Kind) *Check {
		return &Check{Name: "the host's outward gateway", Addr: "172.19.0.1:1", Kind: k, Leg: LegJob}
	}
	dropped := ProbeCounts{Dropped: 2}
	for _, tc := range []struct {
		name     string
		kind     Kind
		outcome  Outcome
		counted  ProbeCounts
		pass     bool
		unproven bool
	}{
		{name: "a block whose drop counted its timeout", kind: KindBlock, outcome: OutcomeTimeout, counted: dropped, pass: true},
		{name: "a block timing out uncounted", kind: KindBlock, outcome: OutcomeTimeout, unproven: true},
		{name: "a block counted past the drops", kind: KindBlock, outcome: OutcomeTimeout, counted: ProbeCounts{Dropped: 2, Passed: 1}},
		{name: "a block only counted past the drops", kind: KindBlock, outcome: OutcomeTimeout, counted: ProbeCounts{Passed: 1}},
		{name: "a block refused", kind: KindBlock, outcome: OutcomeRefused, counted: dropped},
		{name: "a block answered", kind: KindBlock, outcome: OutcomeConnected, counted: dropped},
		{name: "a block reset", kind: KindBlock, outcome: OutcomeReset, counted: dropped},
		{name: "a block unreachable", kind: KindBlock, outcome: OutcomeNoRoute, counted: dropped},
		{name: "a block with another error", kind: KindBlock, outcome: OutcomeError, counted: dropped},
		{name: "a reach that connects", kind: KindReach, outcome: OutcomeConnected, pass: true},
		{name: "a reach that times out", kind: KindReach, outcome: OutcomeTimeout},
		{name: "a reach refused", kind: KindReach, outcome: OutcomeRefused},
		{name: "a lookup that resolves", kind: KindResolve, outcome: OutcomeResolved, pass: true},
		{name: "a lookup that fails", kind: KindResolve, outcome: OutcomeLookupFailed},
		{name: "a lookup that times out", kind: KindResolve, outcome: OutcomeTimeout},
		{name: "no IPv6, no route", kind: KindNoIPv6, outcome: OutcomeNoRoute, pass: true},
		{name: "no IPv6, a timeout", kind: KindNoIPv6, outcome: OutcomeTimeout, pass: true},
		{name: "no IPv6, but it connects", kind: KindNoIPv6, outcome: OutcomeConnected},
		{name: "no IPv6, but it is refused", kind: KindNoIPv6, outcome: OutcomeRefused},
		{name: "an allow entry reached", kind: KindTry, outcome: OutcomeConnected, pass: true},
		{name: "an allow entry down", kind: KindTry, outcome: OutcomeTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := Judge(check(tc.kind), tc.outcome, tc.counted)
			if v.Pass != tc.pass {
				t.Errorf("Pass = %v, want %v (%s)", v.Pass, tc.pass, v.Reason)
			}
			if v.Unproven != tc.unproven {
				t.Errorf("Unproven = %v, want %v", v.Unproven, tc.unproven)
			}
			if v.Required != (tc.kind != KindTry) {
				t.Errorf("Required = %v for kind %d", v.Required, tc.kind)
			}
			switch {
			case v.Pass && v.Reason != "":
				t.Errorf("a passed check has the reason %q", v.Reason)
			case !v.Pass && !strings.Contains(v.Reason, "172.19.0.1:1"):
				t.Errorf("the reason %q does not name the target", v.Reason)
			}
		})
	}
	t.Run("an unproven reason says so", func(t *testing.T) {
		t.Parallel()
		if v := Judge(check(KindBlock), OutcomeTimeout, ProbeCounts{}); !strings.Contains(v.Reason, "unproven") {
			t.Errorf("Reason = %q, want it to say unproven", v.Reason)
		}
	})
}
