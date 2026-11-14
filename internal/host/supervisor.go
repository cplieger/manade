package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/images"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/host/probe"
	"github.com/cplieger/manade/internal/host/resolver"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// The host image's own executable, the probe image's one file, and the
// file the OOM kill total is saved in.
const (
	binaryPath = "/manade"
	oomFile    = stateDir + "/oom-kills.json"
)

// metaTimeout bounds a read of GitHub's meta endpoint.
const metaTimeout = 30 * time.Second

// Options are what a host runs with beyond its config.
type Options struct {
	Log *slog.Logger
	// Reload loads the config again on a SIGHUP.
	Reload func(ctx context.Context) (*config.Config, error)
	// Hangup delivers each SIGHUP.
	Hangup <-chan os.Signal
	Health *health.Status
	// Version is the host's build version.
	Version string
}

// supervisor is one run of a host.
type supervisor struct {
	outward   hostapi.Outward
	served    atomic.Pointer[served]
	lat       *latencies
	dk        *docker.Client
	log       *slog.Logger
	images    *images.Manager
	runners   *runnerSet
	prober    *probe.Runner
	oom       *platform.OOMTotal
	net       *hostNet
	configErr atomic.Pointer[string]
	// ping asks the daemon for the health verdict: dk.Ping.
	ping     func(ctx context.Context) error
	opts     *Options
	info     docker.Info
	instance string
	plat     jobspec.Platform
	restored atomic.Uint64
	reloadMu sync.Mutex
	serving  atomic.Bool
	apiPort  uint16
}

// Run brings host cfg.Self up, serves its API until ctx ends and then stops
// its daemon, which stops each runner. Any failure before the API serves
// leaves no daemon; a guard or daemon failure later stops the daemon and
// answers why.
func Run(ctx context.Context, cfg *config.Config, opts *Options) error {
	h, ok := cfg.Host(cfg.Self.Name)
	if !ok {
		return fmt.Errorf("MANADE_HOST %q names no hosts entry", cfg.Self.Name)
	}
	s := &supervisor{log: opts.Log, opts: opts, lat: newLatencies(), instance: newInstanceID()}
	ms, err := platform.Mountinfo(platform.System)
	if err != nil {
		return err
	}
	for _, pid := range []int{1, os.Getpid()} {
		if err = platform.SetOOMScoreAdj(platform.System, pid, platform.HostOOMScoreAdj); err != nil {
			return fmt.Errorf("oom_score_adj of PID %d: %w", pid, err)
		}
	}
	cg, err := platform.Read(platform.System)
	if err != nil {
		return err
	}
	if s.oom, err = platform.LoadOOMTotal(ctx, oomFile); err != nil {
		s.log.Warn("The saved OOM kill total was not read, so the total counts from this start", "error", err)
	}
	if s.net, err = bringUp(ctx, h, s.platform(), guardHooks{Restored: s.onRestored, Checked: s.onChecked}); err != nil {
		return err
	}
	err = s.serve(ctx, cfg, h, cg, ms)
	return errors.Join(err, s.net.stop())
}

