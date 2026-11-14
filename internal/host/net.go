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
	Guard  *guard
	rules  *ruleLoader
	host   *config.Host
	log    *slog.Logger
	echo   echoFunc
	seams  *seams
	// resolver serves for the host's life; gh is what it learns into on an
	// internet: false host, nil on any other, and ghMoved wakes
	// followGitHub when a reload turns learning on or off.
	resolver *dnsServer
	gh       atomic.Pointer[gitHub]
	ghMoved  chan struct{}
	// self64 is the machine's /64 as last learned; wake64 wakes learn64
	// when a reload turns internet on.
	self64  atomic.Pointer[learned64]
	wake64  chan struct{}
	dns     atomic.Pointer[[]netip.AddrPort]
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
	mu         sync.Mutex
	NoBridgeNF bool
}

// bringUp brings host h up: it checks the cgroup namespace and picks the storage
// driver and the rule backend, writes resolv.conf, sets the namespace's
// IPv6, picks the job subnets, loads and compares the rules, starts the
// host's resolver, reads an internet: false host's domain list, resolves
// GitHub's API name, and only then starts the daemon and creates the job
// network. Any failure before the daemon starts leaves no daemon and no
// resolver; one after stops both.
func bringUp(ctx context.Context, h *config.Host, p *seams, hooks guardHooks) (*hostNet, error) {
	if err := platform.CheckPrivateNamespace(p.Root); err != nil {
		return nil, err
	}
	n := &hostNet{host: h, log: p.Log, echo: p.Echo, seams: p, ghMoved: make(chan struct{}, 1), wake64: make(chan struct{}, 1)}
	n.self64.Store(unasked64())
	n.setUpstreams(h)
	var err error
	if n.Storage, err = p.PickStorage(dataRoot); err != nil {
		return nil, err
	}
	run := platform.Bounded(p.Run)
	if n.Kernel, err = platform.ProbeKernel(ctx, run); err != nil {
		return nil, err
	}
	if err := n.up(ctx, p, run, hooks); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *hostNet) up(ctx context.Context, p *seams, run platform.Command, hooks guardHooks) error {
	h := n.host
	n.rules = &ruleLoader{Run: run, Kernel: n.Kernel, Root: p.Root}
	if err := n.namespace(ctx, p); err != nil {
		return err
	}
	var g *gitHub
	if !h.Egress.Internet {
		g = n.newGitHub(h)
	}
	want := n.want(h, g)
	rs, err := want.Generate()
	if err != nil {
		return err
	}
	err = n.rules.load(ctx, rs)
	if err != nil {
		return err
	}
	if err := n.serveDNS(ctx); err != nil {
		return err
	}
	if err := n.checkAndStart(ctx, p, want, g); err != nil {
		return errors.Join(err, n.resolver.shutdown())
	}
	n.Guard = newGuard(n.rules, want, p.Log, hooks)
	return nil
}

// checkAndStart reads an internet: false host's domain list, learning from
// the resolver as it does, then resolves GitHub's API name and starts the
// daemon.
func (n *hostNet) checkAndStart(ctx context.Context, p *seams, want wanted, g *gitHub) error {
	h := n.host
	if g != nil {
		n.gh.Store(g)
		if err := g.source.Start(ctx); err != nil {
			return err
		}
	}
	err := netsetup.CheckName(ctx, h, p.Lookup)
	if err != nil {
		return err
	}
	if want.BridgeNF6 {
		err = netsetup.SetBridgeNF6(p.Root)
		if err != nil {
			return fmt.Errorf("egress.ipv6 needs %s at 1 in the host's namespace: %w", platform.BridgeNFCall6, err)
		}
	}
	return n.startDaemon(ctx, p)
}

