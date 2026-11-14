package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

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

// gitHub is an internet: false host's resolver and domain list.
type gitHub struct {
	err    error
	source *resolver.Source
	fwd    *resolver.Forwarder
	stop   context.CancelFunc
	// done closes once the forwarder has stopped, err then set.
	done chan struct{}
}

// upstreams answers the resolvers the host's section names now, with no
// lock, so a query never waits on a reload.
func (n *hostNet) upstreams() []netip.AddrPort { return *n.dns.Load() }

// learnable answers the test a learned address passes on host h.
func (n *hostNet) learnable(h *config.Host) func(netip.Addr) bool {
	return (&egress.Input{Host: h, State: egress.State{Own64: n.Own64}}).Learnable()
}

// startGitHub starts the forwarder on the platform's DNS listeners, then
// reads GitHub's domain list through it, or loads the saved one. On a
// failure nothing is left serving.
func (n *hostNet) startGitHub(ctx context.Context, p *seams) error {
	udp, tcp, err := p.ListenDNS(ctx)
	if err != nil {
		return err
	}
	g := &gitHub{
		source: resolver.NewSource(p.FetchDomains, p.Root.Path(domainsFile), p.Log),
		done:   make(chan struct{}),
	}
	g.fwd = resolver.NewForwarder(n.upstreams, g.source.Matcher, n.learner, p.Log)
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	g.stop = cancel
	go func() {
		g.err = g.fwd.Serve(sctx, udp, tcp)
		close(g.done)
	}()
	if err := g.source.Start(ctx); err != nil {
		return errors.Join(err, g.shutdown())
	}
	n.github = g
	return nil
}

// shutdown stops the forwarder and waits for it.
func (g *gitHub) shutdown() error {
	g.stop()
	<-g.done
	return g.err
}

func (g *gitHub) failure() error {
	return fmt.Errorf("the host's DNS resolver stopped: %w", g.err)
}

// gitHubStatus answers an internet: false host's domain list and learned
// addresses; nil on any other host.
func (n *hostNet) gitHubStatus() *hostapi.GitHubStatus {
	if n.github == nil {
		return nil
	}
	l, c := n.github.source.List(), n.learner.Counts()
	return &hostapi.GitHubStatus{
		DomainsAt: l.At, Source: l.Source,
		Addresses4: c.Held[resolver.IPv4], Addresses6: c.Held[resolver.IPv6],
		Refused4: c.Refused[resolver.IPv4], Refused6: c.Refused[resolver.IPv6],
	}
}

// gitHubName reports a name an internet: false host reaches by name: one of
// GitHub's domain list; false on any other host.
func (n *hostNet) gitHubName(name string) bool {
	return n.github != nil && n.github.source.Matcher().Match(name)
}
