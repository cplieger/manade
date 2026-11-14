package host

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/host/resolver"
)

// metaBody is a GET /meta body in GitHub's shape, trimmed.
const metaBody = `{"domains":{"actions_inbound":{"full_domains":["github.com","api.github.com","codeload.github.com","ghcr.io"],
"wildcard_domains":["*.githubusercontent.com","*.github.com"]}}}`

func closedSection() *config.Host {
	h := section()
	h.Egress.Internet = false
	return h
}

func TestUpInternetFalse(t *testing.T) {
	t.Run("reads the list through its resolver, then resolves and starts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newUpFixture(t)
			start := time.Now()
			n, err := bringUp(t.Context(), closedSection(), f.platform(), guardHooks{})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(compact(f.events), []string{"rules", "resolver", "domains", "lookup", "daemon"}) {
				t.Errorf("events = %q, want the rules, the resolver, the list, the lookup, then the daemon", compact(f.events))
			}
			st := n.gitHubStatus()
			if st == nil || st.Source != resolver.SourceRead || !st.DomainsAt.Equal(start) {
				t.Errorf("GitHubStatus = %+v, want the list read at start", st)
			}
			if !strings.Contains(readAt(t, f.root, domainsFile), "codeload.github.com") {
				t.Error("the list was not saved on the data volume")
			}
			if n.iPv6Status() != nil {
				t.Error("an internet: false host reports a /64")
			}
			if err := n.stop(); err != nil || !f.dnsUDP.isClosed() || !f.dnsTCP.isClosed() || f.proc.stopped != 1 {
				t.Errorf("Stop = %v, want the daemon and the resolver stopped", err)
			}
		})
	})
	t.Run("a failed read with no saved list starts no daemon and leaves nothing serving", func(t *testing.T) {
		f := newUpFixture(t)
		f.fetchErr = errors.New("403 rate limit exceeded")
		_, err := bringUp(t.Context(), closedSection(), f.platform(), guardHooks{})
		if err == nil || !strings.Contains(err.Error(), "domain list") || !strings.Contains(err.Error(), "no saved copy") {
			t.Errorf("Up = %v, want the refusal naming the list", err)
		}
		if slices.Contains(f.events, "lookup") || slices.Contains(f.events, "daemon") || !f.dnsUDP.isClosed() || !f.dnsTCP.isClosed() {
			t.Errorf("events %q, resolver closed %v, want no lookup, no daemon and the resolver closed", f.events, f.dnsUDP.isClosed())
		}
	})
	t.Run("a failed read starts on the saved list", func(t *testing.T) {
		f := newUpFixture(t)
		writeAt(t, f.root, domainsFile, `{"read_at":"2026-11-01T00:00:00Z","full_domains":["api.github.com","github.com"]}`)
		f.fetchErr = errors.New("503")
		n := upNet(t, f, closedSection())
		if st := n.gitHubStatus(); st.Source != resolver.SourceSaved || st.DomainsAt.Format(time.DateOnly) != "2026-11-01" {
			t.Errorf("GitHubStatus = %+v, want the saved list with its age", st)
		}
	})
	t.Run("a failed lookup stops the resolver", func(t *testing.T) {
		f := newUpFixture(t)
		f.lookupErr = errors.New("i/o timeout")
		if _, err := bringUp(t.Context(), closedSection(), f.platform(), guardHooks{}); err == nil || !f.dnsUDP.isClosed() {
			t.Errorf("Up = %v, resolver closed %v", err, f.dnsUDP.isClosed())
		}
	})
	t.Run("an internet host serves its resolver and reads no list", func(t *testing.T) {
		f := newUpFixture(t)
		n := upNet(t, f, section())
		if !slices.Contains(f.events, "resolver") || slices.Contains(f.events, "domains") || n.gitHubStatus() != nil {
			t.Errorf("events %q, status %+v, want the resolver serving with no list", f.events, n.gitHubStatus())
		}
	})
}

// upNet brings host h up and stops it when the test ends, inside a
// synctest bubble too, whose goroutines must end with it.
func upNet(t *testing.T, f *upFixture, h *config.Host) *hostNet {
	t.Helper()
	n, err := bringUp(t.Context(), h, f.platform(), guardHooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.stop() })
	return n
}

// learn adds recs through l, running the drain when the add starts one,
// and waits for their write.
func learn(ctx context.Context, l *resolver.Learner, recs []resolver.Record) {
	written, drain := l.Learn(recs)
	if drain != nil {
		drain(ctx)
	}
	<-written
}

// compact drops repeats in a row, the restores of one rule load.
func compact(events []string) []string { return slices.Compact(slices.Clone(events)) }

func TestLearnedAddressesInTheRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newUpFixture(t)
		n := upNet(t, f, closedSection())
		learn(t.Context(), n.gh.Load().learner, []resolver.Record{
			{Addr: netip.MustParseAddr("140.82.121.10"), TTL: time.Minute},
			{Addr: netip.MustParseAddr("192.168.1.10"), TTL: time.Minute},
		})
		_, ipset := f.kernel.payloads()
		if last := ipset[len(ipset)-1]; last != "add manade-gh4 140.82.121.10 timeout 660\n" {
			t.Errorf("learned payload = %q, want the public address with its TTL plus 10 minutes", last)
		}
		time.Sleep(2 * time.Minute)
		f.kernel.edit(func(tb map[bool]map[string]*xtable) { tb[false]["filter"].chains["MANADE-FWD"] = []string{} })
		if err := n.Guard.check(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, ipset = f.kernel.payloads()
		if last := ipset[len(ipset)-1]; !strings.Contains(last, "add manade-gh4 140.82.121.10 timeout 540\n") || strings.Contains(last, "192.168.1.10") {
			t.Errorf("restore payload = %q, want the held address with its remaining 540 s", last)
		}
		if st := n.gitHubStatus(); st.Addresses4 != 1 || st.Addresses6 != 0 {
			t.Errorf("GitHubStatus = %+v, want one address held", st)
		}
	})
}

func TestReloadMovesTheResolversUpstreams(t *testing.T) {
	f := newUpFixture(t)
	n := upNet(t, f, closedSection())
	h := closedSection()
	h.DNS = []netip.Addr{netip.MustParseAddr("9.9.9.9")}
	if k, fatal := n.reload(t.Context(), h); k != nil || fatal != nil {
		t.Fatalf("Reload = %v, %v", k, fatal)
	}
	if readAt(t, f.root, "/etc/resolv.conf") != "nameserver 127.0.0.1\n" {
		t.Error("resolv.conf no longer names the host's resolver")
	}
	if !slices.ContainsFunc(f.kernel.chain(false, "filter", "MANADE-OUT"), func(r string) bool { return strings.Contains(r, "9.9.9.9/32") }) {
		t.Error("the new resolver has no port 53 accept")
	}
	if got := n.upstreams(); !slices.Equal(got, []netip.AddrPort{netip.MustParseAddrPort("9.9.9.9:53")}) {
		t.Errorf("upstreams = %v, want the new section's resolver", got)
	}
}

func TestHoldEndsWhenTheResolverStops(t *testing.T) {
	for name, h := range map[string]*config.Host{"internet": section(), "internet: false": closedSection()} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newUpFixture(t)
				n := upNet(t, f, h)
				f.dnsUDP.end(errors.New("read udp 127.0.0.1:53: bad file descriptor"))
				err := n.hold(t.Context())
				if err == nil || !strings.Contains(err.Error(), "DNS resolver stopped") || f.proc.stopped != 1 {
					t.Errorf("Hold = %v, stopped %d, want the failure with the daemon stopped", err, f.proc.stopped)
				}
			})
		})
	}
}

func TestAddLearned(t *testing.T) {
	ls := []egress.Learned{
		{Addr: netip.MustParseAddr("140.82.121.10"), Left: 90 * time.Second},
		{Addr: netip.MustParseAddr("2606:50c0:8000::154"), Left: 10 * time.Minute},
	}
	t.Run("nftables", func(t *testing.T) {
		k := newFakeIPT(false)
		k.nft = true
		l := &ruleLoader{Run: k.run, Kernel: &platform.Kernel{Backend: egress.NFTables}}
		if err := l.addLearned(t.Context(), ls); err != nil {
			t.Fatal(err)
		}
		nft, _ := k.payloads()
		want := "add element inet manade manade-gh4 { 140.82.121.10 timeout 90s }\n" +
			"delete element inet manade manade-gh4 { 140.82.121.10 }\n" +
			"add element inet manade manade-gh4 { 140.82.121.10 timeout 90s }\n" +
			"add element inet manade manade-gh6 { 2606:50c0:8000::154 timeout 600s }\n" +
			"delete element inet manade manade-gh6 { 2606:50c0:8000::154 }\n" +
			"add element inet manade manade-gh6 { 2606:50c0:8000::154 timeout 600s }\n"
		if len(nft) != 1 || nft[0] != want {
			t.Errorf("nft payloads = %q, want one transaction %q", nft, want)
		}
	})
	t.Run("iptables", func(t *testing.T) {
		k := newFakeIPT(false)
		l := &ruleLoader{Run: k.run, Kernel: iptKernel()}
		if err := l.addLearned(t.Context(), ls); err != nil {
			t.Fatal(err)
		}
		_, ipset := k.payloads()
		if want := "add manade-gh4 140.82.121.10 timeout 90\nadd manade-gh6 2606:50c0:8000::154 timeout 600\n"; len(ipset) != 1 || ipset[0] != want {
			t.Errorf("ipset payloads = %q, want %q", ipset, want)
		}
	})
	t.Run("a refused write names what it added", func(t *testing.T) {
		k := newFakeIPT(false)
		k.fail["ipset restore"] = errors.New("Hash is full")
		l := &ruleLoader{Run: k.run, Kernel: iptKernel()}
		if err := l.addLearned(t.Context(), ls); err == nil || !strings.Contains(err.Error(), "learned GitHub addresses") {
			t.Errorf("AddLearned = %v", err)
		}
	})
}

