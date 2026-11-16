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
	"github.com/cplieger/manade/internal/hostlink"
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

// ErrRestart ends the Run of a host that restarts to apply its section:
// the caller exits with a non-zero code, so the container's restart policy
// boots it again on the section it saved.
var ErrRestart = errors.New("the host restarts to apply its section")

// Options are what a host runs with beyond its environment.
type Options struct {
	Log    *slog.Logger
	Health *health.Status
	// Version is the host's build version.
	Version string
}

// The phases of a host's run.
const (
	// phaseWaiting has no section yet, so no daemon and no job.
	phaseWaiting int32 = iota
	// phaseBooting brings the first section up.
	phaseBooting
	// phaseUp serves the section it runs.
	phaseUp
)

// supervisor is one run of a host.
type supervisor struct {
	self    *config.Self
	outward hostapi.Outward
	served  atomic.Pointer[served]
	lat     *latencies
	dk      *docker.Client
	log     *slog.Logger
	images  *images.Manager
	runners *runnerSet
	prober  *probe.Runner
	oom     *platform.OOMTotal
	net     *hostNet
	// running, pending and refused are the section the host runs, one it
	// accepted and has not applied, and the last one it refused, all under
	// sectionMu; view is their status, read without the lock.
	running *accepted
	pending *pendingSection
	refused *hostapi.SectionRefusal
	// state is the host's state, saved unless unsaved, both under
	// sectionMu.
	state hostState
	// unbootable is the section a boot of this run refused, under
	// sectionMu: its settings delivered again would restart the host into
	// the same refusal.
	unbootable *hostapi.SectionRefusal
	view       atomic.Pointer[hostapi.SectionStatus]
	// seams are the kernel and daemon the host boots on.
	seams *seams
	// arrived hands the first section delivered to a waiting boot; changed
	// wakes changes; restarting closes once the host announces its restart.
	arrived    chan *accepted
	changed    chan struct{}
	restarting chan struct{}
	// cancelRun ends the run with why, set before the host is up.
	cancelRun context.CancelCauseFunc
	// ping asks the daemon for the health verdict: dk.Ping.
	ping        func(ctx context.Context) error
	opts        *Options
	info        docker.Info
	instance    string
	sectionPath string
	plat        jobspec.Platform
	restored    atomic.Uint64
	// saveFailures counts the failed saves of state.
	saveFailures atomic.Uint64
	// sectionMu serialises deliveries and the applies they leave pending;
	// rebuilding, under it, marks a rebuild running without it.
	sectionMu  sync.Mutex
	phase      atomic.Int32
	serving    atomic.Bool
	rebuilding bool
	unsaved    bool
}

// newSupervisor is a run of host self on p, its section saved at path.
func newSupervisor(self *config.Self, opts *Options, path string, p *seams) *supervisor {
	s := &supervisor{
		self: self, log: opts.Log, opts: opts, lat: newLatencies(), instance: newInstanceID(), seams: p,
		sectionPath: path,
		arrived:     make(chan *accepted, 1), changed: make(chan struct{}, 1), restarting: make(chan struct{}),
	}
	s.publish()
	return s
}

// Run boots host self on the section it saved or, with none, on the first
// one the control plane delivers, and holds it until ctx ends, then stops its
// daemon, which stops each runner. Throughout, it keeps one connection to
// the control plane open on the hosts socket and serves the host API over
// it. Any failure before the host is up leaves no daemon; a guard or daemon
// failure later stops the daemon and answers why, and a restart to apply a
// section answers ErrRestart.
func Run(ctx context.Context, self *config.Self, opts *Options) error {
	return newSupervisor(self, opts, sectionPath, systemSeams(self, opts.Log)).runLinked(ctx, dialSocket(hostlink.SocketPath))
}

