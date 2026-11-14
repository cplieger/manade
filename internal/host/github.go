package host

import (
	"context"
	"errors"
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

// gitHub is an internet: false host's resolver, domain list and learned
// addresses.
type gitHub struct {
	err     error
	learner *resolver.Learner
	source  *resolver.Source
	fwd     *resolver.Forwarder
	stop    context.CancelFunc
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

// newGitHub answers host h's learner, forwarder and domain list, not yet
// serving: its rules are generated from the learner before it serves.
func (n *hostNet) newGitHub(h *config.Host) *gitHub {
	p := n.seams
	g := &gitHub{
		learner: resolver.NewLearner(n.rules.addLearned, n.learnable(h), p.Log),
		source:  resolver.NewSource(p.FetchDomains, p.Root.Path(domainsFile), p.Log),
		done:    make(chan struct{}),
	}
	g.fwd = resolver.NewForwarder(n.upstreams, g.source.Matcher, g.learner, p.Log)
	return g
}

// serveGitHub starts g's forwarder on the platform's DNS listeners.
func (n *hostNet) serveGitHub(ctx context.Context, g *gitHub) error {
	udp, tcp, err := n.seams.ListenDNS(ctx)
	if err != nil {
		return fmt.Errorf("starting the host's DNS resolver: %w", err)
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	g.stop = cancel
	go func() {
		defer cancel()
		g.err = g.fwd.Serve(sctx, udp, tcp)
		close(g.done)
	}()
	return nil
}

// startGitHub serves g's forwarder, then reads GitHub's domain list through
// it, or loads the saved one. On a failure nothing is left serving.
func (n *hostNet) startGitHub(ctx context.Context, g *gitHub) error {
	if err := n.serveGitHub(ctx, g); err != nil {
		return err
	}
	if err := g.source.Start(ctx); err != nil {
		return errors.Join(err, g.shutdown())
	}
	return nil
}

// shutdown stops the forwarder, if it serves, and waits for it.
func (g *gitHub) shutdown() error {
	if g.stop == nil {
		return nil
	}
	g.stop()
	<-g.done
	return g.err
}

func (g *gitHub) failure() error {
	return fmt.Errorf("the host's DNS resolver stopped: %w", g.err)
}

// setGitHub makes g the resolver the host runs, nil for none, and wakes
// followGitHub.
func (n *hostNet) setGitHub(g *gitHub) {
	n.gh.Store(g)
	select {
	case n.ghMoved <- struct{}{}:
	default:
	}
}

// followGitHub refreshes the domain list of whichever resolver the host
// runs, until ctx ends or that resolver stops unasked, answering why.
func (n *hostNet) followGitHub(ctx context.Context) error {
	for {
		g := n.gh.Load()
		if g == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-n.ghMoved:
				continue
			}
		}
		gctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Go(func() { g.source.Run(gctx) })
		var err error
		select {
		case <-ctx.Done():
		case <-n.ghMoved:
		case <-g.done:
			if n.gh.Load() == g {
				err = g.failure()
			}
		}
		cancel()
		wg.Wait()
		if err != nil || ctx.Err() != nil {
			return err
		}
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