func TestReloadFiltersLearnedAddressesByTheNewSection(t *testing.T) {
	f := v6Fixture(t, &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::1")}})
	h := closedSection()
	h.Egress.IPv6 = true
	n := upNet(t, f, h)
	next := closedSection()
	next.Egress.IPv6 = true
	next.Egress.Block6 = []netip.Prefix{pfx("2606:50c0::/32")}
	if k, fatal := n.reload(t.Context(), next); k != nil || fatal != nil {
		t.Fatalf("Reload = %v, %v", k, fatal)
	}
	_, before := f.kernel.payloads()
	learn(t.Context(), n.gh.Load().learner, []resolver.Record{{Addr: netip.MustParseAddr("2606:50c0:8000::154"), TTL: time.Minute}})
	if _, after := f.kernel.payloads(); len(after) != len(before) {
		t.Errorf("an address in the new block6 was written: %q", after[len(before):])
	}
}

// followGitHub refreshes the list of whichever learning a reload made the
// host's, and none once learning is off.
func TestFollowGitHubFollowsTheLearning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := slog.New(slog.DiscardHandler)
		var fetches atomic.Int32
		fetch := func(context.Context) ([]byte, error) { fetches.Add(1); return []byte(metaBody), nil }
		n := &hostNet{ghMoved: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { n.followGitHub(ctx); close(done) }()
		n.setGitHub(&gitHub{source: resolver.NewSource(fetch, filepath.Join(t.TempDir(), "domains.json"), log)})
		time.Sleep(6*time.Hour + time.Minute)
		if got := fetches.Load(); got != 1 {
			t.Errorf("fetches after one refresh period = %d, want 1", got)
		}
		n.setGitHub(nil)
		time.Sleep(12 * time.Hour)
		if got := fetches.Load(); got != 1 {
			t.Errorf("fetches with learning off = %d, want none more", got)
		}
		cancel()
		<-done
	})
}

// A job started before a reload that flips egress.internet keeps the
// resolver resolv.conf named at its start: it resolves after each flip, and
// its GitHub lookup is learned once internet is off.
func TestARunningJobResolvesAcrossInternetFlips(t *testing.T) {
	f := newUpFixture(t)
	up := fakeUpstream(t, netip.MustParseAddr("140.82.121.10"))
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	job := jobResolver(udp.LocalAddr().String())
	learned := func() bool {
		_, ipset := f.kernel.payloads()
		return slices.ContainsFunc(ipset, func(p string) bool { return strings.HasPrefix(p, "add manade-gh4 140.82.121.10 ") })
	}
	// The list read resolves api.github.com through the host's resolver, as
	// the supervisor's HTTP client does, and needs that answer learned.
	var learnedForTheList bool
	p := f.platform()
	p.ListenDNS = func(context.Context) (net.PacketConn, net.Listener, error) { return udp, tcp, nil }
	p.Upstreams = func([]netip.Addr) []netip.AddrPort { return []netip.AddrPort{up} }
	p.FetchDomains = func(ctx context.Context) ([]byte, error) {
		if _, err := job.LookupNetIP(ctx, "ip4", "api.github.com"); err != nil {
			return nil, err
		}
		learnedForTheList = learned()
		return []byte(metaBody), nil
	}
	n, err := bringUp(t.Context(), section(), p, guardHooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.stop() })
	resolvAtStart := readAt(t, f.root, "/etc/resolv.conf")
	resolves := func(when string) {
		t.Helper()
		addrs, err := job.LookupNetIP(t.Context(), "ip4", "codeload.github.com")
		if err != nil || !slices.Equal(addrs, []netip.Addr{netip.MustParseAddr("140.82.121.10")}) {
			t.Errorf("%s: the job's lookup = %v, %v, want the upstream's answer", when, addrs, err)
		}
		if got := readAt(t, f.root, "/etc/resolv.conf"); got != resolvAtStart {
			t.Errorf("%s: resolv.conf = %q, want %q unchanged", when, got, resolvAtStart)
		}
	}
	resolves("before any flip")
	if learned() {
		t.Error("an internet host learned the job's answer")
	}
	if k, fatal := n.reload(t.Context(), closedSection()); k != nil || fatal != nil {
		t.Fatalf("Reload to internet: false = %v, %v", k, fatal)
	}
	if !learnedForTheList {
		t.Error("api.github.com's address was not learned before the domain list was read")
	}
	resolves("after internet was turned off")
	if !learned() {
		t.Error("the job's GitHub lookup after internet was turned off was not learned")
	}
	if k, fatal := n.reload(t.Context(), section()); k != nil || fatal != nil {
		t.Fatalf("Reload to internet = %v, %v", k, fatal)
	}
	resolves("after internet was turned on again")
}