// runLinked is Run with the channel dialed by dial. The saved section is
// read before the first dial, so every upgrade names its revision; the
// channel and keepSaving outlive ctx until the run has stopped, so the
// control plane reads the host through its shutdown.
func (s *supervisor) runLinked(ctx context.Context, dial func(context.Context) (net.Conn, error)) error {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s.bootSaved(ctx)
	up := hostlink.NewUplink(&hostlink.UplinkOptions{
		Dial: dial, Hello: s.hello, Handler: hostapi.NewServer(&backend{s: s}, s.instance, s.log).Handler(), Log: s.log,
	})
	uctx, stopUplink := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { up.Run(uctx) })
	wg.Go(func() { s.keepSaving(uctx) })
	s.log.Info("The runner host dials the control plane", "host", s.self.Name, "instance", s.instance, "version", s.opts.Version)
	err := s.run(ctx, rctx, cancel)
	stopUplink()
	wg.Wait()
	return err
}

// hello is what each upgrade says of the host: the revision named is the
// running section's, none before the first.
func (s *supervisor) hello() hostlink.Hello {
	h := hostlink.Hello{Name: s.self.Name, Build: s.opts.Version, Instance: s.instance}
	if st := s.view.Load(); st.Hash != "" {
		h.Section = strconv.FormatUint(st.Revision, 10)
	}
	return h
}

// run boots on the first section and holds the host until rctx ends,
// answering nil when ctx ended it and why otherwise.
func (s *supervisor) run(ctx, rctx context.Context, cancel context.CancelCauseFunc) error {
	if s.awaitSection(rctx) == nil {
		return ended(ctx, rctx)
	}
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
	if s.oom, err = platform.LoadOOMTotal(rctx, oomFile); err != nil {
		s.log.Warn("The saved OOM kill total was not read, so the total counts from this start", "error", err)
	}
	if s.net, err = s.boot(rctx); err != nil {
		return err
	}
	if s.net == nil {
		return ended(ctx, rctx)
	}
	err = s.serve(rctx, cancel, cg, ms)
	if err == nil {
		err = ended(ctx, rctx)
	}
	return errors.Join(err, s.net.stop())
}

// ended answers why rctx, a run under ctx, ended: nil when ctx did.
func ended(ctx, rctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	return context.Cause(rctx)
}

// boot brings the host's network and daemon up on the section it boots on,
// nil when ctx ends first. A section the host's network refuses there, or a
// section not yet booted on this host that fails a later step of the boot
// (stepError), is refused as a delivery is: the host boots on the section
// it ran before or, with none, waits for another. A namespace a failed boot
// wrote that the next section cannot run in, or that a waiting host would
// judge deliveries in, ends the run with ErrRestart once the refusal is
// saved for the next start. Any other failure ends the run with its cause.
func (s *supervisor) boot(ctx context.Context) (*hostNet, error) {
	hooks := guardHooks{Restored: s.onRestored, Checked: s.onChecked}
	off, err := netsetup.IPv6Off(s.seams.Root)
	if err != nil {
		return nil, err
	}
	b := &booting{on6: !off}
	for {
		next := s.awaitSection(ctx)
		if next == nil {
			return nil, nil
		}
		s.sectionMu.Lock()
		unbooted := s.state.Unbooted
		s.sectionMu.Unlock()
		n, err := bringUp(ctx, next.host, s.seams, hooks)
		errs := s.refusalOf(ctx, err, b, unbooted)
		if errs == nil {
			if err == nil && unbooted {
				s.cameUp(ctx)
			}
			return n, err
		}
		if err := s.bootRefused(ctx, b, next, errs); err != nil {
			return nil, err
		}
	}
}

// cameUp marks the section the host booted on, which was not booted here
// before, as booted.
func (s *supervisor) cameUp(ctx context.Context) {
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	s.state = hostState{Section: s.state.Section}
	_ = s.save(context.WithoutCancel(ctx))
}

