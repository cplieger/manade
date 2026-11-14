package host

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/host/resolver"
)

// route4 has a default route via 172.17.0.1 and the on-link 172.17.0.0/16,
// little-endian words as /proc/net/route prints them.
const route4 = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
	"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
	"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"

// fakeProcess is a daemon that runs until stopped or exited.
type fakeProcess struct {
	done    chan struct{}
	err     error
	stopped int
	mu      sync.Mutex
}

func newProcess() *fakeProcess { return &fakeProcess{done: make(chan struct{})} }

func (p *fakeProcess) Done() <-chan struct{} { return p.done }

func (*fakeProcess) PID() int     { return 4242 }
func (p *fakeProcess) Err() error { return p.err }

func (p *fakeProcess) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped++
	p.exit(nil)
	return nil
}

func (p *fakeProcess) exit(err error) {
	select {
	case <-p.done:
	default:
		p.err = err
		close(p.done)
	}
}

type fakeNetworks struct {
	err    error
	spec   *docker.NetworkSpec
	closed bool
}

func (n *fakeNetworks) EnsureNetwork(_ context.Context, spec *docker.NetworkSpec) error {
	n.spec = spec
	return n.err
}

func (n *fakeNetworks) Close() { n.closed = true }

// upFixture is a floor-like host: legacy iptables without raw or IPv6 nat,
// no bridge sysctls, cgroup v1, IPv6 on in its namespace, Docker's
// resolv.conf naming the machine's resolver.
type upFixture struct {
	lookupErr error
	startErr  error
	fetchErr  error
	echo      echoFunc
	dnsUDP    *idleConn
	dnsTCP    *idleConn
	kernel    *fakeIPT
	proc      *fakeProcess
	nets      *fakeNetworks
	spec      *daemonSpec
	root      platform.Root
	lookups   []string
	events    []string
}

func newUpFixture(t *testing.T) *upFixture {
	t.Helper()
	return &upFixture{
		kernel: newFakeIPT(false),
		root: tree(t, map[string]string{
			"/etc/resolv.conf":     "# ExtServers: [host(192.168.1.1)]\nnameserver 127.0.0.11\n",
			"/proc/net/route":      route4,
			disableAll:             "0\n",
			disableDefault:         "0\n",
			"/proc/self/cgroup":    "4:memory:/docker/abc\n",
			"/proc/self/mountinfo": "",
		}),
		proc:   newProcess(),
		nets:   &fakeNetworks{},
		dnsUDP: newIdleConn(),
		dnsTCP: newIdleConn(),
	}
}

func (f *upFixture) platform() *seams {
	return &seams{
		Root: f.root,
		Run: func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
			if strings.HasSuffix(name, "-restore") {
				f.events = append(f.events, "rules")
			}
			return f.kernel.run(ctx, stdin, name, args...)
		},
		Lookup: func(_ context.Context, s netip.Addr, name string) ([]netip.Addr, error) {
			f.events = append(f.events, "lookup")
			f.lookups = append(f.lookups, s.String()+" "+name)
			return []netip.Addr{netip.MustParseAddr("140.82.112.6")}, f.lookupErr
		},
		Random:      bytes.NewReader([]byte{1, 2, 3, 4, 5}),
		PickStorage: func(string) (platform.StoragePick, error) { return platform.StoragePick{Driver: platform.Btrfs}, nil },
		StartDaemon: func(spec *daemonSpec) (process, error) {
			f.events = append(f.events, "daemon")
			f.spec = spec
			return f.proc, f.startErr
		},
		Dial: func(context.Context) (jobNetworks, error) { return f.nets, nil },
		ListenDNS: func(context.Context) (net.PacketConn, net.Listener, error) {
			f.events = append(f.events, "resolver")
			return f.dnsUDP, f.dnsTCP, nil
		},
		FetchDomains: func(context.Context) ([]byte, error) {
			f.events = append(f.events, "domains")
			if f.fetchErr != nil {
				return nil, f.fetchErr
			}
			return []byte(metaBody), nil
		},
		Upstreams: resolver.Upstreams,
		Echo:      f.echo,
		Log:       slog.New(slog.DiscardHandler),
	}
}

