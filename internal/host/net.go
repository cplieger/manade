// Package host is the runner host's supervisor: it brings up the host's
// network namespace, its egress rules and its own Docker daemon, in that
// order, and holds them in place while the host runs.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/host/resolver"
)

// The job network every runner joins, on the bridge the rules match.
const (
	jobNetwork = "manade"
	jobBridge  = "manade0"
)

// daemonStartTimeout bounds the wait for the daemon's first answer.
const daemonStartTimeout = 5 * time.Minute

// process is the running daemon.
type process interface {
	PID() int
	Done() <-chan struct{}
	Err() error
	Stop() error
}

// jobNetworks creates the job network on the host's daemon.
type jobNetworks interface {
	EnsureNetwork(ctx context.Context, spec *docker.NetworkSpec) error
	Close()
}

// seams is what bring-up reads and starts, the real kernel and daemon in
// production.
type seams struct {
	Run         platform.Command
	Lookup      netsetup.Lookup
	Random      io.Reader
	PickStorage func(dataRoot string) (platform.StoragePick, error)
	StartDaemon func(spec *daemonSpec) (process, error)
	// Dial connects to the started daemon; bring-up retries it until the
	// daemon answers.
	Dial func(ctx context.Context) (jobNetworks, error)
	// ListenDNS, FetchDomains and Echo serve the host's resolver, read an
	// internet: false host's domain list and learn an ipv6 host's /64;
	// listenDNS, resolver.FetchMeta and echoOverIPv6 in production.
	ListenDNS    func(ctx context.Context) (net.PacketConn, net.Listener, error)
	FetchDomains resolver.Fetch
	// Upstreams answers where the resolver asks the configured resolvers:
	// resolver.Upstreams in production.
	Upstreams func(dns []netip.Addr) []netip.AddrPort
	Echo      echoFunc
	Log       *slog.Logger
	Root      platform.Root
}

// hostNet is a host whose namespace, rules and daemon are up.
type hostNet struct {
	Daemon process
	rules  *ruleLoader
	log    *slog.Logger
	echo   echoFunc
	seams  *seams
	hooks  guardHooks
	// resolver serves for the host's life.
	resolver *dnsServer
	// posture is what the section's open and internet keys decide; a change
	// of either replaces it whole through hold.
	posture  atomic.Pointer[posture]
	rebuilds chan *rebuild
	// dns are the configured resolvers the host's resolver asks.
	dns     []netip.AddrPort
	Kernel  *platform.Kernel
	Storage platform.StoragePick
	Subnet  netip.Prefix
	Job6    netip.Prefix
	Own64   netip.Prefix
	// Gateway4 and Gateway6 are the host container's default gateways;
	// Addr6 its own IPv6 address, with ipv6.
	Gateway4   netip.Addr
	Gateway6   netip.Addr
	Addr6      netip.Addr
	Original   egress.Resolvers
	NoBridgeNF bool
}

// bringUp brings host h up: it checks the cgroup namespace and picks the storage
// driver and the rule backend, writes resolv.conf, reads the routes, picks
// the job subnets, brings h's posture up (its rules loaded and compared),
// starts the host's resolver, reads an internet: false host's domain list,
// resolves GitHub's API name, and only then starts the daemon and creates
// the job network. Any failure before the daemon starts leaves no daemon
// and no resolver; one after stops both.
func bringUp(ctx context.Context, h *config.Host, p *seams, hooks guardHooks) (*hostNet, error) {
	if err := platform.CheckPrivateNamespace(p.Root); err != nil {
		return nil, err
	}
	n := &hostNet{log: p.Log, echo: p.Echo, seams: p, hooks: hooks, rebuilds: make(chan *rebuild), dns: p.Upstreams(h.DNS)}
	var err error
	if n.Storage, err = p.PickStorage(dataRoot); err != nil {
		return nil, err
	}
	run := platform.Bounded(p.Run)
	if n.Kernel, err = platform.ProbeKernel(ctx, run); err != nil {
		return nil, err
	}
	n.rules = &ruleLoader{Run: run, Kernel: n.Kernel, Root: p.Root}
	if err := n.namespace(ctx, h); err != nil {
		return nil, err
	}
	pst := n.newPosture(h)
	n.posture.Store(pst)
	if err := pst.guard.bring(ctx); err != nil {
		return nil, err
	}
	if err := n.serveDNS(ctx); err != nil {
		return nil, err
	}
	if err := n.checkAndStart(ctx, pst); err != nil {
		return nil, errors.Join(err, n.resolver.shutdown())
	}
	return n, nil
}

// checkAndStart makes posture p ready, then starts the daemon.
func (n *hostNet) checkAndStart(ctx context.Context, p *posture) error {
	if err := n.ready(ctx, p); err != nil {
		return err
	}
	if p.guard.want.BridgeNF6 {
		if err := netsetup.SetBridgeNF6(n.rules.Root); err != nil {
			return fmt.Errorf("egress.ipv6 needs %s at 1 in the host's namespace: %w", platform.BridgeNFCall6, err)
		}
	}
	return n.startDaemon(ctx, n.seams)
}

