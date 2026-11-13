// Package egress generates a runner host's egress rules for either kernel
// backend and compares what the kernel holds with them. It does no I/O: the
// host loads the payloads and reads the kernel back.
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/cplieger/manade/internal/config"
)

// Backend is the kernel interface the rules load through.
type Backend int

// The backends; the zero Backend is neither.
const (
	NFTables Backend = iota + 1
	IPTables
)

// The named sets, under the same names on both backends.
const (
	setProbe4  = "manade-probe4"
	setTarget4 = "manade-target4"
	setProbe6  = "manade-probe6"
	setTarget6 = "manade-target6"
	setGitHub4 = "manade-gh4"
	setGitHub6 = "manade-gh6"
)

// learnedMax bounds each family's learned set, as ipset's default maxelem
// does.
const learnedMax = 65536

// globalUnicast6 is the only IPv6 jobs reach under internet.
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// blocked6 are the IPv6 ranges jobs never reach: the IPv6 forms of private,
// link-local, multicast, translated, tunnelled and documentation space.
var blocked6 = []netip.Prefix{
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("::ffff:0:0/96"),
}

var ula = netip.MustParsePrefix("fc00::/7")

// Input is what a host's rule set is generated from.
type Input struct {
	// Host is the host's validated section; its DNS are the resolvers the
	// output chain opens port 53 to.
	Host *config.Host
	// Job6 is the job network's IPv6 /64, inside fc00::/7; required when
	// the host's egress has IPv6, ignored otherwise.
	Job6  netip.Prefix
	State State
}

// State is what the host learns while it runs.
type State struct {
	// Self64 is the machine's /64 as ipv6_echo showed it, or the zero
	// Prefix: until it is learned, job IPv6 to the internet stays dropped.
	Self64 netip.Prefix
	// Own64 is the /64 of the host container's own IPv6 address when that
	// address is global, or the zero Prefix.
	Own64 netip.Prefix
	// GitHub holds the addresses learned for GitHub's names; they are
	// loaded only on an internet: false host, IPv6 ones only with IPv6.
	GitHub []Learned
}

// Learned is one address learned for a GitHub name and how long it has left.
type Learned struct {
	Addr netip.Addr
	Left time.Duration
}

// Ruleset is a generated rule set: the payloads to load and what Compare
// expects to read back. An open host's payloads only remove manade's rules.
type Ruleset struct {
	nat *rule
	// NFT is an `nft -f` payload that replaces manade's tables in one
	// transaction.
	NFT string
	// IPSet, IPv4 and IPv6 load in that order: `ipset restore -exist`, then
	// `iptables-restore --noflush` and `ip6tables-restore --noflush`. The
	// IPv4 payload owns DOCKER-USER, which must exist before the daemon
	// starts.
	IPSet, IPv4, IPv6 string
	// Jumps must each be the first rule of their chain once the payloads
	// are loaded; the loader inserts any that is not.
	Jumps   []Jump
	chains  []chain
	sets    []set
	Backend Backend
	open    bool
	ipv6    bool
}

// Jump is a rule `-j Target` that must be first in Chain of Table, under
// ip6tables when IPv6 is true.
type Jump struct {
	Table, Chain, Target string
	IPv6                 bool
}

// family is the address family a rule matches; anyFamily matches both.
type family uint8

const (
	anyFamily family = iota
	ipv4
	ipv6
)

// iface matches an interface name; the zero iface matches any interface.
type iface struct {
	name string
	// glob matches every name starting with name.
	glob bool
	not  bool
}

// jobBridges matches every job bridge, manade0 among them.
var jobBridges = iface{name: "manade", glob: true}

var loopback = iface{name: "lo"}

type action uint8

const (
	accept action = iota + 1
	drop
	// count only counts, for the probe.
	count
	masquerade
)

const (
	ctEstablished = "established,related"
	ctNew         = "new"
)

// rule is one rule in backend-neutral form. A rule whose daddr holds several
// prefixes matches any of them.
type rule struct {
	saddrSet, daddrSet string
	ct                 string
	iif, oif           iface
	saddr              netip.Prefix
	daddr              []netip.Prefix
	dports             []config.PortRange
	proto              config.Protocol
	family             family
	action             action
	notDaddr           bool
	// nd matches ICMPv6 neighbour solicitations and advertisements.
	nd bool
	// after marks a counting rule that follows the chain's drops, which a
	// probe packet reaches only when no drop took it.
	after bool
}

// chain is one of the three filter chains, named by its hook.
type chain struct {
	hook  string
	rules []rule
}

type set struct {
	name    string
	elems   []Learned
	family  family
	timeout bool
}