func TestUp(t *testing.T) {
	t.Run("the floor, in order", func(t *testing.T) {
		f := newUpFixture(t)
		n := upNet(t, f, section("172.17.4.10/32"))
		if !slices.Equal(compact(f.events), []string{"rules", "resolver", "lookup", "daemon"}) {
			t.Errorf("events = %q, want the rules, the resolver, the lookup through it, then the daemon", compact(f.events))
		}
		if readAt(t, f.root, "/etc/resolv.conf") != resolvRendered {
			t.Errorf("resolv.conf = %q", readAt(t, f.root, "/etc/resolv.conf"))
		}
		if !slices.Equal(n.Original.Addrs, []netip.Addr{netip.MustParseAddr("192.168.1.1")}) {
			t.Errorf("Original = %v, want the machine's resolver kept for status", n.Original)
		}
		if readAt(t, f.root, disableAll) != "1\n" || readAt(t, f.root, disableDefault) != "1\n" {
			t.Error("IPv6 left on in the namespace")
		}
		if n.Subnet != pfx("172.18.0.0/16") || f.nets.spec.Subnet != n.Subnet || f.nets.spec.Bridge != jobBridge || f.nets.spec.Subnet6.IsValid() {
			t.Errorf("job network %+v, want 172.18.0.0/16 on manade0 with no IPv6", f.nets.spec)
		}
		if !f.nets.closed {
			t.Error("the bring-up's daemon client was left open")
		}
		env := f.spec.env([]string{"PATH=/usr/bin"})
		for _, want := range []string{"DOCKER_IGNORE_BR_NETFILTER_ERROR=1", "DOCKER_INSECURE_NO_IPTABLES_RAW=1", "PATH=" + platform.LegacyDir + ":/usr/bin"} {
			if !slices.Contains(env, want) {
				t.Errorf("daemon env %q lacks %s", env, want)
			}
		}
		if f.spec.Storage != platform.Btrfs {
			t.Errorf("storage = %s, want the pick", f.spec.Storage)
		}
		if !slices.Equal(f.lookups, []string{"127.0.0.1 api.github.com"}) {
			t.Errorf("lookups = %q, want the host's own resolver asked", f.lookups)
		}
		if n.gitHubStatus() != nil || n.learning() != nil {
			t.Error("an internet host learns GitHub's addresses")
		}
	})
	t.Run("a failed lookup starts no daemon and names the field", func(t *testing.T) {
		f := newUpFixture(t)
		f.lookupErr = errors.New("i/o timeout")
		_, err := bringUp(t.Context(), section(), f.platform(), guardHooks{})
		if err == nil || !strings.Contains(err.Error(), "hosts.lan.dns") || slices.Contains(f.events, "daemon") || !f.dnsUDP.isClosed() {
			t.Errorf("Up = %v, events %q, resolver closed %v, want the refusal before any daemon with the resolver stopped", err, f.events, f.dnsUDP.isClosed())
		}
	})
	t.Run("rules that do not load start no lookup and no daemon", func(t *testing.T) {
		f := newUpFixture(t)
		f.kernel.fail["ipset list -n"] = errors.New("no ip_set")
		_, err := bringUp(t.Context(), section(), f.platform(), guardHooks{})
		if err == nil || slices.Contains(f.events, "lookup") || slices.Contains(f.events, "daemon") {
			t.Errorf("Up = %v, events %q", err, f.events)
		}
	})
	t.Run("an internet: false host resolves through its own forwarder", func(t *testing.T) {
		f := newUpFixture(t)
		upNet(t, f, closedSection())
		if readAt(t, f.root, "/etc/resolv.conf") != "nameserver 127.0.0.1\n" || !slices.Equal(f.lookups, []string{"127.0.0.1 api.github.com"}) {
			t.Errorf("resolv.conf %q, lookups %q", readAt(t, f.root, "/etc/resolv.conf"), f.lookups)
		}
	})
	t.Run("a configured subnet on an on-link route", func(t *testing.T) {
		f := newUpFixture(t)
		h := section()
		h.Subnet = pfx("172.17.8.0/24")
		_, err := bringUp(t.Context(), h, f.platform(), guardHooks{})
		if err == nil || !strings.Contains(err.Error(), "172.17.0.0/16") || len(f.events) != 0 {
			t.Errorf("Up = %v, events %q, want a refusal naming the route before the rules", err, f.events)
		}
	})
	t.Run("IPv6 on the floor names what is missing", func(t *testing.T) {
		f := newUpFixture(t)
		h := section()
		h.Egress.IPv6 = true
		_, err := bringUp(t.Context(), h, f.platform(), guardHooks{})
		if err == nil || !strings.Contains(err.Error(), "IPv6 nat table") || len(f.events) != 0 {
			t.Errorf("Up = %v, events %q", err, f.events)
		}
	})
	t.Run("IPv6 jobs", func(t *testing.T) {
		f := newUpFixture(t)
		f.kernel = newFakeIPT(true)
		writeAt(t, f.root, "/proc/net/if_inet6", "fd0000c8000000000000000000000002 07 40 00 80     eth0\n")
		writeAt(t, f.root, "/proc/net/ipv6_route", "00000000000000000000000000000000 00 00000000000000000000000000000000 00 fd0000c8000000000000000000000001 00000400 00000001 00000000 00000003     eth0\n")
		writeAt(t, f.root, "/proc/sys/net/ipv6/conf/all/forwarding", "0\n")
		writeAt(t, f.root, platform.BridgeNFCall6, "0\n")
		h := section()
		h.Egress.IPv6 = true
		n := upNet(t, f, h)
		if n.Job6 != pfx("fd01:203:405::/64") || f.nets.spec.Subnet6 != n.Job6 {
			t.Errorf("Job6 = %v, network %+v", n.Job6, f.nets.spec)
		}
		if readAt(t, f.root, "/proc/sys/net/ipv6/conf/all/forwarding") != "1\n" || readAt(t, f.root, platform.BridgeNFCall6) != "1\n" {
			t.Error("forwarding or bridge-nf-call-ip6tables not set")
		}
		if readAt(t, f.root, disableAll) != "0\n" {
			t.Error("IPv6 turned off on an IPv6 host")
		}
		if !strings.Contains(readAt(t, f.root, stateDir+"/job-ipv6"), "fd01:203:405::/64") {
			t.Error("the job prefix is not saved for the next start")
		}
	})
	t.Run("a cgroup v2 host in the machine's namespace runs nothing", func(t *testing.T) {
		f := newUpFixture(t)
		writeAt(t, f.root, "/sys/fs/cgroup/cgroup.controllers", "cpu memory\n")
		writeAt(t, f.root, "/proc/self/cgroup", "0::/system.slice/docker-abc.scope\n")
		writeAt(t, f.root, "/proc/self/mountinfo", "30 25 0:26 / /sys/fs/cgroup rw,nosuid - cgroup2 cgroup2 rw\n")
		_, err := bringUp(t.Context(), section(), f.platform(), guardHooks{})
		if err == nil || !strings.Contains(err.Error(), "cgroup: private") || len(f.kernel.commands()) != 0 {
			t.Errorf("Up = %v, commands %q", err, f.kernel.commands())
		}
	})
	t.Run("a failed network create stops the daemon", func(t *testing.T) {
		f := newUpFixture(t)
		f.nets.err = errors.New("pool overlaps")
		if _, err := bringUp(t.Context(), section(), f.platform(), guardHooks{}); err == nil || f.proc.stopped != 1 || !f.dnsUDP.isClosed() {
			t.Errorf("Up = %v, stopped %d times, resolver closed %v", err, f.proc.stopped, f.dnsUDP.isClosed())
		}
	})
	t.Run("a daemon that exits before it answers", func(t *testing.T) {
		f := newUpFixture(t)
		p := f.platform()
		p.Dial = func(context.Context) (jobNetworks, error) {
			f.proc.exit(errors.New("exit status 1"))
			return nil, errors.New("connection refused")
		}
		if _, err := bringUp(t.Context(), section(), p, guardHooks{}); err == nil || !strings.Contains(err.Error(), "exited") {
			t.Errorf("Up = %v", err)
		}
	})
}

