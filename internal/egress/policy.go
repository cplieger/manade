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
	setCP4     = "manade-cp4"
	setCP6     = "manade-cp6"
	setLast4   = "manade-cplast4"
	setLast6   = "manade-cplast6"
	setJoin4   = "manade-cpjoin4"
	setJoin6   = "manade-cpjoin6"
)

// Mark is the socket mark of the supervisor's control-plane dials, which
// the output chain's exception accepts match: ASCII "MN", clear of every
// mark a process in the host container's namespace sets (moby's swarm load
// balancer and encrypted overlay marks and bridge-accept-fwmark, none of
// which the host's daemon runs).
const Mark = 0x4d4e

// DialTarget is one of an enrolled host's dial targets, each with its own
// pair of exception sets.
type DialTarget int

// The dial targets; the zero DialTarget is none of them.
const (
	// TargetNewest is the newest address, or MANADE_CONTROL_PLANE.
	TargetNewest DialTarget = iota + 1
	// TargetLastGood is the address the last successful upgrade went over.
	TargetLastGood
	// TargetJoin is an unspent join token's address.
	TargetJoin
)

var dialTargets = []DialTarget{TargetNewest, TargetLastGood, TargetJoin}

// sets answers the target's IPv4 and IPv6 exception sets.
func (d DialTarget) sets() (v4, v6 string) {
	switch d {
	case TargetNewest:
		return setCP4, setCP6
	case TargetLastGood:
		return setLast4, setLast6
	case TargetJoin:
		return setJoin4, setJoin6
	}
	return "", ""
}

// Target is one dial target as the rules hold it: its TCP port, or 0 for a
// target the host does not hold at this start, whose sets stay empty and
// get no accept, and the addresses its name resolved to.
type Target struct {
	Addrs []netip.Addr
	Port  uint16
}

// ControlPlane is an enrolled host's dial targets. Two targets may hold
// the same addresses.
type ControlPlane struct {
	Newest, LastGood, Join Target
}

func (cp *ControlPlane) target(d DialTarget) *Target {
	switch d {
	case TargetNewest:
		return &cp.Newest
	case TargetLastGood:
		return &cp.LastGood
	}
	return &cp.Join
}

// LearnedMax bounds the learned set, in the kernel and in the host's own
// record, as ipset's default maxelem does.
const LearnedMax = 65536

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
	Job6 netip.Prefix
	// Block6 is the host's MANADE_BLOCK6: IPv6 prefixes dropped ahead of
	// every accept the section controls, so no allow entry or resolver
	// reopens them.
	Block6 []netip.Prefix
	// ControlPlane is an enrolled host's dial targets, nil on a socket host,
	// which has no exception.
	ControlPlane *ControlPlane
	State        State
	// KeepIPv6 says a host with ipv6 off keeps IPv6 in its namespace for a
	// dial target, so its output chain accepts neighbour discovery ahead of
	// the IPv6 drop; it needs ControlPlane.
	KeepIPv6 bool
}