// serve runs everything above a host whose daemon is up, until ctx ends or
// a part fails.
func (s *supervisor) serve(ctx context.Context, cfg *config.Config, h *config.Host, cg *platform.Cgroup, ms []platform.Mount) error {
	if err := platform.SetOOMScoreAdj(platform.System, s.net.Daemon.PID(), platform.HostOOMScoreAdj); err != nil {
		return fmt.Errorf("oom_score_adj of dockerd: %w", err)
	}
	var err error
	if s.dk, err = docker.Dial(ctx, daemonSocket, s.lat.observe); err != nil {
		return err
	}
	defer s.dk.Close()
	s.ping = s.dk.Ping
	usage, err := platform.DataUsage(dataRoot)
	if err != nil {
		return fmt.Errorf("reading the data volume: %w", err)
	}
	if derr := checkDisk(h, usage.Size); derr != nil {
		return derr
	}
	if s.info, err = s.dk.Info(ctx); err != nil {
		return fmt.Errorf("reading the host daemon's info: %w", err)
	}
	s.log.Info("The host's daemon answered", "version", s.info.ServerVersion, "api_version", s.dk.APIVersion(),
		"storage_driver", s.info.Driver, "rule_backend", backendName(s.net.Kernel.Backend))
	gid, err := socketGID()
	if err != nil {
		return err
	}
	s.plat = jobspec.Platform{
		Network: jobNetwork, Mounts: platform.StartMounts(ms), CPUs: cg.CPUs, SocketGID: gid,
		Pinning: !cg.Quota, PIDs: cg.PIDsController, Swap: cg.Swap,
		Disk: s.net.Storage.Driver == platform.Overlay2 && platform.ProjectQuota(ms, dataRoot),
	}
	s.outward = s.outwardAddrs()
	if s.apiPort, err = apiPort(cfg.Self.Listen); err != nil {
		return err
	}
	sv := s.newServed(cfg, h)
	s.served.Store(sv)
	if berr := s.build(ctx, h, sv, cg); berr != nil {
		return berr
	}
	ln, err := listen(ctx, cfg.Self)
	if err != nil {
		return err
	}
	return s.run(ctx, ln)
}

// build makes the image manager, the runner record and the prober, and
// loads the probe image and the daemon's runner containers.
func (s *supervisor) build(ctx context.Context, h *config.Host, sv *served, cg *platform.Cgroup) error {
	s.images = images.New(&images.Config{
		Daemon: s.dk, Host: h, Lookup: s.lookup, ByName: s.net.gitHubName, Log: s.log,
		Keep:    images.Keep{Runner: func(n string) bool { return s.runners.holds(n) }, ProbeRun: func(r string) (time.Time, bool) { return s.prober.Ended(r) }},
		Version: s.opts.Version, StateDir: stateDir, Binary: binaryPath, Profiles: sv.Profiles,
	})
	var cpus []int
	if !cg.Quota {
		var err error
		if cpus, err = platform.ParseCPUList(cg.CPUSet); err != nil {
			return fmt.Errorf("the host's cpuset %q: %w", cg.CPUSet, err)
		}
	}
	s.runners = newRunners(&runnersConfig{Daemon: s.dk, Images: s.images, Served: sv, Log: s.log, CPUs: cpus})
	if err := s.runners.load(ctx); err != nil {
		return err
	}
	s.prober = probe.NewRunner(&probe.Config{
		Daemon: s.dk, Attach: func(ctx context.Context, id string) (probe.Stream, error) { return s.dk.Attach(ctx, id) },
		Sets: &probeSets{guard: s.net.Guard, ipv6: h.Egress.IPv6}, Image: s.images,
		Spec: func() *jobspec.Host { return s.served.Load().Spec }, Host: s.probeHost,
		Log: s.log, Network: jobNetwork, Started: time.Now(),
	})
	if _, err := s.images.LoadProbe(ctx); err != nil {
		return err
	}
	return nil
}

// run serves ln and holds the host until ctx ends or a part fails, then
// stops each part and waits for it.
func (s *supervisor) run(ctx context.Context, ln net.Listener) error {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := s.net.hold(rctx); err != nil {
			cancel(err)
		}
	})
	wg.Go(func() { s.images.Run(rctx) })
	wg.Go(func() { s.runners.run(rctx) })
	wg.Go(func() { s.hangups(rctx, cancel) })
	srv := hostapi.NewServer(&backend{s: s}, s.instance, s.log)
	s.serving.Store(true)
	s.log.Info("The runner host serves its API", "host", s.served.Load().Host, "instance", s.instance, "version", s.opts.Version)
	s.warnOpen()
	err := srv.Serve(rctx, ln)
	s.serving.Store(false)
	if s.opts.Health != nil {
		s.opts.Health.BeginDrain()
	}
	if err == nil {
		err = context.Canceled
	}
	cancel(err)
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return context.Cause(rctx)
}

func (s *supervisor) warnOpen() {
	if h := s.host(); h.Egress.Open {
		s.log.Warn("The host is open: its jobs reach any destination and its proof skips the block checks", "host", h.Name)
	}
}