func TestReload(t *testing.T) {
	up := func(t *testing.T) (*upFixture, *hostNet) {
		t.Helper()
		f := newUpFixture(t)
		return f, upNet(t, f, section())
	}
	kept := map[string]struct {
		edit func(h *config.Host)
		want []string
	}{
		"an allow entry inside the subnet in use": {
			edit: func(h *config.Host) { *h = *section("172.18.3.4/32") }, want: []string{"172.18.0.0/16", "egress.allow[0]"},
		},
		"an allow entry containing it": {
			edit: func(h *config.Host) { *h = *section("172.16.0.0/12") }, want: []string{"172.18.0.0/16", "172.16.0.0/12"},
		},
		"a resolver inside it": {
			edit: func(h *config.Host) { h.DNS = []netip.Addr{netip.MustParseAddr("172.18.0.53")} }, want: []string{"dns[0]"},
		},
		"egress.ipv6 turned on": {
			edit: func(h *config.Host) { h.Egress.IPv6 = true }, want: []string{"hosts.lan.egress.ipv6", "restart"},
		},
	}
	for name, tc := range kept {
		t.Run(name, func(t *testing.T) {
			f, n := up(t)
			before := f.kernel.chain(false, "filter", "MANADE-FWD")
			h := section()
			tc.edit(h)
			k, fatal := n.reload(t.Context(), h)
			if fatal != nil || k == nil {
				t.Fatalf("Reload = %v, %v, want kept", k, fatal)
			}
			for _, w := range tc.want {
				if !strings.Contains(k.Error(), w) {
					t.Errorf("kept = %v, want it to name %q", k, w)
				}
			}
			if !slices.Equal(f.kernel.chain(false, "filter", "MANADE-FWD"), before) {
				t.Error("a kept section changed the loaded rules")
			}
		})
	}
	t.Run("a valid section is swapped in after a kept one", func(t *testing.T) {
		f, n := up(t)
		if k, _ := n.reload(t.Context(), section("172.18.3.4/32")); k == nil {
			t.Fatal("setup: want kept")
		}
		h := section("10.0.0.50/32")
		h.DNS = []netip.Addr{netip.MustParseAddr("9.9.9.9")}
		if k, fatal := n.reload(t.Context(), h); k != nil || fatal != nil {
			t.Fatalf("Reload = %v, %v", k, fatal)
		}
		if readAt(t, f.root, "/etc/resolv.conf") != resolvRendered {
			t.Errorf("resolv.conf = %q, want the host's resolver kept", readAt(t, f.root, "/etc/resolv.conf"))
		}
		if got := n.upstreams(); !slices.Equal(got, []netip.AddrPort{netip.MustParseAddrPort("9.9.9.9:53")}) {
			t.Errorf("upstreams = %v, want the new section's resolver", got)
		}
		if !slices.ContainsFunc(f.kernel.chain(false, "filter", "MANADE-OUT"), func(r string) bool { return strings.Contains(r, "9.9.9.9/32") }) {
			t.Error("the new resolver has no port 53 accept")
		}
	})
}