const (
	hookForward = "forward"
	hookOutput  = "output"
	hookInput   = "input"
	// hookPostrouting is also the masquerade chain's name.
	hookPostrouting = "postrouting"
)

// Generate builds in's rule set for backend b.
func Generate(in *Input, b Backend) (*Ruleset, error) {
	if b != NFTables && b != IPTables {
		return nil, errors.New("egress: unknown backend")
	}
	if in == nil || in.Host == nil {
		return nil, errors.New("egress: no host section")
	}
	e := &in.Host.Egress
	rs := &Ruleset{Backend: b, open: e.Open, ipv6: e.IPv6 && !e.Open}
	if !e.Open {
		if err := in.check(); err != nil {
			return nil, err
		}
		rs.build(in)
	}
	if b == NFTables {
		rs.NFT = rs.renderNFT()
	} else {
		rs.renderIPTables()
	}
	return rs, nil
}

func (in *Input) check() error {
	var errs []error
	sixty4 := func(name string, p netip.Prefix) {
		if p.IsValid() && (!p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() != 64 || p != p.Masked()) {
			errs = append(errs, fmt.Errorf("egress: %s %s is not an IPv6 /64", name, p))
		}
	}
	if in.Host.Egress.IPv6 {
		if !in.Job6.IsValid() || !ula.Contains(in.Job6.Addr()) {
			errs = append(errs, fmt.Errorf("egress: the job network's IPv6 prefix %s is not a /64 inside %s", in.Job6, ula))
		}
		sixty4("job network", in.Job6)
		sixty4("learned prefix", in.State.Self64)
		sixty4("own prefix", in.State.Own64)
	}
	return errors.Join(errs...)
}

func (rs *Ruleset) build(in *Input) {
	e := &in.Host.Egress
	v6 := rs.ipv6
	// Job IPv6 to the internet waits for the machine's /64; the host's own
	// does not, since the learner asks ipv6_echo over it.
	jobs := destinations(in, v6, e.Internet && in.State.Self64.IsValid())
	own := destinations(in, v6, e.Internet)
	rs.chains = []chain{
		{hook: hookForward, rules: counted(forward(jobs, v6), v6)},
		{hook: hookOutput, rules: counted(output(in.Host.DNS, own, v6), v6)},
		{hook: hookInput, rules: counted(input(v6), v6)},
	}
	rs.sets = []set{{name: setProbe4, family: ipv4}, {name: setTarget4, family: ipv4}}
	if v6 {
		rs.sets = append(rs.sets, set{name: setProbe6, family: ipv6}, set{name: setTarget6, family: ipv6})
		rs.nat = &rule{family: ipv6, oif: iface{name: jobBridges.name, glob: true, not: true}, saddr: in.Job6, action: masquerade}
	}
	if !e.Internet {
		rs.sets = append(rs.sets, set{name: setGitHub4, family: ipv4, timeout: true, elems: learned(in.State.GitHub, ipv4)})
		if v6 {
			rs.sets = append(rs.sets, set{name: setGitHub6, family: ipv6, timeout: true, elems: learned(in.State.GitHub, ipv6)})
		}
	}
}

// learned answers the addresses of family f that still have time left, at
// most learnedMax of them.
func learned(all []Learned, f family) []Learned {
	var out []Learned
	for _, l := range all {
		if l.Left > 0 && l.Addr.Unmap().Is4() == (f == ipv4) && len(out) < learnedMax {
			out = append(out, Learned{Addr: l.Addr.Unmap(), Left: l.Left})
		}
	}
	return out
}

// destinations are the rules forward and output share, with no interface
// match: the allow entries, the learned GitHub addresses, the private and
// IPv6 drops and the internet drop. With global6 IPv6 global unicast
// passes.
func destinations(in *Input, v6, global6 bool) []rule {
	e := &in.Host.Egress
	var rs []rule
	for _, a := range e.Allow {
		rs = append(rs, rule{daddr: []netip.Prefix{a.To}, proto: a.Protocol, dports: a.Ports, action: accept})
	}
	if !e.Internet {
		rs = append(rs, rule{daddrSet: setGitHub4, action: accept})
		if v6 {
			rs = append(rs, rule{daddrSet: setGitHub6, action: accept})
		}
	}
	rs = append(rs, rule{daddr: config.PrivateRanges, action: drop})
	if v6 {
		block := slices.Concat(blocked6, e.Block6)
		for _, p := range []netip.Prefix{in.State.Self64, in.State.Own64} {
			if p.IsValid() {
				block = append(block, p)
			}
		}
		rs = append(rs, rule{daddr: outermost(block), action: drop})
		if global6 {
			rs = append(rs, rule{daddr: []netip.Prefix{globalUnicast6}, notDaddr: true, action: drop})
		} else {
			rs = append(rs, rule{family: ipv6, action: drop})
		}
	}
	if !e.Internet {
		rs = append(rs, rule{family: ipv4, action: drop})
	}
	return rs
}