func (n *hostNet) namespace(ctx context.Context, h *config.Host) error {
	p := n.seams
	var err error
	if n.Original, err = netsetup.ReadOriginal(p.Root); err != nil {
		return err
	}
	err = netsetup.WriteResolvConf(p.Root, netsetup.ResolvConf())
	if err != nil {
		return err
	}
	routes, err := netsetup.ReadRoutes(p.Root)
	if err != nil {
		return err
	}
	n.Gateway4, n.Gateway6 = routes.Gateway4, routes.Gateway6
	if h.Egress.IPv6 {
		err = n.ipv6(ctx, p, routes)
		if err != nil {
			return err
		}
	}
	if h.Subnet.IsValid() {
		n.Subnet = h.Subnet
		return netsetup.CheckSubnet(h, routes.OnLink)
	}
	n.Subnet, err = netsetup.PickSubnet(h, routes.OnLink)
	return err
}

func (n *hostNet) ipv6(ctx context.Context, p *seams, routes netsetup.Routes) error {
	addrs, err := netsetup.GlobalAddrs6(p.Root)
	if err != nil {
		return err
	}
	if n.Own64, err = netsetup.CheckIPv6(addrs, routes, n.Kernel.IPv6NAT); err != nil {
		return err
	}
	n.Addr6 = addrs[0]
	err = netsetup.EnableIPv6Forwarding(p.Root)
	if err != nil {
		return err
	}
	n.Job6, err = netsetup.JobPrefix6(ctx, p.Root.Path(stateDir+"/job-ipv6"), routes.OnLink, p.Random)
	return err
}

func (n *hostNet) startDaemon(ctx context.Context, p *seams) error {
	v4, _ := platform.BridgeNF(p.Root)
	n.NoBridgeNF = !v4
	d, err := p.StartDaemon(&daemonSpec{Kernel: n.Kernel, Storage: n.Storage.Driver, NoBridgeNF: n.NoBridgeNF})
	if err != nil {
		return err
	}
	n.Daemon = d
	fail := func(err error) error {
		return errors.Join(err, d.Stop())
	}
	wctx, cancel := context.WithTimeout(ctx, daemonStartTimeout)
	defer cancel()
	var c jobNetworks
	err = waitDaemon(wctx, d.Done(), func(ctx context.Context) error {
		var derr error
		c, derr = p.Dial(ctx)
		return derr
	})
	if err != nil {
		return fail(err)
	}
	spec := &docker.NetworkSpec{Name: jobNetwork, Bridge: jobBridge, Subnet: n.Subnet, Subnet6: n.Job6}
	err = c.EnsureNetwork(ctx, spec)
	c.Close()
	if err != nil {
		return fail(fmt.Errorf("creating the job network: %w", err))
	}
	return nil
}

// hold holds the host's posture, its guard, an internet: false posture's
// domain list refresh and an ipv6 posture's /64 learner, applying each
// change sent through change in between, until ctx ends, answering nil, or
// until a part fails, the resolver stops or the daemon exits, answering
// why; on any failure but the daemon's exit it stops the daemon first,
// since jobs may no longer run under the host's policy. It waits for what
// it started.
func (n *hostNet) hold(ctx context.Context) error {
	hctx, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel(nil)
	wg.Go(func() {
		select {
		case <-n.Daemon.Done():
			cancel(nil)
		case <-hctx.Done():
		}
	})
	wg.Go(func() {
		select {
		case <-n.resolver.done:
			cancel(n.resolver.failure())
		case <-hctx.Done():
		}
	})
	err := n.holdPostures(hctx)
	select {
	case <-n.Daemon.Done():
		return fmt.Errorf("dockerd exited: %w", errors.Join(err, n.Daemon.Err()))
	default:
	}
	if err == nil && ctx.Err() == nil {
		err = context.Cause(hctx)
	}
	if err != nil {
		return errors.Join(err, n.Daemon.Stop())
	}
	return nil
}

// stop stops the daemon, which stops each runner, then the host's resolver,
// and waits for both.
func (n *hostNet) stop() error {
	return errors.Join(n.Daemon.Stop(), n.resolver.shutdown())
}

// restartOnly names the keys whose change the running host cannot apply:
// the job network's shape, which runners are attached to, and the
// resolvers every running job's lookups go to.
func restartOnly(old, h *config.Host) error {
	var keys []string
	at := "hosts." + h.Name + "."
	if !slices.Equal(old.DNS, h.DNS) {
		keys = append(keys, at+"dns")
	}
	if old.Egress.IPv6 != h.Egress.IPv6 {
		keys = append(keys, at+"egress.ipv6")
	}
	if old.Subnet != h.Subnet {
		keys = append(keys, at+"network.subnet")
	}
	if len(keys) == 0 {
		return nil
	}
	return fmt.Errorf("%s changed, which takes a restart of the host; restart it to apply the new section", strings.Join(keys, ", "))
}