func TestHold(t *testing.T) {
	t.Run("the daemon exits", func(t *testing.T) {
		f := newUpFixture(t)
		n := upNet(t, f, section())
		f.proc.exit(errors.New("exit status 2"))
		if err := n.hold(t.Context()); err == nil || !strings.Contains(err.Error(), "exit status 2") {
			t.Errorf("Hold = %v", err)
		}
	})
	t.Run("a failed restore stops the daemon", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newUpFixture(t)
			n := upNet(t, f, section())
			f.kernel.edit(func(tb map[bool]map[string]*xtable) { tb[false]["filter"].chains["MANADE-IN"] = []string{} })
			f.kernel.fail[platform.LegacyDir+"/iptables-restore"] = errors.New("refused")
			if err := n.hold(t.Context()); err == nil || !strings.Contains(err.Error(), "restoring") || f.proc.stopped != 1 {
				t.Errorf("Hold = %v, stopped %d, want the failure with the daemon stopped", err, f.proc.stopped)
			}
		})
	})
	t.Run("a cancelled context leaves the daemon to Stop", func(t *testing.T) {
		f := newUpFixture(t)
		n := upNet(t, f, section())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := n.hold(ctx); err != nil || f.proc.stopped != 0 {
			t.Errorf("Hold = %v, stopped %d", err, f.proc.stopped)
		}
	})
}

