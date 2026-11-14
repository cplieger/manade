package host

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
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
		n := upClosed(t, f, closedSection())
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
	t.Run("an internet host runs no resolver", func(t *testing.T) {
		f := newUpFixture(t)
		n, err := bringUp(t.Context(), section(), f.platform(), guardHooks{})
		if err != nil || slices.Contains(f.events, "resolver") || n.gitHubStatus() != nil {
			t.Errorf("Up = %v, events %q, status %+v", err, f.events, n.gitHubStatus())
		}
	})
}

// upClosed brings an internet: false host up and stops it when the test
// ends, inside a synctest bubble too, whose goroutines must end with it.
func upClosed(t *testing.T, f *upFixture, h *config.Host) *hostNet {
	t.Helper()
	n, err := bringUp(t.Context(), h, f.platform(), guardHooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.stop() })
	return n
}

// compact drops repeats in a row, the restores of one rule load.
func compact(events []string) []string { return slices.Compact(slices.Clone(events)) }

func TestLearnedAddressesInTheRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newUpFixture(t)
		n := upClosed(t, f, closedSection())
		err := n.learner.Learn(t.Context(), []resolver.Record{
			{Addr: netip.MustParseAddr("140.82.121.10"), TTL: time.Minute},
			{Addr: netip.MustParseAddr("192.168.1.10"), TTL: time.Minute},
		})
		if err != nil {
			t.Fatal(err)
		}
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
	n := upClosed(t, f, closedSection())
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
	synctest.Test(t, func(t *testing.T) {
		f := newUpFixture(t)
		n := upClosed(t, f, closedSection())
		f.dnsUDP.end(errors.New("read udp 127.0.0.1:53: bad file descriptor"))
		err := n.hold(t.Context())
		if err == nil || !strings.Contains(err.Error(), "DNS resolver stopped") || f.proc.stopped != 1 {
			t.Errorf("Hold = %v, stopped %d, want the failure with the daemon stopped", err, f.proc.stopped)
		}
	})
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
	n := upClosed(t, f, h)
	next := closedSection()
	next.Egress.IPv6 = true
	next.Egress.Block6 = []netip.Prefix{pfx("2606:50c0::/32")}
	if k, fatal := n.reload(t.Context(), next); k != nil || fatal != nil {
		t.Fatalf("Reload = %v, %v", k, fatal)
	}
	_, before := f.kernel.payloads()
	if err := n.learner.Learn(t.Context(), []resolver.Record{{Addr: netip.MustParseAddr("2606:50c0:8000::154"), TTL: time.Minute}}); err != nil {
		t.Fatal(err)
	}
	if _, after := f.kernel.payloads(); len(after) != len(before) {
		t.Errorf("an address in the new block6 was written: %q", after[len(before):])
	}
}
