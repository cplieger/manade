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
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/host/platform"
)

// The job network every runner joins, on the bridge the rules match.
const (
	JobNetwork = "manade"
	JobBridge  = "manade0"
)

// daemonStartTimeout bounds the wait for the daemon's first answer.
const daemonStartTimeout = 5 * time.Minute

// Process is the running daemon.
type Process interface {
	Done() <-chan struct{}
	Err() error
	Stop() error
}

// Networks creates the job network on the host's daemon.
type Networks interface {
	EnsureNetwork(ctx context.Context, spec *docker.NetworkSpec) error
}

// Platform is what bring-up reads and starts, the real kernel and daemon in
// production.
type Platform struct {
	Run         platform.Command
	Lookup      netsetup.Lookup
	Random      io.Reader
	PickStorage func(dataRoot string) (platform.StoragePick, error)
	StartDaemon func(spec *DaemonSpec) (Process, error)
	// Dial connects to the started daemon; bring-up retries it until the
	// daemon answers.
	Dial func(ctx context.Context) (Networks, error)
	Log  *slog.Logger
	Root platform.Root
}

// Net is a host whose namespace, rules and daemon are up.
type Net struct {
	kept       error
	Daemon     Process
	Docker     Networks
	Guard      *Guard
	rules      *Rules
	host       *config.Host
	Kernel     *platform.Kernel
	Storage    platform.StoragePick
	Subnet     netip.Prefix
	Job6       netip.Prefix
	Own64      netip.Prefix
	Original   egress.Resolvers
	mu         sync.Mutex
	NoBridgeNF bool
}

// Up brings host h up: it checks the cgroup namespace and picks the storage
// driver and the rule backend, writes resolv.conf, sets the namespace's
// IPv6, picks the job subnets, loads and compares the rules, resolves
// GitHub's API name, and only then starts the daemon and creates the job
// network. Any failure before the daemon starts leaves no daemon; one after
// stops it.
func Up(ctx context.Context, h *config.Host, p *Platform, hooks Hooks) (*Net, error) {
	if err := platform.CheckPrivateNamespace(p.Root); err != nil {
		return nil, err
	}
	n := &Net{host: h}
	var err error
	if n.Storage, err = p.PickStorage(DataRoot); err != nil {
		return nil, err
	}
	if n.Kernel, err = platform.ProbeKernel(ctx, p.Run); err != nil {
		return nil, err
	}
	if err := n.up(ctx, p, hooks); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Net) up(ctx context.Context, p *Platform, hooks Hooks) error {
	h := n.host
	n.rules = &Rules{Run: p.Run, Kernel: n.Kernel, Root: p.Root}
	if err := n.namespace(ctx, p); err != nil {
		return err
	}
	want := n.want(h)
	rs, err := want.Generate()
	if err != nil {
		return err
	}
	err = n.rules.Load(ctx, rs)
	if err != nil {
		return err
	}
	err = netsetup.CheckName(ctx, h, netsetup.Nameservers(h), p.Lookup)
	if err != nil {
		return err
	}
	if want.BridgeNF6 {
		err = netsetup.SetBridgeNF6(p.Root)
		if err != nil {
			return fmt.Errorf("egress.ipv6 needs %s at 1 in the host's namespace: %w", platform.BridgeNFCall6, err)
		}
	}
	err = n.startDaemon(ctx, p)
	if err != nil {
		return err
	}
	n.Guard = NewGuard(n.rules, want, p.Log, hooks)
	return nil
}