func TestReloadAppliesTheEgressKeys(t *testing.T) {
	t.Run("open to closed swaps the rules and turns IPv6 off", func(t *testing.T) {
		f := newUpFixture(t)
		open := section()
		open.Egress.Open = true
		n := upNet(t, f, open)
		before := f.kernel.chain(false, "filter", "MANADE-FWD")
		if readAt(t, f.root, disableAll) != "0\n" {
			t.Fatal("setup: an open host turned IPv6 off")
		}
		if k, fatal := n.reload(t.Context(), section()); k != nil || fatal != nil {
			t.Fatalf("Reload = %v, %v, want the swap", k, fatal)
		}
		if slices.Equal(f.kernel.chain(false, "filter", "MANADE-FWD"), before) {
			t.Error("the closed section's rules were not loaded")
		}
		if readAt(t, f.root, disableAll) != "1\n" || readAt(t, f.root, disableDefault) != "1\n" {
			t.Error("IPv6 left on after the host stopped being open")
		}
		if n.host.Egress.Open {
			t.Error("the host still holds the open section")
		}
	})
	t.Run("internet on to off turns learning on and keeps resolv.conf", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newUpFixture(t)
			n := upNet(t, f, section())
			if k, fatal := n.reload(t.Context(), closedSection()); k != nil || fatal != nil {
				t.Fatalf("Reload = %v, %v, want the swap", k, fatal)
			}
			if readAt(t, f.root, "/etc/resolv.conf") != resolvRendered || n.gitHubStatus() == nil || n.learning() == nil || f.dnsUDP.isClosed() {
				t.Errorf("resolv.conf %q, status %+v, resolver closed %v", readAt(t, f.root, "/etc/resolv.conf"), n.gitHubStatus(), f.dnsUDP.isClosed())
			}
			if i, j := slices.Index(f.events, "rules"), slices.Index(f.events, "domains"); len(slices.DeleteFunc(slices.Clone(f.events), func(e string) bool { return e != "resolver" })) != 1 || j < i {
				t.Errorf("events %q, want the one resolver kept and the list read after the swap", f.events)
			}
			if _, ipset := f.kernel.payloads(); !slices.ContainsFunc(ipset, func(p string) bool { return strings.Contains(p, "create manade-gh4") }) {
				t.Errorf("ipset payloads %q, want the learned sets created", ipset)
			}
		})
	})
	t.Run("internet off to on turns learning off and keeps the resolver serving", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newUpFixture(t)
			n := upNet(t, f, closedSection())
			if k, fatal := n.reload(t.Context(), section()); k != nil || fatal != nil {
				t.Fatalf("Reload = %v, %v, want the swap", k, fatal)
			}
			if readAt(t, f.root, "/etc/resolv.conf") != resolvRendered || n.gitHubStatus() != nil || n.learning() != nil || f.dnsUDP.isClosed() {
				t.Errorf("resolv.conf %q, status %+v, resolver closed %v", readAt(t, f.root, "/etc/resolv.conf"), n.gitHubStatus(), f.dnsUDP.isClosed())
			}
		})
	})
	t.Run("a domain list that cannot be read keeps the internet rules", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newUpFixture(t)
			n := upNet(t, f, section())
			before := f.kernel.chain(false, "filter", "MANADE-OUT")
			f.fetchErr = errors.New("403 rate limit exceeded")
			k, fatal := n.reload(t.Context(), closedSection())
			if fatal != nil || k == nil || !strings.Contains(k.Error(), "domain list") {
				t.Fatalf("Reload = %v, %v, want kept naming the list", k, fatal)
			}
			if !slices.Equal(f.kernel.chain(false, "filter", "MANADE-OUT"), before) || readAt(t, f.root, "/etc/resolv.conf") != resolvRendered {
				t.Error("the internet rules or resolv.conf were not restored")
			}
			if n.gitHubStatus() != nil || n.learning() != nil || f.dnsUDP.isClosed() || !n.host.Egress.Internet {
				t.Errorf("status %+v, resolver closed %v, want learning off and the resolver serving", n.gitHubStatus(), f.dnsUDP.isClosed())
			}
		})
	})
	t.Run("a reload that turns internet on leaves the hold running", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newUpFixture(t)
			n := upNet(t, f, closedSection())
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- n.hold(ctx) }()
			synctest.Wait()
			if k, fatal := n.reload(t.Context(), section()); k != nil || fatal != nil {
				t.Fatalf("Reload = %v, %v", k, fatal)
			}
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("Hold = %v after a reload turned learning off", err)
			default:
			}
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Hold = %v, want nil once its context ends", err)
			}
		})
	})
}