// State is what the host learns while it runs.
type State struct {
	// Self64 is the machine's /64 as the echo endpoint showed it, or the zero
	// Prefix: until it is learned, job IPv6 to the internet stays dropped.
	Self64 netip.Prefix
	// Own64 is the /64 of the host container's own IPv6 address when that
	// address is global, or the zero Prefix.
	Own64 netip.Prefix
	// GitHub holds the addresses learned for GitHub's names; they are
	// loaded only on an internet: false host, and only those Learnable
	// passes.
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
	Jumps []Jump
	// IPv6NATReset (ip6tables-restore --noflush) empties MANADE-NAT on an
	// iptables rule set without IPv6 jobs, only where ip6tables lists a nat
	// table: a kernel without IPv6 NAT refuses a payload naming one.
	IPv6NATReset string
	// StaleJumps are jumps the loader deletes every copy of: the one to
	// MANADE-NAT beside IPv6NATReset, under the same condition, and on an
	// open host those into manade's filter chains.
	StaleJumps []Jump
	// StaleSets are the ipsets an IPTables rule set does not declare, which
	// the loader destroys once the payloads no longer reference them, so a
	// learned or probe set does not outlive the policy that had it.
	StaleSets []string
	chains    []chain
	sets      []set
	Backend   Backend
	open      bool
	ipv6      bool
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
	daddrSet string
	ct       string
	iif, oif iface
	saddr    netip.Prefix
	daddr    []netip.Prefix
	dports   []config.PortRange
	proto    config.Protocol
	family   family
	action   action
	// probe is the family of a counting rule's probe sets, matched after
	// the rule's own matches; anyFamily for a rule that is no counter.
	probe    family
	notDaddr bool
	// mark matches the sockets carrying Mark.
	mark bool
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
	name  string
	elems []Learned
	// addrs are an exception set's elements, which have no timeout and are
	// replaced whole.
	addrs     []netip.Addr
	family    family
	timeout   bool
	exception bool
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
	rs := &Ruleset{Backend: b, open: e.Open, ipv6: e.IPv6}
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
	errs = append(errs, in.checkControlPlane()...)
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

func (in *Input) checkControlPlane() []error {
	if in.ControlPlane == nil {
		if in.KeepIPv6 {
			return []error{errors.New("egress: IPv6 kept for a dial target on a host with none")}
		}
		return nil
	}
	var errs []error
	for _, d := range dialTargets {
		if t := in.ControlPlane.target(d); t.Port == 0 && len(t.Addrs) > 0 {
			errs = append(errs, fmt.Errorf("egress: dial target %d has addresses and no port", d))
		}
	}
	return errs
}

func (rs *Ruleset) build(in *Input) {
	e := &in.Host.Egress
	v6 := rs.ipv6
	// Job IPv6 to the internet waits for the machine's /64; the host's own
	// does not, since the learner asks the echo endpoint over it.
	jobs := destinations(in, v6, e.Internet && in.State.Self64.IsValid())
	own := destinations(in, v6, e.Internet)
	var block []rule
	if v6 && len(in.Block6) > 0 {
		block = []rule{{daddr: outermost(in.Block6), action: drop}}
	}
	cp := in.exceptions(v6)
	rs.chains = []chain{
		{hook: hookForward, rules: counted(forward(cp.drops, block, jobs, v6), v6)},
		{hook: hookOutput, rules: counted(output(&outputIn{dns: in.Host.DNS, marked: cp.accepts, block: block, dest: own, v6: v6, nd: v6 || in.KeepIPv6}), v6)},
		{hook: hookInput, rules: counted(input(v6), v6)},
	}
	rs.sets = []set{{name: setProbe4, family: ipv4}, {name: setTarget4, family: ipv4}}
	if v6 {
		rs.sets = append(rs.sets, set{name: setProbe6, family: ipv6}, set{name: setTarget6, family: ipv6})
		rs.nat = &rule{family: ipv6, oif: iface{name: jobBridges.name, glob: true, not: true}, saddr: in.Job6, action: masquerade}
	}
	if !e.Internet {
		rs.sets = append(rs.sets, set{name: setGitHub4, family: ipv4, timeout: true, elems: in.learned()})
	}
	rs.sets = append(rs.sets, cp.sets...)
}

// exceptionRules are an enrolled host's exception sets and the rules that
// use them; a socket host has none.
type exceptionRules struct {
	sets    []set
	accepts []rule
	drops   []rule
}

// exceptions answers the six exception sets, each target's marked output
// accepts on its port, and the forward drops of every set of a family the
// host's jobs have: without ipv6 the forward chain already drops all IPv6.
func (in *Input) exceptions(v6 bool) exceptionRules {
	var ex exceptionRules
	if in.ControlPlane == nil {
		return ex
	}
	for _, d := range dialTargets {
		t := in.ControlPlane.target(d)
		set4, set6 := d.sets()
		v4s, v6s := splitFamilies(t.Addrs)
		ex.sets = append(ex.sets, set{name: set4, family: ipv4, addrs: v4s, exception: true}, set{name: set6, family: ipv6, addrs: v6s, exception: true})
		for _, name := range []string{set4, set6} {
			if t.Port != 0 {
				port := []config.PortRange{{First: t.Port, Last: t.Port}}
				ex.accepts = append(ex.accepts, rule{mark: true, daddrSet: name, proto: config.ProtocolTCP, dports: port, action: accept})
			}
		}
		ex.drops = append(ex.drops, rule{daddrSet: set4, action: drop})
		if v6 {
			ex.drops = append(ex.drops, rule{daddrSet: set6, action: drop})
		}
	}
	return ex
}

// splitFamilies answers addrs unmapped, sorted and without repeats, split
// by family.
func splitFamilies(addrs []netip.Addr) (v4, v6 []netip.Addr) {
	for _, a := range addrs {
		if a = a.Unmap(); a.Is4() {
			v4 = append(v4, a)
		} else if a.IsValid() {
			v6 = append(v6, a)
		}
	}
	for _, s := range []*[]netip.Addr{&v4, &v6} {
		slices.SortFunc(*s, netip.Addr.Compare)
		*s = slices.Compact(*s)
	}
	return v4, v6
}

// learned answers the learned addresses that still have time left and pass
// Learnable, at most LearnedMax of them.
func (in *Input) learned() []Learned {
	var out []Learned
	for _, l := range in.State.GitHub {
		a := l.Addr.Unmap()
		if l.Left > 0 && Learnable(a) && len(out) < LearnedMax {
			out = append(out, Learned{Addr: a, Left: l.Left})
		}
	}
	return out
}

// Learnable reports whether an address learned for a GitHub name may enter
// the learned set, whose accept precedes the private and IPv6 drops: a
// global unicast IPv4 address outside the private ranges, never an IPv6
// one, since an internet: false host cannot learn the machine's /64 and so
// cannot keep a LAN answer under it out of the set.
func Learnable(a netip.Addr) bool {
	a = a.Unmap()
	return a.Is4() && a.IsGlobalUnicast() && !slices.ContainsFunc(config.PrivateRanges, func(p netip.Prefix) bool { return p.Contains(a) })
}

// block6 answers the IPv6 prefixes jobs never reach on this host.
func (in *Input) block6() []netip.Prefix {
	block := slices.Concat(blocked6, in.Host.Egress.Block6)
	for _, p := range []netip.Prefix{in.State.Self64, in.State.Own64} {
		if p.IsValid() {
			block = append(block, p)
		}
	}
	return outermost(block)
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
	}
	rs = append(rs, rule{daddr: config.PrivateRanges, action: drop})
	if v6 {
		rs = append(rs, rule{daddr: in.block6(), action: drop})
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

// forward is the forward chain: the exception sets' drops cp and block,
// the MANADE_BLOCK6 drops, come ahead of the allow accepts that open dest.
func forward(cp, block, dest []rule, v6 bool) []rule {
	rs := []rule{{iif: jobBridges, oif: jobBridges, action: drop}}
	if !v6 {
		rs = append(rs, rule{family: ipv6, action: drop})
	}
	rs = append(rs, rule{ct: ctEstablished, action: accept})
	all := slices.Concat(cp, block, dest)
	for i := range all {
		r := all[i]
		r.iif = jobBridges
		rs = append(rs, r)
	}
	return append(rs, rule{oif: jobBridges, ct: ctNew, action: drop})
}

// outputIn is what the output chain is built from.
type outputIn struct {
	dns []netip.Addr
	// marked are the exception sets' marked accepts, ahead of the IPv6 drop
	// so an IPv6 dial target stays reachable on a host without ipv6.
	marked []rule
	// block are the MANADE_BLOCK6 drops, ahead of the resolver accepts and
	// dest's allow accepts.
	block, dest []rule
	v6          bool
	// nd is the neighbour discovery accept: the kernel sends it unmarked
	// and untracked from its own socket, so only this accept passes it.
	nd bool
}

// output is the output chain.
func output(o *outputIn) []rule {
	rs := slices.Concat([]rule{{oif: loopback, action: accept}, {ct: ctEstablished, action: accept}}, o.marked)
	if o.nd {
		rs = append(rs, rule{nd: true, action: accept})
	}
	if !o.v6 {
		rs = append(rs, rule{family: ipv6, action: drop})
	}
	for i := range o.block {
		r := o.block[i]
		r.oif = iface{name: jobBridges.name, glob: true, not: true}
		rs = append(rs, r)
	}
	var r4, r6 []netip.Prefix
	for _, a := range o.dns {
		p := netip.PrefixFrom(a, a.BitLen())
		if a.Is4() {
			r4 = append(r4, p)
		} else if o.v6 {
			r6 = append(r6, p)
		}
	}
	dns53 := []config.PortRange{{First: 53, Last: 53}}
	for _, rr := range [][]netip.Prefix{outermost(r4), outermost(r6)} {
		if len(rr) > 0 {
			rs = append(rs, rule{daddr: rr, proto: config.ProtocolBoth, dports: dns53, action: accept})
		}
	}
	for i := range o.dest {
		r := o.dest[i]
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
	switch r.matchFamily() {
	case anyFamily:
		if v6 {
			return []family{ipv4, ipv6}
		}
		return []family{ipv4}
	case ipv6:
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
	c.family, c.action, c.probe = f, count, f
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
	case r.daddrSet != "":
		return setFamily[r.daddrSet]
	}
	return anyFamily
}

// setFamily is the family of each set a rule matches as its own.
var setFamily = map[string]family{
	setGitHub4: ipv4, setCP4: ipv4, setLast4: ipv4, setJoin4: ipv4,
	setCP6: ipv6, setLast6: ipv6, setJoin6: ipv6,
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