func forward(dest []rule, v6 bool) []rule {
	rs := []rule{{iif: jobBridges, oif: jobBridges, action: drop}}
	if !v6 {
		rs = append(rs, rule{family: ipv6, action: drop})
	}
	rs = append(rs, rule{ct: ctEstablished, action: accept})
	for i := range dest {
		r := dest[i]
		r.iif = jobBridges
		rs = append(rs, r)
	}
	return append(rs, rule{oif: jobBridges, ct: ctNew, action: drop})
}

func output(dns []netip.Addr, dest []rule, v6 bool) []rule {
	rs := []rule{{oif: loopback, action: accept}, {ct: ctEstablished, action: accept}}
	if v6 {
		rs = append(rs, rule{nd: true, action: accept})
	} else {
		rs = append(rs, rule{family: ipv6, action: drop})
	}
	var r4, r6 []netip.Prefix
	for _, a := range dns {
		p := netip.PrefixFrom(a, a.BitLen())
		if a.Is4() {
			r4 = append(r4, p)
		} else if v6 {
			r6 = append(r6, p)
		}
	}
	dns53 := []config.PortRange{{First: 53, Last: 53}}
	for _, rr := range [][]netip.Prefix{outermost(r4), outermost(r6)} {
		if len(rr) > 0 {
			rs = append(rs, rule{daddr: rr, proto: config.ProtocolBoth, dports: dns53, action: accept})
		}
	}
	for i := range dest {
		r := dest[i]
		if r.action == drop {
			r.oif = iface{name: jobBridges.name, glob: true, not: true}
		}
		rs = append(rs, r)
	}
	return rs
}

func input(v6 bool) []rule {
	rs := []rule{{iif: loopback, action: accept}, {ct: ctEstablished, action: accept}}
	if v6 {
		rs = append(rs, rule{iif: jobBridges, nd: true, action: accept})
	}
	return append(rs, rule{iif: jobBridges, action: drop})
}

// counted puts the probe's counting rules into a chain: before each drop one
// per family the probe checks, with the drop's own match, and after the
// chain's drops one per family with no other match. A drop of IPv6 on a host
// without IPv6 has no probe set to count with.
func counted(rules []rule, v6 bool) []rule {
	out := make([]rule, 0, 2*len(rules))
	for i := range rules {
		r := &rules[i]
		if r.action == drop {
			for _, f := range r.families(v6) {
				out = append(out, r.counter(f))
			}
		}
		out = append(out, *r)
	}
	after := &rule{after: true}
	out = append(out, after.counter(ipv4))
	if v6 {
		out = append(out, after.counter(ipv6))
	}
	return out
}

func (r *rule) families(v6 bool) []family {
	switch {
	case r.family == anyFamily && len(r.daddr) == 0:
		if v6 {
			return []family{ipv4, ipv6}
		}
		return []family{ipv4}
	case r.matchFamily() == ipv6:
		if v6 {
			return []family{ipv6}
		}
		return nil
	}
	return []family{ipv4}
}

// counter answers a counting rule with r's matches for family f.
func (r *rule) counter(f family) rule {
	c := *r
	c.family, c.action = f, count
	if f == ipv4 {
		c.saddrSet, c.daddrSet = setProbe4, setTarget4
	} else {
		c.saddrSet, c.daddrSet = setProbe6, setTarget6
	}
	return c
}

// matchFamily is the family the rule's matches confine it to.
func (r *rule) matchFamily() family {
	switch {
	case r.family != anyFamily:
		return r.family
	case r.nd:
		return ipv6
	case r.saddr.IsValid():
		return familyOf(r.saddr.Addr())
	case len(r.daddr) > 0:
		return familyOf(r.daddr[0].Addr())
	case r.daddrSet == setGitHub6:
		return ipv6
	case r.daddrSet == setGitHub4:
		return ipv4
	}
	return anyFamily
}

func familyOf(a netip.Addr) family {
	if a.Is4() {
		return ipv4
	}
	return ipv6
}

// outermost sorts ps and drops every prefix another one holds, so a list
// never carries an element that overlaps another.
func outermost(ps []netip.Prefix) []netip.Prefix {
	s := make([]netip.Prefix, 0, len(ps))
	for _, p := range ps {
		s = append(s, p.Masked())
	}
	slices.SortFunc(s, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	out := s[:0]
	for _, p := range s {
		if n := len(out); n > 0 && out[n-1].Bits() <= p.Bits() && out[n-1].Contains(p.Addr()) {
			continue
		}
		out = append(out, p)
	}
	return out
}
