package egress

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cplieger/manade/internal/config"
)

// The iptables chains. Docker keeps DOCKER-USER first in FORWARD and leaves
// it alone; under ip6tables the daemon, run with --ip6tables=false, creates
// no chain, so MANADE-FWD is jumped from FORWARD there.
const (
	iptFwd       = "MANADE-FWD"
	iptOut       = "MANADE-OUT"
	iptIn        = "MANADE-IN"
	iptNAT       = "MANADE-NAT"
	iptDocker    = "DOCKER-USER"
	ipsetTimeout = 600
	tableFilter  = "filter"
)

var iptChainNames = map[string]string{hookForward: iptFwd, hookOutput: iptOut, hookInput: iptIn}

// renderIPTables writes the three payloads and the jumps. The chains are
// declared in each payload, which under --noflush empties them, so a reload
// replaces their rules in one restore; on an open host that is all it does.
func (rs *Ruleset) renderIPTables() {
	rs.IPv4 = rs.restore(ipv4)
	rs.IPv6 = rs.restore(ipv6)
	if rs.open {
		return
	}
	var ipset strings.Builder
	for _, s := range rs.sets {
		ipset.WriteString(s.ipset())
	}
	rs.IPSet = ipset.String()
	rs.Jumps = []Jump{
		{Table: tableFilter, Chain: "FORWARD", Target: iptDocker},
		{Table: tableFilter, Chain: "OUTPUT", Target: iptOut},
		{Table: tableFilter, Chain: "INPUT", Target: iptIn},
		{Table: tableFilter, Chain: "FORWARD", Target: iptFwd, IPv6: true},
		{Table: tableFilter, Chain: "OUTPUT", Target: iptOut, IPv6: true},
		{Table: tableFilter, Chain: "INPUT", Target: iptIn, IPv6: true},
	}
	if rs.nat != nil {
		rs.Jumps = append(rs.Jumps, Jump{Table: "nat", Chain: "POSTROUTING", Target: iptNAT, IPv6: true})
	}
}

// ownChains are the filter chains a payload declares under family f.
func ownChains(f family) []string {
	if f == ipv4 {
		return []string{iptFwd, iptOut, iptIn, iptDocker}
	}
	return []string{iptFwd, iptOut, iptIn}
}

func (rs *Ruleset) restore(f family) string {
	var b strings.Builder
	b.WriteString("*filter\n")
	for _, name := range ownChains(f) {
		fmt.Fprintf(&b, ":%s - [0:0]\n", name)
	}
	for _, line := range rs.iptLines(f) {
		b.WriteString(line + "\n")
	}
	b.WriteString("COMMIT\n")
	if f == ipv6 && rs.nat != nil {
		fmt.Fprintf(&b, "*nat\n:%s - [0:0]\n", iptNAT)
		for _, l := range rs.nat.ipt(ipv6) {
			fmt.Fprintf(&b, "-A %s %s\n", iptNAT, l)
		}
		b.WriteString("COMMIT\n")
	}
	return b.String()
}

// iptLines answers the filter table's rules for family f as iptables-save
// writes them.
func (rs *Ruleset) iptLines(f family) []string {
	var out []string
	for _, c := range rs.chains {
		name := iptChainNames[c.hook]
		for i := range c.rules {
			for _, l := range c.rules[i].ipt(f) {
				out = append(out, "-A "+name+" "+l)
			}
		}
	}
	if f == ipv4 && !rs.open {
		out = append(out, "-A "+iptDocker+" -j "+iptFwd)
	}
	return out
}

func (s *set) ipset() string {
	fam := "inet"
	if s.family == ipv6 {
		fam = "inet6"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "create %s hash:ip family %s", s.name, fam)
	if s.timeout {
		fmt.Fprintf(&b, " timeout %d", ipsetTimeout)
	}
	fmt.Fprintf(&b, " maxelem %d\nflush %s\n", learnedMax, s.name)
	for _, l := range s.elems {
		fmt.Fprintf(&b, "add %s %s timeout %d\n", s.name, l.Addr, seconds(l.Left))
	}
	return b.String()
}

// ipt answers the rule's lines under family f's iptables, after the chain
// name, in iptables-save's order: addresses, interfaces, protocol, then
// matches in the order written. One rule becomes a line per destination,
// per protocol and port range, and per neighbour discovery type.
func (r *rule) ipt(f family) []string {
	if mf := r.matchFamily(); mf != anyFamily && mf != f {
		return nil
	}
	lines := []string{""}
	for _, alts := range [][]string{r.iptSaddr(), r.iptDaddrs(), r.iif.ipt("-i"), r.oif.ipt("-o"), r.iptPorts(), r.iptND()} {
		lines = cross(lines, alts)
	}
	tail := r.iptTail()
	for i := range lines {
		lines[i] = strings.TrimPrefix(lines[i]+tail, " ")
	}
	return lines
}

// cross appends each of alts to each of lines; no alts leaves lines as
// they are.
func cross(lines, alts []string) []string {
	if len(alts) == 0 {
		return lines
	}
	next := make([]string, 0, len(lines)*len(alts))
	for _, l := range lines {
		for _, a := range alts {
			next = append(next, l+a)
		}
	}
	return next
}

