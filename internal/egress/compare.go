package egress

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// ReadBack is what the host read from the kernel after loading a rule set.
type ReadBack struct {
	// NFT is `nft -j list ruleset`.
	NFT []byte
	// IPv4 and IPv6 are iptables-save and ip6tables-save of every table.
	IPv4, IPv6 []byte
	// IPSets is `ipset list -t`.
	IPSets []byte
}

// Compare reports how the rules rb shows differ from rs, or nil when they
// match: tables, base chains with their hook, priority and policy, every
// rule in order, the jumps' positions and the sets' definitions. It never
// compares set elements or counters, which change while the rules hold.
func (rs *Ruleset) Compare(rb *ReadBack) error {
	var errs []error
	if rs.Backend == NFTables {
		errs = rs.compareNFT(rb.NFT)
	} else {
		errs = rs.compareIPT(rb)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("egress: the loaded rules differ: %w", errors.Join(errs...))
}

// nftChainDef is a base chain's definition as nft's JSON lists it.
type nftChainDef struct {
	Type   string `json:"type"`
	Hook   string `json:"hook"`
	Policy string `json:"policy"`
	Prio   int    `json:"prio"`
}

type nftSetDef struct {
	Type  string
	Flags []string
	Size  int
}

// nftTableState is one of manade's tables as read back.
type nftTableState struct {
	chains map[string]nftChainDef
	rules  map[string][]string
	sets   map[string]nftSetDef
}

func (rs *Ruleset) compareNFT(data []byte) []error {
	tables, err := parseNFT(data)
	if err != nil {
		return []error{err}
	}
	var errs []error
	want := map[string]*nftTableState{}
	if !rs.open {
		want["inet "+nftTable] = rs.nftExpected()
		if rs.nat != nil {
			want["ip6 "+nftNATTable] = &nftTableState{
				chains: map[string]nftChainDef{hookPostrouting: {Type: "nat", Hook: hookPostrouting, Prio: nftSrcNAT, Policy: verdictAccept}},
				rules:  map[string][]string{hookPostrouting: {rs.nat.canon()}},
			}
		}
	}
	for _, name := range []string{"inet " + nftTable, "ip6 " + nftNATTable} {
		got, w := tables[name], want[name]
		switch {
		case got == nil && w != nil:
			errs = append(errs, fmt.Errorf("table %s is missing", name))
		case got != nil && w == nil:
			errs = append(errs, fmt.Errorf("table %s is loaded where none belongs", name))
		case got != nil:
			errs = append(errs, compareTable(name, got, w)...)
		}
	}
	return errs
}

func (rs *Ruleset) nftExpected() *nftTableState {
	t := &nftTableState{chains: map[string]nftChainDef{}, rules: map[string][]string{}, sets: map[string]nftSetDef{}}
	for _, c := range rs.chains {
		name := nftChainNames[c.hook]
		t.chains[name] = nftChainDef{Type: tableFilter, Hook: c.hook, Prio: nftPriority, Policy: verdictAccept}
		t.rules[name] = []string{}
		for i := range c.rules {
			t.rules[name] = append(t.rules[name], c.rules[i].canon())
		}
	}
	for i := range rs.sets {
		s := &rs.sets[i]
		d := nftSetDef{Type: s.nftType()}
		if s.timeout {
			d.Flags, d.Size = []string{"timeout"}, learnedMax
		}
		t.sets[s.name] = d
	}
	return t
}

func compareTable(table string, got, want *nftTableState) []error {
	var errs []error
	for _, name := range sortedKeys(got.chains, want.chains) {
		g, gok := got.chains[name]
		w, wok := want.chains[name]
		switch {
		case !gok:
			errs = append(errs, fmt.Errorf("table %s: chain %s is missing", table, name))
			continue
		case !wok:
			errs = append(errs, fmt.Errorf("table %s: chain %s does not belong", table, name))
			continue
		case g != w:
			errs = append(errs, fmt.Errorf("table %s: chain %s is type %s hook %s priority %d policy %s, want type %s hook %s priority %d policy %s",
				table, name, g.Type, g.Hook, g.Prio, g.Policy, w.Type, w.Hook, w.Prio, w.Policy))
		}
		if err := compareRules("table "+table+" chain "+name, got.rules[name], want.rules[name]); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range sortedKeys(got.sets, want.sets) {
		g, gok := got.sets[name]
		w, wok := want.sets[name]
		switch {
		case !gok:
			errs = append(errs, fmt.Errorf("table %s: set %s is missing", table, name))
		case !wok:
			errs = append(errs, fmt.Errorf("table %s: set %s does not belong", table, name))
		case g.Type != w.Type || g.Size != w.Size || !slices.Equal(g.Flags, w.Flags):
			errs = append(errs, fmt.Errorf("table %s: set %s is %s flags [%s] size %d, want %s flags [%s] size %d",
				table, name, g.Type, strings.Join(g.Flags, ","), g.Size, w.Type, strings.Join(w.Flags, ","), w.Size))
		}
	}
	return errs
}

// compareRules names the first rule where got and want part.
func compareRules(where string, got, want []string) error {
	for i := range max(len(got), len(want)) {
		switch {
		case i >= len(got):
			return fmt.Errorf("%s: rule %d is missing: want %s", where, i+1, want[i])
		case i >= len(want):
			return fmt.Errorf("%s: rule %d does not belong: %s", where, i+1, got[i])
		case got[i] != want[i]:
			return fmt.Errorf("%s: rule %d is %s, want %s", where, i+1, got[i], want[i])
		}
	}
	return nil
}

func sortedKeys[V any](a, b map[string]V) []string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// canon is the rule as canonRule reads it back.
func (r *rule) canon() string {
	var parts []string
	for _, m := range r.matches() {
		parts = append(parts, m.canon())
	}
	return strings.Join(append(parts, r.action.nft()), "; ")
}

// parseNFT reads manade's tables out of `nft -j list ruleset`, keyed
// "<family> <name>"; every other table is left out.
func parseNFT(data []byte) (map[string]*nftTableState, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("reading nft's JSON: %w", err)
	}
	tables := map[string]*nftTableState{}
	for _, obj := range doc.Nftables {
		for kind, raw := range obj {
			if err := addNFTObject(tables, kind, raw); err != nil {
				return nil, err
			}
		}
	}
	return tables, nil
}

// addNFTObject records one listed object in tables when it is one of
// manade's.
func addNFTObject(tables map[string]*nftTableState, kind string, raw json.RawMessage) error {
	var hdr struct{ Family, Table, Name, Chain string }
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return fmt.Errorf("reading nft's JSON %s: %w", kind, err)
	}
	table := hdr.Table
	if kind == "table" {
		table = hdr.Name
	}
	key := hdr.Family + " " + table
	if key != "inet "+nftTable && key != "ip6 "+nftNATTable {
		return nil
	}
	t := tables[key]
	if t == nil {
		t = &nftTableState{chains: map[string]nftChainDef{}, rules: map[string][]string{}, sets: map[string]nftSetDef{}}
		tables[key] = t
	}
	switch kind {
	case "chain":
		var c nftChainDef
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("reading nft's JSON chain: %w", err)
		}
		t.chains[hdr.Name] = c
		if t.rules[hdr.Name] == nil {
			t.rules[hdr.Name] = []string{}
		}
	case "rule":
		t.rules[hdr.Chain] = append(t.rules[hdr.Chain], canonRule(raw))
	case "set":
		t.sets[hdr.Name] = parseSetDef(raw)
	}
	return nil
}

