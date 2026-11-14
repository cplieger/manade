package host

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/host/resolver"
	"github.com/cplieger/manade/internal/hostapi"
)

// domainsFile is where an internet: false host saves GitHub's domain list.
const domainsFile = stateDir + "/github-domains.json"

// listenDNS listens on the forwarder's address, where resolv.conf points.
func listenDNS(ctx context.Context) (net.PacketConn, net.Listener, error) {
	return resolver.Listen(ctx, netip.AddrPortFrom(netsetup.ForwarderAddr, resolver.Port))
}

// dnsServer is the host's own resolver, serving from bring-up to the host's
// stop on every host: resolv.conf names it alone, so a running job's
// upstream never moves.
type dnsServer struct {
	err  error
	stop context.CancelFunc
	// done closes once the forwarder has stopped, err then set.
	done chan struct{}
}

// gitHub is an internet: false host's domain list and learned addresses,
// which the host's resolver learns into while it is the host's.
type gitHub struct {
	learner *resolver.Learner
	source  *resolver.Source
}

// upstreams answers the resolvers the host's section names now, with no
// lock, so a query never waits on a reload.
func (n *hostNet) upstreams() []netip.AddrPort { return *n.dns.Load() }

// learning answers what the resolver learns now: an internet: false host's
// list and learner, nil on any other host.
func (n *hostNet) learning() *resolver.Learning {
	g := n.gh.Load()
	if g == nil {
		return nil
	}
	return &resolver.Learning{Match: g.source.Matcher(), Learner: g.learner}
}

// learnable answers the test a learned address passes on host h.
func (n *hostNet) learnable(h *config.Host) func(netip.Addr) bool {
	return (&egress.Input{Host: h, State: egress.State{Own64: n.Own64}}).Learnable()
}

// newGitHub answers host h's learner and domain list; the list is the
// bootstrap one until its Source starts.
func (n *hostNet) newGitHub(h *config.Host) *gitHub {
	p := n.seams
	return &gitHub{
		learner: resolver.NewLearner(n.rules.addLearned, n.learnable(h), p.Log),
		source:  resolver.NewSource(p.FetchDomains, p.Root.Path(domainsFile), p.Log),
	}
}

func (n *hostNet) serveDNS(ctx context.Context) error {
	udp, tcp, err := n.seams.ListenDNS(ctx)
	if err != nil {
		return fmt.Errorf("starting the host's DNS resolver: %w", err)
	}
	fwd := resolver.NewForwarder(n.upstreams, n.learning, n.log)
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d := &dnsServer{stop: cancel, done: make(chan struct{})}
	go func() {
		defer cancel()
		d.err = fwd.Serve(sctx, udp, tcp)
		close(d.done)
	}()
	n.resolver = d
	return nil
}

// shutdown stops the resolver, if it serves, and waits for it.
func (d *dnsServer) shutdown() error {
	if d == nil {
		return nil
	}
	d.stop()
	<-d.done
	return d.err
}

func (d *dnsServer) failure() error {
	return fmt.Errorf("the host's DNS resolver stopped: %w", d.err)
}

// setGitHub makes g what the resolver learns into, nil for nothing, and
// wakes followGitHub.
func (n *hostNet) setGitHub(g *gitHub) {
	n.gh.Store(g)
	select {
	case n.ghMoved <- struct{}{}:
	default:
	}
}

// followGitHub refreshes the domain list the host learns from, following a
// reload that turns learning on or off, until ctx ends.
func (n *hostNet) followGitHub(ctx context.Context) {
	for ctx.Err() == nil {
		g := n.gh.Load()
		gctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		if g != nil {
			wg.Go(func() { g.source.Run(gctx) })
		}
		select {
		case <-ctx.Done():
		case <-n.ghMoved:
		}
		cancel()
		wg.Wait()
	}
}

// gitHubStatus answers an internet: false host's domain list and learned
// addresses; nil on any other host.
func (n *hostNet) gitHubStatus() *hostapi.GitHubStatus {
	g := n.gh.Load()
	if g == nil {
		return nil
	}
	l, c := g.source.List(), g.learner.Counts()
	return &hostapi.GitHubStatus{
		DomainsAt: l.At, Source: l.Source,
		Addresses4: c.Held[resolver.IPv4], Addresses6: c.Held[resolver.IPv6],
		Refused4: c.Refused[resolver.IPv4], Refused6: c.Refused[resolver.IPv6],
	}
}

// gitHubName reports a name an internet: false host reaches by name: one of
// GitHub's domain list; false on any other host.
func (n *hostNet) gitHubName(name string) bool {
	g := n.gh.Load()
	return g != nil && g.source.Matcher().Match(name)
}