// hangups reloads the config on each SIGHUP until ctx ends; a reload the
// host cannot survive cancels ctx with why.
func (s *supervisor) hangups(ctx context.Context, cancel context.CancelCauseFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.opts.Hangup:
			if err := s.reload(ctx); err != nil {
				cancel(err)
				return
			}
		}
	}
}

// reload reads the config again and swaps the host's rules and what it
// serves. A config or section that does not validate keeps all of it, and
// status says why; only a failure a restore would answer is returned.
func (s *supervisor) reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	cfg, err := s.opts.Reload(ctx)
	if err == nil {
		if _, ok := cfg.Host(s.served.Load().Host); !ok {
			err = fmt.Errorf("the config no longer has hosts.%s", s.served.Load().Host)
		}
	}
	if err != nil {
		s.keep(fmt.Errorf("the reloaded config does not validate: %w", err))
		return nil
	}
	h, _ := cfg.Host(s.served.Load().Host)
	kept, fatal := s.net.reload(ctx, h)
	if fatal != nil {
		return fatal
	}
	if kept != nil {
		s.keep(kept)
		return nil
	}
	sv := s.newServed(cfg, h)
	s.served.Store(sv)
	s.runners.setServed(sv)
	s.images.Reload(h, sv.Profiles)
	s.configErr.Store(nil)
	s.log.Info("The host reloaded its config", "host", h.Name)
	return nil
}

func (s *supervisor) keep(err error) {
	msg := err.Error()
	s.configErr.Store(&msg)
	s.log.Error("The host kept its old config and rules", "error", err)
}

func (s *supervisor) newServed(cfg *config.Config, h *config.Host) *served {
	sv := &served{Config: cfg, Host: h.Name, Spec: jobspec.NewHost(cfg, h, &s.plat)}
	sv.Profiles = s.checked(cfg, h, sv.Spec)
	return sv
}

func (s *supervisor) host() *config.Host {
	sv := s.served.Load()
	h, _ := sv.Config.Host(sv.Host)
	return h
}

// checked answers the profiles host h serves whose limits it can enforce;
// each refused one is logged.
func (s *supervisor) checked(cfg *config.Config, h *config.Host, spec *jobspec.Host) []*config.Profile {
	var out []*config.Profile
	for _, p := range servedProfiles(cfg, h.Name) {
		if err := spec.Check(p); err != nil {
			s.log.Warn("A profile is refused on this host", "profile", p.Name, "error", err)
			continue
		}
		if g := p.Grants(); len(g) > 0 {
			s.log.Warn("A profile grants elevated access, so its jobs reach past the default isolation", "profile", p.Name, "grants", strings.Join(g, ","))
		}
		out = append(out, p)
	}
	return out
}

func (s *supervisor) onRestored(string) { s.restored.Add(1) }

// healthAsk bounds the daemon's answer the health verdict asks for.
const healthAsk = 3 * time.Second

// onChecked records the health verdict after each guard check that left
// the rules in place: healthy while the daemon answers and the API serves.
func (s *supervisor) onChecked() {
	if s.opts.Health == nil {
		return
	}
	ok := s.serving.Load() && s.ping != nil
	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), healthAsk)
		ok = s.ping(ctx) == nil
		cancel()
	}
	s.opts.Health.Set(ok)
}

// probeHost is what a probe run reads of the host at its start.
func (s *supervisor) probeHost() probe.Host {
	h := s.host()
	n := s.net
	in := egress.ProbeInput{
		Host: h, OutwardGateway: n.Gateway4, Original: n.Original, APIPort: s.apiPort,
	}
	ph := probe.Host{Sources: []netip.Addr{s.outward.Address}}
	if h.Egress.IPv6 {
		in.OutwardGateway6, in.Own6 = n.Gateway6, n.Addr6
		ph.Sources = append(ph.Sources, n.Addr6)
		if st := n.iPv6Status(); st != nil {
			in.Self64 = st.Prefix
			if !st.Prefix.IsValid() {
				ph.No64 = st.Reason
			}
		}
	}
	ph.Input = in
	return ph
}