func parseSetDef(raw json.RawMessage) nftSetDef {
	var s struct {
		Type  any `json:"type"`
		Flags any `json:"flags"`
		Size  int `json:"size"`
	}
	_ = json.Unmarshal(raw, &s)
	d := nftSetDef{Size: s.Size, Type: fmt.Sprint(s.Type)}
	switch f := s.Flags.(type) {
	case string:
		d.Flags = []string{f}
	case []any:
		for _, x := range f {
			d.Flags = append(d.Flags, fmt.Sprint(x))
		}
		slices.Sort(d.Flags)
	}
	return d
}

// canonRule writes a listed rule's expressions in the form rule.canon
// writes the expected one; an expression it does not know is written raw,
// which matches no expected rule.
func canonRule(raw json.RawMessage) string {
	var r struct {
		Expr []map[string]json.RawMessage `json:"expr"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "?" + string(raw)
	}
	parts := make([]string, 0, len(r.Expr))
	for _, e := range r.Expr {
		parts = append(parts, canonExpr(e))
	}
	return strings.Join(parts, "; ")
}

func canonExpr(e map[string]json.RawMessage) string {
	if len(e) != 1 {
		return fmt.Sprint("?", e)
	}
	for kind, raw := range e {
		switch kind {
		case "counter", "accept", "drop", "masquerade":
			return kind
		case "match":
			var m struct {
				Left  map[string]map[string]string `json:"left"`
				Op    string                       `json:"op"`
				Right json.RawMessage              `json:"right"`
			}
			if json.Unmarshal(raw, &m) != nil {
				break
			}
			left, ok := canonLeft(m.Left)
			op := map[string]string{"==": "==", "in": "==", "!=": "!="}[m.Op]
			right, rok := canonRight(m.Right)
			if ok && rok && op != "" {
				return left + " " + op + " " + strings.Join(right, ",")
			}
		}
		return "?" + kind + string(raw)
	}
	return ""
}

func canonLeft(l map[string]map[string]string) (string, bool) {
	if len(l) != 1 {
		return "", false
	}
	for kind, f := range l {
		switch kind {
		case "meta", "ct":
			return kind + " " + f["key"], f["key"] != ""
		case "payload":
			return f["protocol"] + " " + f["field"], f["protocol"] != "" && f["field"] != ""
		}
	}
	return "", false
}

// canonRight writes a match's right side: a set reference or a name as
// itself, words sorted, and addresses and numbers as merged intervals, so a
// listing that collapses or orders elements its own way reads the same.
func canonRight(raw json.RawMessage) ([]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["set"]; ok {
			v = s
		}
	}
	elems, ok := v.([]any)
	if !ok {
		elems = []any{v}
	}
	var words []string
	var ivs []interval
	for _, el := range elems {
		if iv, ok := toInterval(el); ok {
			ivs = append(ivs, iv)
			continue
		}
		s, ok := el.(string)
		if !ok {
			return nil, false
		}
		words = append(words, s)
	}
	if len(words) > 0 && len(ivs) > 0 {
		return nil, false
	}
	if len(ivs) > 0 {
		return mergeIntervals(ivs), true
	}
	return sortedWords(words), true
}

func toInterval(el any) (interval, bool) {
	switch x := el.(type) {
	case json.Number:
		n, err := strconv.ParseUint(x.String(), 10, 64)
		return numInterval(n, n), err == nil
	case string:
		a, err := netip.ParseAddr(x)
		return interval{lo: a, hi: a, addr: true}, err == nil
	case map[string]any:
		if p, ok := x["prefix"].(map[string]any); ok {
			addr, _ := p["addr"].(string)
			n, _ := p["len"].(json.Number)
			bits, err := strconv.Atoi(n.String())
			a, aerr := netip.ParseAddr(addr)
			if err != nil || aerr != nil {
				return interval{}, false
			}
			pfx, perr := a.Prefix(bits)
			return prefixInterval(pfx), perr == nil
		}
		if r, ok := x["range"].([]any); ok && len(r) == 2 {
			lo, lok := toInterval(r[0])
			hi, hok := toInterval(r[1])
			if !lok || !hok || lo.addr != hi.addr {
				return interval{}, false
			}
			return interval{lo: lo.lo, hi: hi.hi, nlo: lo.nlo, nhi: hi.nhi, addr: lo.addr}, true
		}
	}
	return interval{}, false
}

// interval is an inclusive range of addresses or of numbers.
type interval struct {
	lo, hi   netip.Addr
	nlo, nhi uint64
	addr     bool
}

func numInterval(lo, hi uint64) interval { return interval{nlo: lo, nhi: hi} }

func prefixInterval(p netip.Prefix) interval {
	p = p.Masked()
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	hi, _ := netip.AddrFromSlice(b)
	return interval{lo: p.Addr(), hi: hi, addr: true}
}

// mergeIntervals sorts ivs and joins those that overlap or touch, writing
// each as "lo" or "lo-hi".
func mergeIntervals(ivs []interval) []string {
	s := slices.Clone(ivs)
	slices.SortFunc(s, compareIntervals)
	var merged []interval
	for _, iv := range s {
		if n := len(merged); n > 0 && touches(merged[n-1], iv) {
			merged[n-1] = merged[n-1].join(iv)
			continue
		}
		merged = append(merged, iv)
	}
	out := make([]string, len(merged))
	for i, iv := range merged {
		out[i] = iv.String()
	}
	return out
}

// compareIntervals orders address intervals before number intervals, each
// by its start.
func compareIntervals(a, b interval) int {
	switch {
	case a.addr && !b.addr:
		return -1
	case !a.addr && b.addr:
		return 1
	case a.addr:
		return a.lo.Compare(b.lo)
	}
	return cmp.Compare(a.nlo, b.nlo)
}

// join answers a widened to end where b ends, when b ends later.
func (a interval) join(b interval) interval {
	if a.addr && b.hi.Compare(a.hi) > 0 {
		a.hi = b.hi
	}
	if !a.addr && b.nhi > a.nhi {
		a.nhi = b.nhi
	}
	return a
}

func (a interval) String() string {
	lo, hi := strconv.FormatUint(a.nlo, 10), strconv.FormatUint(a.nhi, 10)
	if a.addr {
		lo, hi = a.lo.String(), a.hi.String()
	}
	if lo == hi {
		return lo
	}
	return lo + "-" + hi
}

// touches reports whether b, sorted after a, overlaps a or starts right
// after it.
func touches(a, b interval) bool {
	if a.addr != b.addr {
		return false
	}
	if !a.addr {
		return a.nhi == ^uint64(0) || b.nlo <= a.nhi+1
	}
	if a.lo.Is4() != b.lo.Is4() {
		return false
	}
	next := a.hi.Next()
	return !next.IsValid() || b.lo.Compare(next) <= 0
}

func sortedWords(ws []string) []string {
	s := slices.Clone(ws)
	slices.Sort(s)
	return s
}

func (rs *Ruleset) compareIPT(rb *ReadBack) []error {
	byFamily := map[bool]map[string]saved{false: parseSave(rb.IPv4), true: parseSave(rb.IPv6)}
	if rs.open {
		return compareOpenIPT(byFamily)
	}
	var errs []error
	for _, f := range []family{ipv4, ipv6} {
		v6 := f == ipv6
		for _, c := range rs.chains {
			name := iptChainNames[c.hook]
			if err := compareChain(tool(v6)+" "+tableFilter, name, byFamily[v6][tableFilter], rs.iptChain(f, name)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if rs.nat != nil {
		if err := compareChain(tool(true)+" nat", iptNAT, byFamily[true]["nat"], rs.nat.ipt(ipv6)); err != nil {
			errs = append(errs, err)
		}
	}
	jumps := append(slices.Clone(rs.Jumps), Jump{Table: tableFilter, Chain: iptDocker, Target: iptFwd})
	for _, j := range jumps {
		rules := byFamily[j.IPv6][j.Table][j.Chain]
		if len(rules) == 0 || rules[0] != "-j "+j.Target {
			errs = append(errs, fmt.Errorf("%s %s %s: the jump to %s is not its first rule", tool(j.IPv6), j.Table, j.Chain, j.Target))
		}
	}
	return append(errs, rs.compareIPSets(ipsetDefs(rb.IPSets))...)
}

// compareOpenIPT checks that an open host's chains hold no rule.
func compareOpenIPT(byFamily map[bool]map[string]saved) []error {
	var errs []error
	for _, f := range []family{ipv4, ipv6} {
		for _, name := range ownChains(f) {
			if rules := byFamily[f == ipv6][tableFilter][name]; len(rules) > 0 {
				errs = append(errs, fmt.Errorf("%s filter %s holds rules on an open host: %s", tool(f == ipv6), name, rules[0]))
			}
		}
	}
	return errs
}

func (rs *Ruleset) compareIPSets(defs map[string]ipsetDef) []error {
	var errs []error
	for i := range rs.sets {
		s := &rs.sets[i]
		got, ok := defs[s.name]
		switch want := s.ipsetDef(); {
		case !ok:
			errs = append(errs, fmt.Errorf("ipset %s is missing", s.name))
		case got != want:
			errs = append(errs, fmt.Errorf("ipset %s is %s family %s maxelem %s timeout %q, want %s family %s maxelem %s timeout %q",
				s.name, got.typ, got.family, got.maxelem, got.timeout, want.typ, want.family, want.maxelem, want.timeout))
		}
	}
	return errs
}

func compareChain(where, name string, table saved, want []string) error {
	got, ok := table[name]
	if !ok {
		return fmt.Errorf("%s: chain %s is missing", where, name)
	}
	return compareRules(where+" chain "+name, got, want)
}

// iptChain answers chain name's rules under family f, after "-A <name> ".
func (rs *Ruleset) iptChain(f family, name string) []string {
	out := []string{}
	for _, l := range rs.iptLines(f) {
		if r, ok := strings.CutPrefix(l, "-A "+name+" "); ok {
			out = append(out, r)
		}
	}
	return out
}

func tool(v6 bool) string {
	if v6 {
		return "ip6tables"
	}
	return "iptables"
}
