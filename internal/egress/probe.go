package egress

import (
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/cplieger/manade/internal/config"
)

const (
	// ConnectTimeout bounds each connection a check makes.
	ConnectTimeout = 3 * time.Second
	// LookupTimeout bounds each configured resolver the output leg's lookup
	// asks, once, so it never waits out resolv.conf's 5 s and two attempts.
	LookupTimeout = 3 * time.Second
	// embeddedUpstreamWait and embeddedUpstreams are Docker's embedded
	// resolver's per-upstream wait and upstream count, extIOTimeout and
	// maxExtDNS in https://github.com/moby/moby/blob/docker-v29.0.0/daemon/libnetwork/resolver.go.
	embeddedUpstreamWait = 4 * time.Second
	embeddedUpstreams    = 3
	// ForwarderBudget bounds an internet: false host's forwarder, its
	// embedded resolver's one upstream, on each query across every resolver
	// it asks (SERVFAIL past it): strictly inside embeddedUpstreamWait, so
	// its answer lands before that wait gives up.
	ForwarderBudget = embeddedUpstreamWait - 500*time.Millisecond
	// lookupMargin is the job-leg lookup's time past the embedded
	// resolver's last upstream wait, to relay an answer that came at its end.
	lookupMargin = time.Second
	// ExchangeSlack is added to a job-leg check's timeout for its exchange
	// over the probe container's attached stream.
	ExchangeSlack = 2 * time.Second
	// probeCalls are a run's Docker calls, each under the host's call
	// timeout: for the probe and the listener a create, one more call of
	// inspection after a timed-out create, a start, and after a timed-out
	// start an inspect and the start issued again; then the probe's attach,
	// whose call timeout bounds only its 101 answer, and the load a missing
	// probe image needs.
	probeCalls = 2*5 + 1 + 1
)

// GitHubAPI is the name every host must resolve and reach.
const GitHubAPI = "api.github.com"

var (
	// outsideGitHub is a public address outside GitHub's names, which an
	// internet: false host must drop.
	outsideGitHub = netip.MustParseAddrPort("9.9.9.9:443")
	// public6 is the IPv6 address a host with ipv6 reaches under internet.
	public6 = netip.MustParseAddrPort("[2606:4700:4700::1111]:443")
	// private6 are the IPv6 ranges a resolver counts as private in.
	private6 = []netip.Prefix{ula, netip.MustParsePrefix("fe80::/10")}
)

// Leg is where a check runs from.
type Leg int

// The legs; the zero Leg is neither.
const (
	// LegJob checks run in the probe container on the job network.
	LegJob Leg = iota + 1
	// LegOutput checks run from the supervisor's own stack.
	LegOutput
)

// Kind is what a check must show.
type Kind int

// The kinds; the zero Kind is none of them.
const (
	// KindBlock must time out, with a drop counting it.
	KindBlock Kind = iota + 1
	// KindReach must connect.
	KindReach
	// KindResolve must resolve its name.
	KindResolve
	// KindNoIPv6 must fail with no route or time out: on a host without
	// ipv6 the absence of IPv6 is what it proves.
	KindNoIPv6
	// KindTry is an allow entry's reachability, logged and never required:
	// the destination may be down.
	KindTry
)

// Check is one step of a probe run.
type Check struct {
	// Target is the address a KindBlock check puts alone in its family's
	// target set while it runs.
	Target netip.Addr
	// Name says what the check proves, for its log line and a failed
	// proof.
	Name string
	// Addr is the host:port a connection check dials, or the name a
	// KindResolve check looks up.
	Addr string
	// Timeout is the probe binary's deadline for the whole check: one
	// connection, or one lookup through every resolver it asks.
	Timeout time.Duration
	Leg     Leg
	Kind    Kind
}

// Exchange bounds the check's exchange with the probe container, or is 0
// for an output-leg check, which has none.
func (c *Check) Exchange() time.Duration {
	if c.Leg != LegJob {
		return 0
	}
	return c.Timeout + ExchangeSlack
}