// refusalOf answers the field errors of a boot's failure err that refuse
// its section: the facts check's, or a later step's of a section not yet
// booted on this host, which b records; nil when err is the host's own,
// a stop's among them.
func (s *supervisor) refusalOf(ctx context.Context, err error, b *booting, unbooted bool) []hostapi.FieldError {
	if sr, ok := errors.AsType[*hostapi.SectionRefusedError](err); ok {
		return sr.Errors
	}
	se, ok := errors.AsType[*stepError](err)
	if !ok || !unbooted || ctx.Err() != nil {
		return nil
	}
	b.written = true
	return []hostapi.FieldError{{Path: "hosts." + s.self.Name + "." + se.at, Message: se.Error()}}
}

// booting is what the boots of one run left in the namespace.
type booting struct {
	// on6 is IPv6 on in the namespace as the run found it; written is a
	// failed boot's steps past the facts check.
	on6     bool
	written bool
}

// bootRefused records next, the section a boot refused with errs, and
// makes its fallback the section the host boots on, or with none leaves the
// host waiting with no section, both saved in one state. It answers
// ErrRestart when the namespace b left cannot boot the fallback, or a
// waiting host would read it, as a fresh one would; a restart whose state
// was not saved would boot the refused section again, so the host then
// waits here with the refusal instead.
func (s *supervisor) bootRefused(ctx context.Context, b *booting, next *accepted, errs []hostapi.FieldError) error {
	ctx = context.WithoutCancel(ctx)
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	fb := s.fallback()
	restart := b.written && (fb == nil || s.ipv6Lost(b, fb.host))
	s.refuse(&next.raw, &hostapi.SectionRefusedError{Errors: errs})
	s.unbootable = s.refused
	st := hostState{}
	if fb != nil {
		st.Section = &fb.raw
	}
	if restart {
		st.Refused = s.refused
	}
	s.state = st
	saved := s.save(ctx) == nil
	s.running = fb
	if restart {
		s.running = nil
	}
	switch {
	case restart && saved:
		s.log.Warn("The failed boot left the host's namespace unlike a fresh one, so the host restarts", "hash", next.hash)
		return ErrRestart
	case restart:
		s.log.Error("The failed boot left the host's namespace unlike a fresh one, but the host stays, since a restart would boot the refused section again", "hash", next.hash)
	case fb == nil:
		s.log.Warn("The host refused the section it was booting on, so it waits for the control plane's", "hash", next.hash)
	default:
		s.log.Warn("The host refused the section it was booting on, so it boots on the section it ran before", "hash", next.hash, "fallback", fb.hash)
	}
	if s.running == nil {
		s.phase.Store(phaseWaiting)
	}
	s.publish()
	return nil
}

// ipv6Lost reports whether a failed boot turned off IPv6, which section h
// keeps on and only a fresh namespace gives back with its addresses.
func (s *supervisor) ipv6Lost(b *booting, h *config.Host) bool {
	if !b.on6 || (!h.Egress.IPv6 && !h.Egress.Open) {
		return false
	}
	off, err := netsetup.IPv6Off(s.seams.Root)
	return err != nil || off
}

// fallback answers the section the host ran before the one it boots, which
// the state holds only while that one has not booted here, when it still
// validates. A fallback refused in turn is booted, so it has none.
// sectionMu is held.
func (s *supervisor) fallback() *accepted {
	if s.state.Fallback == nil {
		return nil
	}
	fb, err := check(s.self, s.state.Fallback)
	if err != nil {
		s.log.Warn("The fallback section no longer validates", "error", err)
		return nil
	}
	return fb
}

// bootSaved loads the host's state: the section it boots on, when it
// still validates, and the refusal a boot saved before restarting, whose
// settings are refused for this run only, so it leaves the state at once.
// A refusal stays in the file while the state cannot be saved; saving any
// change to the host's settings, or keepSaving, takes it out.
func (s *supervisor) bootSaved(ctx context.Context) {
	st, ok, err := readState(ctx, s.sectionPath)
	if err != nil {
		s.log.Error("The host's state could not be read, so it waits for the control plane's section", "error", err)
	}
	if !ok {
		return
	}
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	s.state = st
	if r := st.Refused; r != nil {
		s.refused, s.unbootable = r, r
		s.state.Refused = nil
		_ = s.save(context.WithoutCancel(ctx))
		s.publish()
	}
	next := s.savedSection(st.Section)
	if next == nil || s.phase.Load() != phaseWaiting {
		return
	}
	s.running = next
	s.phase.Store(phaseBooting)
	s.publish()
	s.log.Info("The host boots on the section it saved", "hash", next.hash, "revision", next.raw.Revision)
}

