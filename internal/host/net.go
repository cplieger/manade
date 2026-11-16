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
	"os"
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
	"github.com/cplieger/manade/internal/hostapi"
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
	// EchoURL is the endpoint Echo asks: the host's MANADE_IPV6_ECHO.
	EchoURL string
	Log     *slog.Logger
	Root    platform.Root
	// Block6 is the host's MANADE_BLOCK6.
	Block6 []netip.Prefix
	// original is resolv.conf as the process's first boot read it: a boot
	// tried again in the same namespace reads the host's own file there.
	original struct {
		err  error
		v    egress.Resolvers
		once sync.Once
	}
}

// readOriginal answers the resolvers the container's resolv.conf named
// before any boot of this process rewrote it.
func (p *seams) readOriginal() (egress.Resolvers, error) {
	p.original.once.Do(func() { p.original.v, p.original.err = netsetup.ReadOriginal(p.Root) })
	return p.original.v, p.original.err
}

// stepError is a boot step after the facts check that failed on what the
// section's field at, under hosts.<name>, decides: the section's failure,
// where any other boot failure is the host's own.
type stepError struct {
	err error
	at  string
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// step answers err as section field at's failure, nil for nil.
func step(at string, err error) error {
	if err == nil {
		return nil
	}
	return &stepError{at: at, err: err}
}

// hostNet is a host whose namespace, rules and daemon are up.
type hostNet struct {
	Daemon process
	rules  *ruleLoader
	log    *slog.Logger
	echo   echoFunc
	// echoURL is where echo asks for the machine's /64.
	echoURL string
	seams   *seams
	hooks   guardHooks
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

// bringUp brings host h up in bring's order. A section h the host's network
// refuses answers a *hostapi.SectionRefusedError before it writes anything
// in the namespace, and a later step that fails on what h decides answers a
// *stepError. A failure before the daemon starts leaves no daemon and no
// resolver; one after stops both. Either turns IPv6 forwarding off again
// for an egress.ipv6 section.
func bringUp(ctx context.Context, h *config.Host, p *seams, hooks guardHooks) (*hostNet, error) {
	n, err := bring(ctx, h, p, hooks)
	if _, refused := errors.AsType[*hostapi.SectionRefusedError](err); err != nil && !refused && h.Egress.IPv6 {
		if ferr := netsetup.DisableIPv6Forwarding(p.Root); ferr != nil {
			p.Log.Error("IPv6 forwarding was left on after a failed boot", "error", ferr)
		}
	}
	return n, err
}

// bring checks the cgroup namespace, picks the storage driver, rule backend
// and job subnets, writes resolv.conf, brings h's posture up (its rules
// loaded and compared), starts the host's resolver, reads an internet:
// false host's domain list, resolves GitHub's API name, and only then
// starts the daemon and creates the job network.
func bring(ctx context.Context, h *config.Host, p *seams, hooks guardHooks) (*hostNet, error) {
	if err := platform.CheckPrivateNamespace(p.Root); err != nil {
		return nil, err
	}
	n := &hostNet{log: p.Log, echo: p.Echo, echoURL: p.EchoURL, seams: p, hooks: hooks, rebuilds: make(chan *rebuild), dns: p.Upstreams(h.DNS)}
	var err error
	if n.Storage, err = p.PickStorage(dataRoot); err != nil {
		return nil, err
	}
	run := platform.Bounded(p.Run)
	if n.Kernel, err = platform.ProbeKernel(ctx, run); err != nil {
		return nil, err
	}
	facts, err := readNetFacts(p.Root, n.Kernel, h, true)
	if err != nil {
		return nil, err
	}
	plan, refusals := facts.plan(h)
	if len(refusals) > 0 {
		return nil, refusedError(refusals)
	}
	n.rules = &ruleLoader{Run: run, Kernel: n.Kernel, Root: p.Root}
	if err := n.namespace(ctx, h, facts, plan); err != nil {
		return nil, err
	}
	pst := n.newPosture(h)
	n.posture.Store(pst)
	if err := pst.guard.bring(ctx); err != nil {
		return nil, step("egress", err)
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
			return step("egress.ipv6", fmt.Errorf("egress.ipv6 needs %s at 1 in the host's namespace: %w", platform.BridgeNFCall6, err))
		}
	}
	return n.startDaemon(ctx, n.seams)
}

// netFacts is what a boot reads of the host's network before it writes
// anything there.
type netFacts struct {
	kernel *platform.Kernel
	routes netsetup.Routes
	// addrs6 are the namespace's global IPv6 addresses, read only for an
	// egress.ipv6 section when read6 is set.
	addrs6 []netip.Addr
	read6  bool
}

// readNetFacts reads the routes of the namespace at root and, when read6
// and section h has egress.ipv6, its global IPv6 addresses. It writes
// nothing.
func readNetFacts(root platform.Root, k *platform.Kernel, h *config.Host, read6 bool) (*netFacts, error) {
	routes, err := netsetup.ReadRoutes(root)
	if err != nil {
		return nil, err
	}
	f := &netFacts{kernel: k, routes: routes, read6: read6 && h.Egress.IPv6}
	if f.read6 {
		if f.addrs6, err = netsetup.GlobalAddrs6(root); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// netPlan is the job subnet a section boots with and, for egress.ipv6, the
// /64 of the host's own address.
type netPlan struct {
	subnet netip.Prefix
	own64  netip.Prefix
}

// plan answers the job network section h boots with, or what of h the
// facts refuse: a network.subnet an on-link route shadows, no free pool for
// an unset one, and egress.ipv6 with no IPv6 address, default route or nat
// table; with the addresses unread, the nat table alone.
func (f *netFacts) plan(h *config.Host) (netPlan, []config.FieldError) {
	at := "hosts." + h.Name + "."
	var p netPlan
	var out []config.FieldError
	var err error
	if h.Subnet.IsValid() {
		p.subnet, err = h.Subnet, netsetup.CheckSubnet(h, f.routes.OnLink)
	} else {
		p.subnet, err = netsetup.PickSubnet(h, f.routes.OnLink)
	}
	if err != nil {
		out = append(out, config.FieldError{Path: at + "network.subnet", Message: strings.TrimPrefix(err.Error(), "netsetup: job subnet: ")})
	}
	switch {
	case !h.Egress.IPv6:
	case f.read6:
		if p.own64, err = netsetup.CheckIPv6(f.addrs6, f.routes, f.kernel.IPv6NAT); err != nil {
			out = append(out, config.FieldError{Path: at + "egress.ipv6", Message: strings.TrimPrefix(err.Error(), "netsetup: egress.ipv6 is true, but ")})
		}
	case !f.kernel.IPv6NAT:
		out = append(out, config.FieldError{Path: at + "egress.ipv6", Message: "the host's kernel has no IPv6 nat table, which egress.ipv6 needs"})
	}
	return p, out
}

// bootRefusals answers what of section h the host's network refuses, read
// as a boot on p reads it.
func bootRefusals(ctx context.Context, p *seams, h *config.Host) ([]config.FieldError, error) {
	k, err := platform.ProbeKernel(ctx, platform.Bounded(p.Run))
	if err != nil {
		return nil, err
	}
	f, err := readNetFacts(p.Root, k, h, true)
	if err != nil {
		return nil, err
	}
	_, out := f.plan(h)
	return out, nil
}

// namespace writes resolv.conf and, for egress.ipv6, the namespace's IPv6
// forwarding and the job /64, over the facts and plan a boot checked.
func (n *hostNet) namespace(ctx context.Context, h *config.Host, f *netFacts, plan netPlan) error {
	p := n.seams
	var err error
	if n.Original, err = p.readOriginal(); err != nil {
		return err
	}
	err = netsetup.WriteResolvConf(p.Root, netsetup.ResolvConf())
	if err != nil {
		return err
	}
	n.Gateway4, n.Gateway6, n.Subnet = f.routes.Gateway4, f.routes.Gateway6, plan.subnet
	if !h.Egress.IPv6 {
		return nil
	}
	n.Own64, n.Addr6 = plan.own64, f.addrs6[0]
	if err = netsetup.EnableIPv6Forwarding(p.Root); err != nil {
		return step("egress.ipv6", err)
	}
	n.Job6, err = netsetup.JobPrefix6(ctx, p.Root.Path(stateDir+"/job-ipv6"), f.routes.OnLink, p.Random)
	return step("egress.ipv6", err)
}

// staleDaemonFiles are what a daemon that did not stop cleanly leaves in
// the container's writable layer for its next run: Docker's restart
// policy restarts the same container, and the daemon refuses to start while
// its pid file names a live process, which a restarted container's PIDs,
// counted from 1 again, often do (moby pkg/pidfile). /run/docker holds
// every other socket the daemon makes.
var staleDaemonFiles = []string{"/run/docker.pid", daemonSocket, "/run/docker"}

func (n *hostNet) startDaemon(ctx context.Context, p *seams) error {
	v4, _ := platform.BridgeNF(p.Root)
	n.NoBridgeNF = !v4
	for _, f := range staleDaemonFiles {
		if err := os.RemoveAll(p.Root.Path(f)); err != nil {
			return fmt.Errorf("removing %s, left by an earlier daemon: %w", f, err)
		}
	}
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
	err = c.EnsureNetwork(ctx, jobNetworkSpec(n.Subnet, n.Job6))
	c.Close()
	if err != nil {
		return fail(step("network.subnet", fmt.Errorf("creating the job network: %w", err)))
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