func (n *hostNet) namespace(ctx context.Context, p *seams) error {
	h := n.host
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
	switch {
	case h.Egress.IPv6:
		err = n.ipv6(ctx, p, routes)
	case !h.Egress.Open:
		err = netsetup.TurnIPv6Off(p.Root)
	}
	if err != nil {
		return err
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

// want is what the guard holds for host h, with g its resolver or nil.
func (n *hostNet) want(h *config.Host, g *gitHub) wanted {
	_, bridge6 := platform.BridgeNF(n.rules.Root)
	w := wanted{
		Generate: func() (*egress.Ruleset, error) {
			st := egress.State{Own64: n.Own64}
			// moved64 forgets the /64 only after the swap that turns
			// internet off, so the closed rules must not read it.
			if h.Egress.Internet {
				st.Self64 = n.self64.Load().prefix
			}
			if g != nil {
				st.GitHub = g.learner.Held()
			}
			return egress.Generate(&egress.Input{Host: h, Job6: n.Job6, State: st}, n.Kernel.Backend)
		},
		IPv6Off:   !h.Egress.Open && !h.Egress.IPv6,
		BridgeNF6: h.Egress.IPv6 && bridge6,
	}
	if g != nil {
		w.Loading = g.learner.Loading()
	}
	return w
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

// hold runs the guard, an internet: false host's domain list refresh and an
// ipv6 host's /64 learner until ctx ends, answering nil, or until a restore
// or a new /64's load fails, the resolver stops or the daemon exits,
// answering why; on any failure but the daemon's exit it stops the daemon
// first, since jobs may no longer run under the host's policy. It waits for
// what it started.
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
	wg.Go(func() { n.followGitHub(hctx) })
	n.mu.Lock()
	v6 := n.host.Egress.IPv6
	n.mu.Unlock()
	if v6 {
		wg.Go(func() {
			if err := n.learn64(hctx, n.echo); err != nil {
				cancel(err)
			}
		})
	}
	err := n.Guard.run(hctx)
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

// reload swaps the host's rules for section h, read again on SIGHUP, and
// turns the resolver's learning and an ipv6 host's /64 learner on or off
// with egress.internet. A section that changes what only a restart applies,
// or whose resolvers or allow entries overlap the job subnet in use, keeps
// the old rules and answers why as kept, as does a failed swap, or a domain
// list unread once internet is turned off, which loads the old rules again.
// fatal is a failure the guard would answer, the old rules not loading
// again among them, and the caller stops the host.
func (n *hostNet) reload(ctx context.Context, h *config.Host) (kept, fatal error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	kept = restartOnly(n.host, h)
	if c := netsetup.Conflicts(n.Subnet, h); len(c) > 0 {
		kept = errors.Join(kept, fmt.Errorf("the job subnet in use %s", strings.Join(c, "; ")))
	}
	if kept != nil {
		return kept, nil
	}
	old, g := n.host, n.gh.Load()
	next := g
	switch {
	case h.Egress.Internet:
		next = nil
	case g == nil:
		next = n.newGitHub(h)
	}
	n.setUpstreams(h)
	// Learning is armed before the swap, so an address the resolver
	// answers during it is in the loaded set or written after it; turning
	// internet on takes learning off only once the swap has loaded.
	if next != nil {
		next.learner.SetAllowed(n.learnable(h))
		n.gh.Store(next)
	}
	kept, fatal = n.Guard.swap(ctx, n.want(h, next))
	if kept == nil && fatal == nil && next != nil && g == nil {
		kept, fatal = n.startList(ctx, next, old)
	}
	if kept != nil || fatal != nil {
		if g != nil {
			g.learner.SetAllowed(n.learnable(old))
		}
		n.gh.Store(g)
		n.setUpstreams(old)
		return kept, fatal
	}
	if next != g {
		n.setGitHub(next)
	}
	n.host = h
	n.moved64(old, h)
	return nil, nil
}

// startList reads the domain list a reload that turned internet off learns
// from; when it cannot be read it loads section old's rules again, and a
// failure of that load is fatal, as a failed restore is.
func (n *hostNet) startList(ctx context.Context, g *gitHub, old *config.Host) (kept, fatal error) {
	err := g.source.Start(ctx)
	if err == nil {
		return nil, nil
	}
	back, fatal := n.Guard.swap(ctx, n.want(old, nil))
	if back != nil && fatal == nil {
		fatal = fmt.Errorf("restoring the internet rules after the domain list failed: %w", back)
	}
	return err, fatal
}

func (n *hostNet) setUpstreams(h *config.Host) {
	ups := n.seams.Upstreams(h.DNS)
	n.dns.Store(&ups)
}

// restartOnly names the keys whose change the running host cannot apply:
// they decide the job network's shape, which runners are attached to.
func restartOnly(old, h *config.Host) error {
	var keys []string
	at := "hosts." + h.Name + "."
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
