package host

import (
	"context"
	"fmt"
	"net"
	"net/netip"

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

// gitHub is an internet: false posture's domain list and learned addresses,
// which the host's resolver learns into while the posture runs.
type gitHub struct {
	learner *resolver.Learner
	source  *resolver.Source
}

// upstreams answers the configured resolvers.
func (n *hostNet) upstreams() []netip.AddrPort { return n.dns }

// learning answers what the resolver learns now: an internet: false
// posture's list and learner, nil on any other.
func (n *hostNet) learning() *resolver.Learning {
	g := n.posture.Load().gh
	if g == nil {
		return nil
	}
	return &resolver.Learning{Match: g.source.Matcher(), Learner: g.learner}
}

// newGitHub answers a learner and a domain list; the list is the bootstrap
// one until its Source starts.
func (n *hostNet) newGitHub() *gitHub {
	p := n.seams
	return &gitHub{
		learner: resolver.NewLearner(n.rules.addLearned, p.Log),
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

// gitHubStatus answers an internet: false posture's domain list and learned
// addresses; nil on any other.
func (n *hostNet) gitHubStatus() *hostapi.GitHubStatus {
	g := n.posture.Load().gh
	if g == nil {
		return nil
	}
	l, c := g.source.List(), g.learner.Counts()
	return &hostapi.GitHubStatus{DomainsAt: l.At, Source: l.Source, Addresses: c.Held, Refused: c.Refused}
}

// gitHubName reports a name an internet: false posture reaches by name: one
// of GitHub's domain list; false on any other.
func (n *hostNet) gitHubName(name string) bool {
	g := n.posture.Load().gh
	return g != nil && g.source.Matcher().Match(name)
}
