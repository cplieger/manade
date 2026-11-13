package egress

import (
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/manade/internal/config"
)

const (
	verdictAccept = "accept"
	protoTCP      = "tcp"
	protoUDP      = "udp"
)

const (
	nftTable    = "manade"
	nftNATTable = "manade-nat"
	// nftPriority is filter - 10: the kernel runs hooks of equal priority
	// in the reverse of their registration order, so at the daemon's
	// priority 0 Docker's inter-container drop, registered later, would
	// take the probe's packets before manade's counting rules
	// (https://github.com/torvalds/linux/blob/v6.12/net/netfilter/core.c#L154).
	nftPriority = -10
	// nftSrcNAT is the priority srcnat names.
	nftSrcNAT = 100
)

var nftChainNames = map[string]string{hookForward: "forward", hookOutput: "output", hookInput: "input"}

// renderNFT writes the payload. Each table is first added empty and deleted,
// so the file loads whether or not the table exists, in one transaction.
func (rs *Ruleset) renderNFT() string {
	var b strings.Builder
	for _, t := range []string{"inet " + nftTable, "ip6 " + nftNATTable} {
		fmt.Fprintf(&b, "table %s {}\ndelete table %s\n", t, t)
	}
	if rs.open {
		return b.String()
	}
	fmt.Fprintf(&b, "table inet %s {\n", nftTable)
	for _, s := range rs.sets {
		b.WriteString(s.nft())
	}
	for _, c := range rs.chains {
		fmt.Fprintf(&b, "\tchain %s {\n\t\ttype filter hook %s priority filter - 10; policy accept;\n", nftChainNames[c.hook], c.hook)
		for i := range c.rules {
			fmt.Fprintf(&b, "\t\t%s\n", c.rules[i].nft())
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	if rs.nat != nil {
		fmt.Fprintf(&b, "table ip6 %s {\n\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n\t\t%s\n\t}\n}\n",
			nftNATTable, rs.nat.nft())
	}
	return b.String()
}

func (s *set) nftType() string {
	if s.family == ipv4 {
		return "ipv4_addr"
	}
	return "ipv6_addr"
}

func (s *set) nft() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\tset %s {\n\t\ttype %s\n", s.name, s.nftType())
	if s.timeout {
		fmt.Fprintf(&b, "\t\tflags timeout\n\t\tsize %d\n", learnedMax)
	}
	if len(s.elems) > 0 {
		elems := make([]string, len(s.elems))
		for i, l := range s.elems {
			elems[i] = fmt.Sprintf("%s timeout %ds", l.Addr, seconds(l.Left))
		}
		fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(elems, ", "))
	}
	b.WriteString("\t}\n")
	return b.String()
}

// maxSetTimeout is IPSET_MAX_TIMEOUT in seconds
// (include/linux/netfilter/ipset/ip_set_timeout.h): one element above it
// fails the whole ipset restore. nftables takes more; both backends share it.
const maxSetTimeout = 2147483

// seconds rounds d up to whole seconds, from one to maxSetTimeout.
func seconds(d time.Duration) int64 {
	return min(maxSetTimeout, max(1, int64(math.Ceil(d.Seconds()))))
}

// nft renders the rule in the order its listing shows it.
func (r *rule) nft() string {
	var w []string
	for _, m := range r.matches() {
		w = append(w, m.nft())
	}
	return strings.Join(append(w, r.action.nft()), " ")
}

func (a action) nft() string {
	switch a {
	case accept:
		return verdictAccept
	case drop:
		return "drop"
	case masquerade:
		return "masquerade"
	}
	return "counter"
}

// match is one condition of a rule: left op right, in both the payload's
// text and the canonical form Compare reads back.
type match struct {
	left string
	// text is the right side as the payload writes it.
	text  string
	right []string
	not   bool
}

func (m match) nft() string {
	op := ""
	if m.not {
		op = "!= "
	}
	left := m.left
	if left == "meta iifname" || left == "meta oifname" {
		left = strings.TrimPrefix(left, "meta ")
	}
	return left + " " + op + m.text
}

// canon is the match as canonRule reads one back from nft's JSON.
func (m match) canon() string {
	op := "=="
	if m.not {
		op = "!="
	}
	return m.left + " " + op + " " + strings.Join(m.right, ",")
}

// matches answers the rule's conditions in listing order. An explicit meta
// nfproto stays only on a rule with no address match: nft drops it from the
// listing wherever an ip or ip6 match implies it.
func (r *rule) matches() []match {
	var ms []match
	addIf := func(key string, i iface) {
		if i.name == "" {
			return
		}
		name := i.name
		if i.glob {
			name += "*"
		}
		ms = append(ms, match{left: "meta " + key, right: []string{name}, not: i.not, text: strconv.Quote(name)})
	}
	addIf("iifname", r.iif)
	addIf("oifname", r.oif)
	f := r.matchFamily()
	l3 := "ip"
	if f == ipv6 {
		l3 = "ip6"
	}
	if r.family != anyFamily && !r.hasL3() {
		proto := map[family]string{ipv4: "ipv4", ipv6: "ipv6"}[r.family]
		ms = append(ms, match{left: "meta nfproto", right: []string{proto}, text: proto})
	}
	if r.saddr.IsValid() {
		ms = append(ms, prefixMatch(l3+" saddr", []netip.Prefix{r.saddr}, false))
	}
	if len(r.daddr) > 0 {
		ms = append(ms, prefixMatch(l3+" daddr", r.daddr, r.notDaddr))
	}
	if r.saddrSet != "" {
		ms = append(ms, match{left: l3 + " saddr", right: []string{"@" + r.saddrSet}, text: "@" + r.saddrSet})
	}
	if r.daddrSet != "" {
		ms = append(ms, match{left: l3 + " daddr", right: []string{"@" + r.daddrSet}, text: "@" + r.daddrSet})
	}
	if r.ct != "" {
		ms = append(ms, match{left: "ct state", right: sortedWords(strings.Split(r.ct, ",")), text: r.ct})
	}
	ms = append(ms, r.portMatches()...)
	if r.nd {
		types := []string{"nd-neighbor-solicit", "nd-neighbor-advert"}
		ms = append(ms, match{left: "icmpv6 type", right: sortedWords(types), text: "{ " + strings.Join(types, ", ") + " }"})
	}
	return ms
}

func (r *rule) hasL3() bool {
	return r.saddr.IsValid() || len(r.daddr) > 0 || r.saddrSet != "" || r.daddrSet != "" || r.nd
}

func (r *rule) portMatches() []match {
	if len(r.dports) == 0 {
		return nil
	}
	texts := make([]string, len(r.dports))
	ivs := make([]interval, len(r.dports))
	for i, p := range r.dports {
		texts[i] = strconv.Itoa(int(p.First))
		if p.Last != p.First {
			texts[i] += "-" + strconv.Itoa(int(p.Last))
		}
		ivs[i] = numInterval(uint64(p.First), uint64(p.Last))
	}
	ports := match{right: mergeIntervals(ivs), text: braced(texts)}
	switch r.proto {
	case config.ProtocolUDP:
		ports.left = "udp dport"
		return []match{ports}
	case config.ProtocolBoth:
		ports.left = "th dport"
		return []match{{left: "meta l4proto", right: []string{protoTCP, protoUDP}, text: "{ tcp, udp }"}, ports}
	}
	ports.left = "tcp dport"
	return []match{ports}
}

func prefixMatch(left string, ps []netip.Prefix, not bool) match {
	texts := make([]string, len(ps))
	ivs := make([]interval, len(ps))
	for i, p := range ps {
		texts[i] = showPrefix(p)
		ivs[i] = prefixInterval(p)
	}
	return match{left: left, right: mergeIntervals(ivs), not: not, text: braced(texts)}
}

// showPrefix writes a full-length prefix as its address, as both nft and
// iptables-save list it.
func showPrefix(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

func braced(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return "{ " + strings.Join(items, ", ") + " }"
}