// ProbeInput is what a probe plan is made from: the host's section, what
// its start found and the listener the run started.
type ProbeInput struct {
	Host *config.Host
	// JobGateway and JobGateway6 are the job network's gateways, the
	// host's own stack; JobGateway6 only with ipv6.
	JobGateway, JobGateway6 netip.Addr
	// Listener and Listener6 are the listener container's addresses on the
	// job network, standing in for another job; Listener6 only with ipv6.
	Listener, Listener6 netip.Addr
	// OutwardGateway and OutwardGateway6 are the host container's own
	// default gateways; OutwardGateway6 only with ipv6.
	OutwardGateway, OutwardGateway6 netip.Addr
	// Own6 is the host container's own IPv6 address, with ipv6.
	Own6 netip.Addr
	// Self64 is the machine's learned /64, or the zero Prefix.
	Self64 netip.Prefix
	// Original are the resolvers the host's resolv.conf named before the
	// host rewrote it.
	Original Resolvers
	// APIPort is the host API's TCP port, or 0 when it serves a Unix
	// socket only.
	APIPort      uint16
	ListenerPort uint16
}

// Plan is a probe run's checks in the order they run: the job leg, its
// allow-entry tries, then the output leg.
type Plan struct {
	Checks []Check
}

// PlanProbe answers the checks that prove in's host. An open host gets no
// block check: nothing there drops.
func PlanProbe(in *ProbeInput) *Plan {
	p := &planner{in: in, e: &in.Host.Egress}
	p.jobLeg()
	p.tries()
	p.outputLeg()
	return &Plan{Checks: p.checks}
}

// Bound is the longest a run of p lasts with callTimeout on each Docker
// call: the calls, then each check's timeout and each exchange's slack.
// The attach's call timeout covers only its 101 answer, so exchanges that
// sum past callTimeout still fit.
func (p *Plan) Bound(callTimeout time.Duration) time.Duration {
	b := probeCalls * callTimeout
	for i := range p.Checks {
		c := &p.Checks[i]
		b += max(c.Timeout, c.Exchange())
	}
	return b
}

// Deadline bounds a probe request: the run it may wait out and its own.
func (p *Plan) Deadline(callTimeout time.Duration) time.Duration {
	return 2 * p.Bound(callTimeout)
}

type planner struct {
	in     *ProbeInput
	e      *config.Egress
	checks []Check
	leg    Leg
}

func (p *planner) add(c *Check) {
	c.Leg = p.leg
	if c.Timeout == 0 {
		c.Timeout = ConnectTimeout
	}
	dup := func(o Check) bool { return o.Leg == c.Leg && o.Kind == c.Kind && o.Addr == c.Addr }
	if !slices.ContainsFunc(p.checks, dup) {
		p.checks = append(p.checks, *c)
	}
}

// block adds a check that a, port, is dropped, unless an allow entry opens
// it: the forward and output chains accept allow entries ahead of their
// drops.
func (p *planner) block(name string, a netip.Addr, port uint16) {
	if a.IsValid() && p.e.AllowsTCP(a, port) {
		return
	}
	p.blockLocal(name, a, port)
}

// blockLocal is block for a target dropped ahead of every allow entry: the
// host's own addresses and another job. It adds nothing when a is absent or
// of a family the host's jobs have no route for.
func (p *planner) blockLocal(name string, a netip.Addr, port uint16) {
	if !a.IsValid() || (a.Is6() && !p.e.IPv6) {
		return
	}
	p.add(&Check{Name: name, Kind: KindBlock, Addr: hostPort(a, port), Target: a})
}

func (p *planner) jobLeg() {
	p.leg = LegJob
	in := p.in
	if !p.e.Open {
		for _, gw := range []netip.Addr{in.JobGateway, in.JobGateway6} {
			if in.APIPort != 0 {
				p.blockLocal("the job network's gateway, on the API's port", gw, in.APIPort)
			}
			p.blockLocal("the job network's gateway", gw, 1)
		}
		p.gateway()
		for _, r := range p.privateResolvers() {
			p.block("a private resolver, reached directly", r, 53)
		}
		p.blockLocal("another job", in.Listener, in.ListenerPort)
		p.blockLocal("another job, over IPv6", in.Listener6, in.ListenerPort)
	}
	p.add(&Check{Name: "GitHub's API", Kind: KindReach, Addr: net.JoinHostPort(GitHubAPI, "443")})
	if !p.e.Internet {
		p.block("a public address outside GitHub's names", outsideGitHub.Addr(), outsideGitHub.Port())
	}
	p.add(&Check{Name: "a lookup through the job's resolver", Kind: KindResolve, Addr: GitHubAPI, Timeout: p.jobLookup()})
	p.ipv6()
}