func (n *Net) namespace(ctx context.Context, p *Platform) error {
	h := n.host
	var err error
	if n.Original, err = netsetup.ReadOriginal(p.Root); err != nil {
		return err
	}
	err = netsetup.WriteResolvConf(p.Root, netsetup.RenderResolvConf(netsetup.Nameservers(h)))
	if err != nil {
		return err
	}
	routes, err := netsetup.ReadRoutes(p.Root)
	if err != nil {
		return err
	}
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

func (n *Net) ipv6(ctx context.Context, p *Platform, routes netsetup.Routes) error {
	addrs, err := netsetup.GlobalAddrs6(p.Root)
	if err != nil {
		return err
	}
	if n.Own64, err = netsetup.CheckIPv6(addrs, routes, n.Kernel.IPv6NAT); err != nil {
		return err
	}
	err = netsetup.EnableIPv6Forwarding(p.Root)
	if err != nil {
		return err
	}
	n.Job6, err = netsetup.JobPrefix6(ctx, p.Root.Path(StateDir+"/job-ipv6"), routes.OnLink, p.Random)
	return err
}

func (n *Net) want(h *config.Host) Want {
	_, bridge6 := platform.BridgeNF(n.rules.Root)
	return Want{
		Generate: func() (*egress.Ruleset, error) {
			in := &egress.Input{Host: h, Job6: n.Job6, State: egress.State{Own64: n.Own64}}
			return egress.Generate(in, n.Kernel.Backend)
		},
		Resolv:    netsetup.RenderResolvConf(netsetup.Nameservers(h)),
		IPv6Off:   !h.Egress.Open && !h.Egress.IPv6,
		BridgeNF6: h.Egress.IPv6 && bridge6,
	}
}

func (n *Net) startDaemon(ctx context.Context, p *Platform) error {
	v4, _ := platform.BridgeNF(p.Root)
	n.NoBridgeNF = !v4
	d, err := p.StartDaemon(&DaemonSpec{Kernel: n.Kernel, Storage: n.Storage.Driver, NoBridgeNF: n.NoBridgeNF})
	if err != nil {
		return err
	}
	n.Daemon = d
	fail := func(err error) error {
		return errors.Join(err, d.Stop())
	}
	wctx, cancel := context.WithTimeout(ctx, daemonStartTimeout)
	defer cancel()
	err = WaitDaemon(wctx, d.Done(), func(ctx context.Context) error {
		c, derr := p.Dial(ctx)
		if derr == nil {
			n.Docker = c
		}
		return derr
	})
	if err != nil {
		return fail(err)
	}
	spec := &docker.NetworkSpec{Name: JobNetwork, Bridge: JobBridge, Subnet: n.Subnet, Subnet6: n.Job6}
	if err := n.Docker.EnsureNetwork(ctx, spec); err != nil {
		return fail(fmt.Errorf("creating the job network: %w", err))
	}
	return nil
}

// Hold runs the guard until ctx ends, answering nil, or until a restore
// fails or the daemon exits, answering why; on a failed restore it stops the
// daemon first, since jobs may no longer run under the host's policy.
func (n *Net) Hold(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-n.Daemon.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	err := n.Guard.Run(ctx)
	select {
	case <-n.Daemon.Done():
		return fmt.Errorf("dockerd exited: %w", errors.Join(err, n.Daemon.Err()))
	default:
	}
	if err != nil {
		return errors.Join(err, n.Daemon.Stop())
	}
	return nil
}

// Stop stops the daemon, which stops each runner.
func (n *Net) Stop() error { return n.Daemon.Stop() }

// Reload swaps the host's rules for section h, read again on SIGHUP. A
// section that changes what only a restart applies, or whose resolvers or
// allow entries overlap the job subnet in use, keeps the old rules: kept
// says why, and Kept answers it until a later Reload succeeds. fatal is a
// failure the guard would answer, and the caller stops the host.
func (n *Net) Reload(ctx context.Context, h *config.Host) (kept, fatal error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	kept = restartOnly(n.host, h)
	if c := netsetup.Conflicts(n.Subnet, h); len(c) > 0 {
		kept = errors.Join(kept, fmt.Errorf("the job subnet in use %s", strings.Join(c, "; ")))
	}
	if kept == nil {
		kept, fatal = n.Guard.Swap(ctx, n.want(h))
	}
	if kept == nil && fatal == nil {
		n.host = h
	}
	n.kept = kept
	return kept, fatal
}

// Kept answers why the last Reload kept the old rules, or nil.
func (n *Net) Kept() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.kept
}

// restartOnly names the keys whose change the running host cannot apply:
// they decide the job network's shape and which namespace settings and
// resolver the host runs.
func restartOnly(old, h *config.Host) error {
	var keys []string
	at := "hosts." + h.Name + "."
	if old.Egress.Open != h.Egress.Open {
		keys = append(keys, at+"egress.open")
	}
	if old.Egress.Internet != h.Egress.Internet {
		keys = append(keys, at+"egress.internet")
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