// awaitSection answers the section the host boots on: bootSaved's, or else
// the first one delivered; nil when ctx ends first.
func (s *supervisor) awaitSection(ctx context.Context) *accepted {
	s.sectionMu.Lock()
	booting, running := s.phase.Load() == phaseBooting, s.running
	if booting {
		// A delivery that came in after the boot's last refusal hands its
		// section to arrived too: left there, the next delivery to a
		// waiting host would block on it.
		select {
		case <-s.arrived:
		default:
		}
	}
	s.sectionMu.Unlock()
	if booting {
		return running
	}
	s.log.Info("The host waits for its first settings from the control plane", "host", s.self.Name)
	select {
	case <-ctx.Done():
		return nil
	case next := <-s.arrived:
		return next
	}
}

// savedSection answers sec, the section the state boots, nil when there is
// none or it no longer validates, which the host reports; sectionMu is
// held.
func (s *supervisor) savedSection(sec *hostapi.Section) *accepted {
	if sec == nil {
		return nil
	}
	next, err := check(s.self, sec)
	if err == nil {
		return next
	}
	s.log.Error("The saved section does not validate, so the host waits for the control plane's", "error", err)
	if sr, ok := errors.AsType[*hostapi.SectionRefusedError](err); ok {
		s.refuse(sec, sr)
	}
	return nil
}