func (r *rule) iptSaddr() []string {
	if !r.saddr.IsValid() {
		return nil
	}
	return []string{" -s " + r.saddr.String()}
}

func (r *rule) iptDaddrs() []string {
	out := make([]string, 0, len(r.daddr))
	for _, p := range r.daddr {
		d := " -d " + p.String()
		if r.notDaddr {
			d = " !" + d
		}
		out = append(out, d)
	}
	return out
}

func (r *rule) iptND() []string {
	if !r.nd {
		return nil
	}
	return []string{" -p ipv6-icmp -m icmp6 --icmpv6-type 135", " -p ipv6-icmp -m icmp6 --icmpv6-type 136"}
}

// iptTail is the conntrack and set matches and the target, which every
// line of the rule ends with.
func (r *rule) iptTail() string {
	var tail string
	if r.ct != "" {
		tail += " -m conntrack --ctstate " + map[string]string{ctEstablished: "RELATED,ESTABLISHED", ctNew: "NEW"}[r.ct]
	}
	if r.saddrSet != "" {
		tail += " -m set --match-set " + r.saddrSet + " src"
	}
	if r.daddrSet != "" {
		tail += " -m set --match-set " + r.daddrSet + " dst"
	}
	return tail + map[action]string{accept: " -j RETURN", drop: " -j DROP", masquerade: " -j MASQUERADE"}[r.action]
}

func (i iface) ipt(flag string) []string {
	if i.name == "" {
		return nil
	}
	name := i.name
	if i.glob {
		name += "+"
	}
	if i.not {
		return []string{" ! " + flag + " " + name}
	}
	return []string{" " + flag + " " + name}
}

func (r *rule) iptPorts() []string {
	protos := map[config.Protocol][]string{
		config.ProtocolTCP: {protoTCP}, config.ProtocolUDP: {protoUDP}, config.ProtocolBoth: {protoTCP, protoUDP},
	}[r.proto]
	out := make([]string, 0, len(protos)*len(r.dports))
	for _, p := range protos {
		for _, pr := range r.dports {
			port := strconv.Itoa(int(pr.First))
			if pr.Last != pr.First {
				port += ":" + strconv.Itoa(int(pr.Last))
			}
			out = append(out, " -p "+p+" -m "+p+" --dport "+port)
		}
	}
	return out
}

// ipsetDef is a set's definition as `ipset list -t` shows it.
type ipsetDef struct {
	typ, family, maxelem, timeout string
}

func (s *set) ipsetDef() ipsetDef {
	d := ipsetDef{typ: "hash:ip", family: "inet", maxelem: strconv.Itoa(learnedMax)}
	if s.family == ipv6 {
		d.family = "inet6"
	}
	if s.timeout {
		d.timeout = strconv.Itoa(ipsetTimeout)
	}
	return d
}

// ipsetDefs parses `ipset list -t`: a block per set, a "Name:" line first.
func ipsetDefs(out []byte) map[string]ipsetDef {
	defs := map[string]ipsetDef{}
	var name string
	for line := range strings.Lines(string(out)) {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Name":
			name = val
			defs[name] = ipsetDef{}
		case "Type":
			d := defs[name]
			d.typ = val
			defs[name] = d
		case "Header":
			d := defs[name]
			f := strings.Fields(val)
			for i := 0; i+1 < len(f); i += 2 {
				switch f[i] {
				case "family":
					d.family = f[i+1]
				case "maxelem":
					d.maxelem = f[i+1]
				case "timeout":
					d.timeout = f[i+1]
				}
			}
			defs[name] = d
		}
	}
	return defs
}

// saved is one table of iptables-save's output: each chain's rules in order,
// after "-A <chain> ", and beside them the packets -c listed for each, 0
// without -c.
type saved struct {
	rules   map[string][]string
	packets map[string][]uint64
}

// parseSave parses iptables-save output, with or without -c, into its
// tables. A chain declared with no rule is present with none.
func parseSave(out []byte) map[string]saved {
	tables := map[string]saved{}
	var cur saved
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "*"):
			cur = saved{rules: map[string][]string{}, packets: map[string][]uint64{}}
			tables[line[1:]] = cur
		case cur.rules != nil:
			cur.add(line)
		}
	}
	return tables
}

// add records one line of a table: a chain declaration or a rule.
func (sv saved) add(line string) {
	if decl, ok := strings.CutPrefix(line, ":"); ok {
		if name, _, ok := strings.Cut(decl, " "); ok && sv.rules[name] == nil {
			sv.rules[name] = []string{}
		}
		return
	}
	var packets uint64
	if c, ok := strings.CutPrefix(line, "["); ok {
		if counts, rest, ok := strings.Cut(c, "] "); ok {
			p, _, _ := strings.Cut(counts, ":")
			packets, _ = strconv.ParseUint(p, 10, 64)
			line = rest
		}
	}
	if rest, ok := strings.CutPrefix(line, "-A "); ok {
		if name, r, ok := strings.Cut(rest, " "); ok {
			sv.rules[name] = append(sv.rules[name], r)
			sv.packets[name] = append(sv.packets[name], packets)
		}
	}
}