// outwardAddrs reads the host container's own addresses toward its
// gateways: a UDP socket's local address after a connect, which sends
// nothing.
func (s *supervisor) outwardAddrs() hostapi.Outward {
	out := hostapi.Outward{Gateway: s.net.Gateway4, Gateway6: s.net.Gateway6, Address6: s.net.Addr6}
	if gw := s.net.Gateway4; gw.IsValid() {
		if c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(gw, 1))); err == nil {
			if la, ok := c.LocalAddr().(*net.UDPAddr); ok {
				out.Address = la.AddrPort().Addr().Unmap()
			}
			_ = c.Close()
		}
	}
	return out
}

// platform is the real kernel and daemon.
func (s *supervisor) platform() *seams {
	echo, err := echoOverIPv6()
	if err != nil {
		s.log.Warn("The IPv6 echo client was not built; an ipv6 host learns no /64", "error", err)
	}
	return &seams{
		Run: platform.Exec, Lookup: netsetup.LookupVia, Random: rand.Reader,
		PickStorage: func(dir string) (platform.StoragePick, error) {
			return platform.PickStorage(platform.System, dir, platform.ForHost)
		},
		StartDaemon: func(spec *daemonSpec) (process, error) {
			return startDaemon(context.Background(), spec.argv(), spec.env(os.Environ()), os.Stderr)
		},
		Dial: func(ctx context.Context) (jobNetworks, error) {
			c, err := docker.Dial(ctx, daemonSocket, nil)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		ListenDNS: listenDNS, FetchDomains: resolver.FetchMeta(httpclient.New(metaTimeout)), Echo: echo,
		Log: s.log, Root: platform.System,
	}
}

// lookup resolves a registry's name through the resolvers the host's
// resolv.conf names, as its daemon's pull does.
func (s *supervisor) lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	var errs []error
	for _, server := range netsetup.Nameservers(s.host()) {
		addrs, err := netsetup.LookupVia(ctx, server, name)
		switch {
		case err != nil:
			errs = append(errs, err)
		case len(addrs) == 0:
			errs = append(errs, fmt.Errorf("%s answered no address", server))
		default:
			return addrs, nil
		}
	}
	return nil, fmt.Errorf("resolving %s: %w", name, errors.Join(errs...))
}

// socketGID is the group that owns the daemon's socket, for a host-socket
// job.
func socketGID() (uint32, error) {
	st, err := os.Stat(daemonSocket)
	if err != nil {
		return 0, fmt.Errorf("the host daemon's socket: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("the host daemon's socket: no owner on this platform")
	}
	return sys.Gid, nil
}

// apiPort is the API's TCP port, 0 for a Unix socket.
func apiPort(a config.Address) (uint16, error) {
	if a.HostPort == "" {
		return 0, nil
	}
	_, port, err := net.SplitHostPort(a.HostPort)
	if err != nil {
		return 0, fmt.Errorf("MANADE_LISTEN %q: %w", a.HostPort, err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("MANADE_LISTEN %q: %w", a.HostPort, err)
	}
	return uint16(n), nil
}

// listen opens the API's listener: the Unix socket owned by APIUID, or TCP
// with mutual TLS from TLSDir.
func listen(ctx context.Context, self *config.Self) (net.Listener, error) {
	if self.Listen.Socket != "" {
		return hostapi.ListenUnix(self.Listen.Socket, self.APIUID)
	}
	tc, err := hostapi.ServerTLS(self.TLSDir, self.Name)
	if err != nil {
		return nil, err
	}
	return hostapi.ListenTLS(ctx, self.Listen.HostPort, tc)
}

func newInstanceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HealthMarker is the host's health marker, which the guard's check keeps
// fresh; HealthLease is how old it may grow: three missed checks and one
// slow check's worth.
var (
	HealthMarker = health.MarkerPath("manade-host")
	HealthLease  = health.Lease{Interval: checkInterval, Timeout: 30 * time.Second, Cycles: 3, Attempts: 1}
)