// serve brings up everything above a host whose daemon is up and holds it
// until rctx ends or a part fails.
func (s *supervisor) serve(rctx context.Context, cancel context.CancelCauseFunc, cg *platform.Cgroup, ms []platform.Mount) error {
	if err := platform.SetOOMScoreAdj(platform.System, s.net.Daemon.PID(), platform.HostOOMScoreAdj); err != nil {
		return fmt.Errorf("oom_score_adj of dockerd: %w", err)
	}
	var err error
	if s.dk, err = docker.Dial(rctx, daemonSocket, s.lat.observe); err != nil {
		return err
	}
	defer s.dk.Close()
	s.ping = s.dk.Ping
	if s.info, err = s.dk.Info(rctx); err != nil {
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
	h := s.running.host
	sv := s.newServed(s.running.cfg, h)
	s.served.Store(sv)
	s.warnNoMemoryLimit(sv, cg)
	if err := s.build(rctx, h, sv, cg); err != nil {
		return err
	}
	s.hold(rctx, cancel)
	return nil
}

// warnNoMemoryLimit warns once of a host cgroup with no memory limit, which
// admits no job memory, naming the reserve a limit must leave.
func (s *supervisor) warnNoMemoryLimit(sv *served, cg *platform.Cgroup) {
	if cg.Memory != 0 {
		return
	}
	m := memoryStatus(cg, sv.Profiles)
	s.log.Warn("The host has no memory limit, so it admits no job memory: give its container a memory limit, of which jobs get all but the reserve",
		"reserve_bytes", m.Reserve)
}

// build makes the image manager, the runner record and the prober, and
// loads the probe image and the daemon's runner containers. ctx is the
// host's run, which each runner's work runs under.
func (s *supervisor) build(ctx context.Context, h *config.Host, sv *served, cg *platform.Cgroup) error {
	s.images = images.New(&images.Config{
		Daemon: s.dk, Host: h, RegistryAuth: s.self.RegistryAuth, Lookup: lookupRegistry, ByName: s.net.gitHubName, Log: s.log,
		Keep:    images.Keep{Runner: func(n string) bool { return s.runners.holds(n) }, ProbeLeftover: func(r string) bool { return s.prober.Leftover(r) }},
		Version: s.opts.Version, StateDir: stateDir, Binary: binaryPath, Profiles: sv.Profiles,
		Tidied: func() { s.runners.tidied() },
	})
	var cpus []int
	if !cg.Quota {
		var err error
		if cpus, err = platform.ParseCPUList(cg.CPUSet); err != nil {
			return fmt.Errorf("the host's cpuset %q: %w", cg.CPUSet, err)
		}
	}
	jobs := &jobDaemon{Client: s.dk, network: jobNetworkSpec(s.net.Subnet, s.net.Job6), log: s.log}
	s.runners = newRunners(ctx, &runnersConfig{Daemon: jobs, Images: s.images, Served: sv, Log: s.log, CPUs: cpus, DataFree: dataFree})
	if err := s.runners.load(ctx); err != nil {
		return err
	}
	s.prober = probe.NewRunner(&probe.Config{
		Daemon: jobs, Attach: func(ctx context.Context, id string) (probe.Stream, error) { return s.dk.Attach(ctx, id) },
		Sets: &probeSets{rules: s.net.rules, guard: s.net.rulesGuard, ipv6: h.Egress.IPv6}, Image: s.images,
		Spec: func() *jobspec.Host { return s.served.Load().Spec }, Host: s.probeHost,
		Log: s.log, Network: jobNetwork, Started: time.Now(),
	})
	if _, err := s.images.LoadProbe(ctx); err != nil {
		return err
	}
	return nil
}

// hold runs the host's parts until rctx ends or a part fails, which
// cancels it with why, then stops each part and waits for it.
func (s *supervisor) hold(rctx context.Context, cancel context.CancelCauseFunc) {
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := s.net.hold(rctx); err != nil {
			cancel(err)
		}
	})
	wg.Go(func() { s.images.Run(rctx) })
	wg.Go(func() { s.runners.run(rctx) })
	wg.Go(func() { s.changes(rctx, cancel) })
	s.sectionMu.Lock()
	s.cancelRun = cancel
	s.phase.Store(phaseUp)
	s.sectionMu.Unlock()
	s.serving.Store(true)
	s.log.Info("The runner host is up on its section", "host", s.self.Name, "hash", s.running.hash)
	s.warnOpen()
	<-rctx.Done()
	s.serving.Store(false)
	if s.opts.Health != nil {
		s.opts.Health.BeginDrain()
	}
	wg.Wait()
}

func (s *supervisor) warnOpen() {
	if h := s.host(); h.Egress.Open {
		s.log.Warn("The host is open: its jobs reach any destination and its proof skips the block checks", "host", h.Name)
	}
}

// apply makes cfg what the host serves.
func (s *supervisor) apply(cfg *config.Config, h *config.Host) {
	sv := s.newServed(cfg, h)
	s.served.Store(sv)
	s.runners.setServed(sv)
	s.images.Reload(h, sv.Profiles)
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
// the rules in place: healthy while the daemon answers and the host serves
// its section.
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
		Host: h, OutwardGateway: n.Gateway4, Original: n.Original,
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

// systemSeams is the real kernel and daemon, with host self's echo
// endpoint and MANADE_BLOCK6.
func systemSeams(self *config.Self, log *slog.Logger) *seams {
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
		ListenDNS: listenDNS, FetchDomains: resolver.FetchMeta(httpclient.New(metaTimeout)), Upstreams: resolver.Upstreams, Echo: echoOverIPv6(), EchoURL: self.IPv6Echo, Block6: self.Ceiling.Block6,
		Log: log, Root: platform.System,
	}
}

// lookupRegistry resolves a registry's name through the host's resolver,
// as its daemon's pull does.
func lookupRegistry(ctx context.Context, name string) ([]netip.Addr, error) {
	return netsetup.LookupVia(ctx, netsetup.ForwarderAddr, name)
}

func dataFree() (uint64, error) {
	u, err := platform.DataUsage(dataRoot)
	return u.Free, err
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