// jobLookup is the embedded resolver's wait across the upstreams it asks,
// the configured resolvers or on an internet: false host the supervisor's
// forwarder alone, plus lookupMargin.
func (p *planner) jobLookup() time.Duration {
	n := min(len(p.in.Host.DNS), embeddedUpstreams)
	if !p.e.Internet {
		n = 1
	}
	return time.Duration(n)*embeddedUpstreamWait + lookupMargin
}

// gateway adds the outward gateway's ports 1 and 53, 53 only when no
// configured resolver is that address.
func (p *planner) gateway() {
	gw := p.in.OutwardGateway
	p.block("the host's outward gateway", gw, 1)
	if !slices.Contains(p.in.Host.DNS, gw) {
		p.block("the host's outward gateway, on port 53", gw, 53)
	}
}

func (p *planner) ipv6() {
	in := p.in
	if p.e.Open {
		return
	}
	if !p.e.IPv6 {
		p.add(&Check{Name: "IPv6, which the host's jobs do not have", Kind: KindNoIPv6, Addr: public6.String()})
		return
	}
	if p.e.Internet {
		p.add(&Check{Name: "a public IPv6 address", Kind: KindReach, Addr: public6.String()})
	} else {
		p.block("a public IPv6 address outside GitHub's names", public6.Addr(), public6.Port())
	}
	p.blockLocal("the host's own IPv6 address", in.Own6, 1)
	p.block("the host's outward IPv6 gateway", in.OutwardGateway6, 1)
	if p.e.Internet && in.Self64.IsValid() {
		p.block("an address in the machine's /64", in.Self64.Masked().Addr().Next(), 1)
	}
}

// tries adds one connection to each TCP allow entry's first host address
// and port; a UDP dial proves nothing, so a UDP-only entry gets none.
func (p *planner) tries() {
	for _, a := range p.e.Allow {
		if a.Protocol == config.ProtocolUDP || len(a.Ports) == 0 {
			continue
		}
		addr := a.To.Masked().Addr()
		if !a.To.IsSingleIP() && a.To.Bits() < addr.BitLen()-1 {
			addr = addr.Next()
		}
		p.add(&Check{Name: "an allow entry, " + a.To.String(), Kind: KindTry, Addr: hostPort(addr, a.Ports[0].First)})
	}
}

func (p *planner) outputLeg() {
	p.leg = LegOutput
	lookup := &Check{
		Name: "a lookup through the configured resolvers", Kind: KindResolve, Addr: GitHubAPI,
		Timeout: time.Duration(len(p.in.Host.DNS)) * LookupTimeout,
	}
	if p.e.Open {
		p.add(lookup)
		return
	}
	p.gateway()
	for _, r := range p.unconfiguredOriginals() {
		p.block("an original resolver the host does not use", r, 53)
	}
	for _, r := range p.in.Host.DNS {
		if !p.e.Internet || private(r) {
			p.block("a configured resolver, off port 53", r, 1)
		}
	}
	p.add(lookup)
	if !p.e.IPv6 {
		p.add(&Check{Name: "IPv6, which the host does not have", Kind: KindNoIPv6, Addr: public6.String()})
	}
}

// privateResolvers answers the private resolvers, original and configured.
func (p *planner) privateResolvers() []netip.Addr {
	all := slices.Concat(p.in.Original.Addrs, p.in.Host.DNS)
	return slices.DeleteFunc(all, func(a netip.Addr) bool { return !private(a) })
}

// unconfiguredOriginals answers the original resolvers the output chain
// drops port 53 to: the private ones, and without internet the public ones
// too. Loopback ones live in the machine's namespace.
func (p *planner) unconfiguredOriginals() []netip.Addr {
	return slices.DeleteFunc(slices.Clone(p.in.Original.Addrs), func(a netip.Addr) bool {
		return slices.Contains(p.in.Host.DNS, a) || a.IsLoopback() || (p.e.Internet && !private(a))
	})
}

// private reports whether a is a LAN address: inside the private IPv4
// ranges or IPv6's ULA and link-local space. Loopback is not: the original
// file's host() loopback resolvers live in the machine's namespace.
func private(a netip.Addr) bool {
	a = a.Unmap()
	ranges := config.PrivateRanges
	if a.Is6() {
		ranges = private6
	}
	return slices.ContainsFunc(ranges, func(r netip.Prefix) bool { return r.Contains(a) })
}

func hostPort(a netip.Addr, port uint16) string {
	return net.JoinHostPort(a.Unmap().String(), strconv.Itoa(int(port)))
}
