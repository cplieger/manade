package host

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/hostapi"
)

// The host parts its network refuses at a boot on the fake kernel: route4's
// on-link 172.17.0.0/16 shadows the subnet, and the namespace has no IPv6
// address or default route.
const (
	shadowedHost = `{"network": {"subnet": "172.17.5.0/24"}}`
	ipv6Host     = `{"egress": {"ipv6": true}}`
)

// withIPv6 gives f's namespace a global IPv6 address, an IPv6 default
// route, IPv6 forwarding off and the kernel's IPv6 nat table.
func withIPv6(t *testing.T, f *upFixture) {
	t.Helper()
	f.kernel = newFakeIPT(true)
	writeAt(t, f.root, "/proc/net/if_inet6", "fd0000c8000000000000000000000002 07 40 00 80     eth0\n")
	writeAt(t, f.root, "/proc/net/ipv6_route", "00000000000000000000000000000000 00 00000000000000000000000000000000 00 fd0000c8000000000000000000000001 00000400 00000001 00000000 00000003     eth0\n")
	writeAt(t, f.root, forwarding6, "0\n")
}

const forwarding6 = "/proc/sys/net/ipv6/conf/all/forwarding"

// rebootable is f's platform with a fresh daemon and resolver at each
// start, as a boot tried again in the same namespace gets.
func rebootable(f *upFixture) *seams {
	p := f.platform()
	p.StartDaemon = func(spec *daemonSpec) (process, error) {
		f.spec = spec
		return newProcess(), nil
	}
	p.ListenDNS = func(context.Context) (net.PacketConn, net.Listener, error) {
		return newIdleConn(), newIdleConn(), nil
	}
	return p
}

// failingFirst makes p's lookups through the host's own resolver fail the
// first time only, or every time with always.
func failingFirst(p *seams, always bool) {
	var asked atomic.Int32
	inner := p.Lookup
	p.Lookup = func(ctx context.Context, server netip.Addr, name string) ([]netip.Addr, error) {
		if server == netsetup.ForwarderAddr && (always || asked.Add(1) == 1) {
			return nil, errors.New("i/o timeout")
		}
		return inner(ctx, server, name)
	}
}

// saveUnbooted saves sec as the host's section in s's directory, not yet
// booted on the host, with before as the section it ran before, if any.
func saveUnbooted(t *testing.T, s *supervisor, sec, before *hostapi.Section) {
	t.Helper()
	if err := writeState(t.Context(), s.sectionPath, &hostState{Section: sec, Fallback: before, Unbooted: true}); err != nil {
		t.Fatal(err)
	}
}

// restarted is a new run of host lan on the files s saved, on platform p.
func restarted(s *supervisor, p *seams) *supervisor {
	return newSupervisor(lanSelf(), &Options{Log: slog.New(slog.DiscardHandler)}, s.sectionPath, p)
}

// bootUp boots s, its network then s.net, and fails unless it comes up.
func bootUp(t *testing.T, s *supervisor) {
	t.Helper()
	n, err := s.boot(t.Context())
	if err != nil || n == nil {
		t.Fatalf("boot = %v, %v; want the host up", n, err)
	}
	t.Cleanup(func() { _ = n.stop() })
	s.net = n
}

// booted is what a boot answered.
type booted struct {
	n   *hostNet
	err error
}

// bootAsync boots s in the background; the test's end waits for the boot.
func bootAsync(t *testing.T, s *supervisor) <-chan booted {
	t.Helper()
	done, finished := make(chan booted, 1), make(chan struct{})
	go func() {
		defer close(finished)
		n, err := s.boot(t.Context())
		done <- booted{n, err}
	}()
	t.Cleanup(func() { <-finished })
	return done
}

// failSaves makes every save of the host's state fail at phase, leaving
// the file as it was, until the test ends or it calls the restore answered.
func failSaves(t *testing.T, phase filesafe.WritePhase) (restore func()) {
	t.Helper()
	inner := writeFile
	writeFile = func(context.Context, string, []byte, ...filesafe.WriteOption) (filesafe.Result, error) {
		return filesafe.Result{}, &filesafe.WriteError{Phase: phase, Err: syscall.ENOSPC}
	}
	restore = func() { writeFile = inner }
	t.Cleanup(restore)
	return restore
}

// A first section the host's network refuses is answered 422 and saved
// nowhere, and the host keeps waiting, holding the refusal, for a section
// it boots on.
func TestAFirstSectionTheNetworkRefuses(t *testing.T) {
	for _, tc := range []struct{ name, host, path string }{
		{"a subnet an on-link route shadows", shadowedHost, "hosts.lan.network.subnet"},
		{"ipv6 with no IPv6 in the namespace", ipv6Host, "hosts.lan.egress.ipv6"},
		{"resolvers that do not answer", dnsHost, "hosts.lan.dns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpFixture(t)
			f.kernel = newFakeIPT(true)
			p := f.platform()
			inner := p.Lookup
			p.Lookup = func(ctx context.Context, server netip.Addr, name string) ([]netip.Addr, error) {
				if server == netip.MustParseAddr("9.9.9.9") {
					return nil, errors.New("i/o timeout")
				}
				return inner(ctx, server, name)
			}
			s := waitingOn(t, lanSelf(), p)
			_, err := apiOf(s).PutSection(t.Context(), sectionOf(1, tc.host, smallJSON))
			if got := refusedAt(t, err); !slices.Equal(got, []string{tc.path}) {
				t.Errorf("refused at %q, want %s", got, tc.path)
			}
			if _, ok, err := readSection(t.Context(), s.sectionPath); ok || err != nil {
				t.Errorf("a refused first section was saved: %v, %v", ok, err)
			}
			if st := s.sectionStatus(); st.State != hostapi.SectionWaiting || st.Refused == nil || st.Refused.Errors[0].Path != tc.path {
				t.Errorf("section status = %+v, want waiting with the refusal", st)
			}
			ans, err := apiOf(s).PutSection(t.Context(), sectionOf(2, lanHost, smallJSON))
			if err != nil || ans.Applies != hostapi.SectionApplied {
				t.Fatalf("PUT of a section the network accepts = %+v, %v", ans, err)
			}
			if got := s.awaitSection(t.Context()); got == nil || got.raw.Revision != 2 {
				t.Errorf("booted on %+v, want revision 2", got)
			}
		})
	}
}

// A first section whose network facts cannot be read is saved and handed
// to the boot, which ends the run with the cause.
func TestAFirstSectionOnAHostWithNoRuleBackend(t *testing.T) {
	f := newUpFixture(t)
	f.kernel.fail["nft list tables"] = errors.New("nft: not found")
	f.kernel.fail[platform.LegacyDir+"/iptables"] = errors.New("iptables: not found")
	s := waitingOn(t, lanSelf(), f.platform())
	done := bootAsync(t, s)
	if ans := put(t, s, sectionOf(1, lanHost, smallJSON)); ans.Applies != hostapi.SectionApplied {
		t.Errorf("PUT = %+v, want the section taken", ans)
	}
	select {
	case b := <-done:
		if err := b.err; err == nil || !strings.Contains(err.Error(), "nft: not found") {
			t.Errorf("boot = %v, want the run ended naming the missing backend", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the boot did not end")
	}
	if _, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil {
		t.Errorf("the section was not saved: %v, %v", ok, err)
	}
}

// A section accepted for a restart that its boot then refuses: the host
// boots on the section it ran before, saved again, reports the refusal,
// refuses the same settings delivered again, and accepts other ones.
func TestABootRefusalRunsTheSectionBefore(t *testing.T) {
	f := newUpFixture(t)
	f.kernel = newFakeIPT(true)
	before := sectionOf(1, lanHost, smallJSON)
	s, _ := upOn(t, before, f.platform())
	refused := sectionOf(2, ipv6Host, smallJSON)
	if ans := put(t, s, refused); ans.Applies != hostapi.SectionPendingRestart {
		t.Fatalf("setup: the ipv6 section = %+v, want a restart, its namespace's IPv6 unread", ans)
	}
	// The restart: a new run on the same saved files, in a fresh namespace
	// that has no IPv6 address.
	g := newUpFixture(t)
	g.kernel = newFakeIPT(true)
	r := restarted(s, g.platform())
	r.bootSaved(t.Context())
	bootUp(t, r)
	t.Run("it boots on the section it ran before, saved again", func(t *testing.T) {
		if r.running == nil || r.running.hash != hashOf(t, before) || g.proc.stopped != 0 || g.spec == nil {
			t.Errorf("running %+v, daemon started %v", r.running, g.spec != nil)
		}
		if saved, ok, err := readSection(t.Context(), r.sectionPath); !ok || err != nil || saved.Revision != 1 {
			t.Errorf("saved %+v, %v, %v; want revision 1", saved, ok, err)
		}
		if !noFallback(stateOf(t, r)) {
			t.Error("the fallback is still saved")
		}
	})
	t.Run("it reports the refusal", func(t *testing.T) {
		st := r.sectionStatus()
		if st.State != hostapi.SectionRefused || st.Hash != hashOf(t, before) || st.Refused == nil || st.Refused.Hash != hashOf(t, refused) {
			t.Fatalf("section status = %+v", st)
		}
		if st.Refused.Errors[0].Path != "hosts.lan.egress.ipv6" {
			t.Errorf("refused at %+v", st.Refused.Errors)
		}
	})
	finishUp(t, r)
	t.Run("the same settings delivered again are refused", func(t *testing.T) {
		_, err := apiOf(r).PutSection(t.Context(), sectionOf(3, ipv6Host, smallJSON))
		if got := refusedAt(t, err); !slices.Equal(got, []string{"hosts.lan.egress.ipv6"}) {
			t.Errorf("refused at %q", got)
		}
		if st := r.sectionStatus(); st.PendingHash != "" || st.Hash != hashOf(t, before) {
			t.Errorf("section status = %+v, want nothing pending", st)
		}
		if !noFallback(stateOf(t, r)) {
			t.Error("a refused section saved a fallback")
		}
	})
	t.Run("another section is accepted", func(t *testing.T) {
		if ans := put(t, r, sectionOf(4, allowHost, smallJSON)); ans.Applies != hostapi.SectionApplied {
			t.Errorf("PUT = %+v", ans)
		}
		if st := r.sectionStatus(); st.State != hostapi.SectionRunning || st.Revision != 4 {
			t.Errorf("section status = %+v", st)
		}
	})
}

// Each boot step after the facts check that fails on what a section not yet
// booted decides is that section's refusal at the field: the host boots on
// the section it ran before, in the same run, and refuses those settings
// for the run.
func TestABootStepFailureRunsTheSectionBefore(t *testing.T) {
	for _, tc := range []struct {
		name, host, path string
		prepare          func(t *testing.T, f *upFixture, p *seams)
	}{
		{"IPv6 forwarding", ipv6Host, "hosts.lan.egress.ipv6", func(t *testing.T, f *upFixture, _ *seams) {
			withIPv6(t, f)
			if err := os.Remove(f.root.Path(forwarding6)); err != nil {
				t.Fatal(err)
			}
		}},
		{"the job IPv6 prefix", ipv6Host, "hosts.lan.egress.ipv6", func(t *testing.T, f *upFixture, p *seams) {
			withIPv6(t, f)
			p.Random = iotest.ErrReader(errors.New("no entropy"))
		}},
		{"bridge-nf-call-ip6tables", ipv6Host, "hosts.lan.egress.ipv6", func(t *testing.T, f *upFixture, _ *seams) {
			withIPv6(t, f)
			if err := os.MkdirAll(f.root.Path(platform.BridgeNFCall6), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"the rule load", allowHost, "hosts.lan.egress", func(_ *testing.T, f *upFixture, _ *seams) {
			f.kernel.failOnce[platform.LegacyDir+"/iptables-restore"] = errors.New("refused")
		}},
		{"GitHub's domain list", closedHost, "hosts.lan.egress.internet", func(_ *testing.T, f *upFixture, _ *seams) {
			f.fetchErr = errors.New("403 rate limit exceeded")
		}},
		{"GitHub's API name through the resolvers", dnsHost, "hosts.lan.dns", func(_ *testing.T, _ *upFixture, p *seams) {
			failingFirst(p, false)
		}},
		{"the job network", `{"network": {"subnet": "172.20.0.0/16"}}`, "hosts.lan.network.subnet", func(_ *testing.T, f *upFixture, p *seams) {
			var dials atomic.Int32
			p.Dial = func(context.Context) (jobNetworks, error) {
				if dials.Add(1) == 1 {
					return &fakeNetworks{err: errors.New("pool overlaps with other one on this address space")}, nil
				}
				return f.nets, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpFixture(t)
			p := rebootable(f)
			tc.prepare(t, f, p)
			before, failing := sectionOf(1, lanHost, smallJSON), sectionOf(2, tc.host, smallJSON)
			s := waitingOn(t, lanSelf(), p)
			saveUnbooted(t, s, failing, before)
			s.bootSaved(t.Context())
			bootUp(t, s)
			if s.running.hash != hashOf(t, before) {
				t.Errorf("booted on revision %d, want 1", s.running.raw.Revision)
			}
			if saved, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil || saved.Revision != 1 {
				t.Errorf("saved %+v, %v, %v; want revision 1", saved, ok, err)
			}
			if st := stateOf(t, s); !noFallback(st) || st.Refused != nil {
				t.Errorf("state %+v, want no fallback and no refusal kept for a restart", st)
			}
			if !slices.Equal(s.net.Original.Addrs, []netip.Addr{netip.MustParseAddr("192.168.1.1")}) {
				t.Errorf("the second boot read the machine's resolvers as %v", s.net.Original.Addrs)
			}
			if b, err := os.ReadFile(f.root.Path(forwarding6)); err == nil && string(b) != "0\n" {
				t.Errorf("IPv6 forwarding left at %q", b)
			}
			st := s.sectionStatus()
			if st.Refused == nil || st.Refused.Hash != hashOf(t, failing) || st.Refused.Errors[0].Path != tc.path {
				t.Fatalf("section status = %+v, want the refusal at %s", st, tc.path)
			}
			finishUp(t, s)
			_, err := apiOf(s).PutSection(t.Context(), sectionOf(3, tc.host, smallJSON))
			if got := refusedAt(t, err); !slices.Equal(got, []string{tc.path}) {
				t.Errorf("the same settings again refused at %q", got)
			}
			if ans := put(t, s, sectionOf(4, lanHost, smallJSON, largeJSON)); ans.Applies != hostapi.SectionApplied {
				t.Errorf("a corrected PUT = %+v", ans)
			}
		})
	}
}

// A boot step failure of a section that booted on this host before is the
// host's own failure: the run ends with it and the section stays saved.
func TestABootStepFailureOfABootedSectionEndsTheRun(t *testing.T) {
	f := newUpFixture(t)
	p := rebootable(f)
	failingFirst(p, true)
	s := waitingOn(t, lanSelf(), p)
	if err := writeSection(t.Context(), s.sectionPath, sectionOf(1, dnsHost, smallJSON)); err != nil {
		t.Fatal(err)
	}
	s.bootSaved(t.Context())
	n, err := s.boot(t.Context())
	if n != nil || err == nil || errors.Is(err, ErrRestart) || !strings.Contains(err.Error(), "hosts.lan.dns") {
		t.Errorf("boot = %v, %v; want the run ended on the lookup", n, err)
	}
	if saved, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil || saved.Revision != 1 {
		t.Errorf("saved %+v, %v, %v; want the section kept", saved, ok, err)
	}
	if st := s.sectionStatus(); st.Refused != nil {
		t.Errorf("refused %+v, want no refusal", st.Refused)
	}
}

// A failed boot that turned IPv6 off restarts the host into a fresh
// namespace to boot a section before that keeps IPv6 on, the refusal saved
// across that restart only: the next restart judges the settings afresh.
func TestABootThatLostIPv6Restarts(t *testing.T) {
	f := newUpFixture(t)
	withIPv6(t, f)
	p := rebootable(f)
	failingFirst(p, false)
	before, failing := sectionOf(1, ipv6Host, smallJSON), sectionOf(2, dnsHost, smallJSON)
	s := waitingOn(t, lanSelf(), p)
	saveUnbooted(t, s, failing, before)
	s.bootSaved(t.Context())
	if n, err := s.boot(t.Context()); n != nil || !errors.Is(err, ErrRestart) {
		t.Fatalf("boot = %v, %v; want ErrRestart", n, err)
	}
	t.Run("the section before is saved to boot on", func(t *testing.T) {
		if saved, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil || saved.Revision != 1 {
			t.Errorf("saved %+v, %v, %v; want revision 1", saved, ok, err)
		}
		if st := stateOf(t, s); !noFallback(st) || st.Refused == nil || st.Refused.Hash != hashOf(t, failing) {
			t.Errorf("state %+v, want the fallback dropped and the refusal saved", st)
		}
	})
	g := newUpFixture(t)
	withIPv6(t, g)
	r := restarted(s, rebootable(g))
	r.bootSaved(t.Context())
	bootUp(t, r)
	finishUp(t, r)
	t.Run("the restart boots it and refuses the same settings", func(t *testing.T) {
		if r.running.hash != hashOf(t, before) || r.net.Addr6 != netip.MustParseAddr("fd00:c8::2") {
			t.Errorf("booted revision %d with IPv6 address %v", r.running.raw.Revision, r.net.Addr6)
		}
		_, err := apiOf(r).PutSection(t.Context(), sectionOf(3, dnsHost, smallJSON))
		if got := refusedAt(t, err); !slices.Equal(got, []string{"hosts.lan.dns"}) {
			t.Errorf("refused at %q", got)
		}
		if stateOf(t, r).Refused != nil {
			t.Error("the saved refusal outlived the restart it was saved for")
		}
	})
	t.Run("a later restart takes the same settings again", func(t *testing.T) {
		h := newUpFixture(t)
		withIPv6(t, h)
		q := restarted(r, rebootable(h))
		q.bootSaved(t.Context())
		bootUp(t, q)
		finishUp(t, q)
		if ans := put(t, q, sectionOf(4, dnsHost, smallJSON)); ans.Applies != hostapi.SectionPendingRestart {
			t.Errorf("PUT = %+v, want a restart", ans)
		}
	})
}

// A stop during the boot of a section not yet booted is no refusal: the
// section and its fallback stay for the next start.
func TestAStopDuringABootStepIsNoRefusal(t *testing.T) {
	f := newUpFixture(t)
	p := rebootable(f)
	ctx, cancel := context.WithCancel(t.Context())
	p.Lookup = func(ctx context.Context, _ netip.Addr, _ string) ([]netip.Addr, error) {
		cancel()
		return nil, ctx.Err()
	}
	before, next := sectionOf(1, lanHost, smallJSON), sectionOf(2, dnsHost, smallJSON)
	s := waitingOn(t, lanSelf(), p)
	saveUnbooted(t, s, next, before)
	s.bootSaved(t.Context())
	if n, err := s.boot(ctx); n != nil || err == nil || errors.Is(err, ErrRestart) {
		t.Errorf("boot = %v, %v; want the run ended by the stop", n, err)
	}
	if st := s.sectionStatus(); st.Refused != nil {
		t.Errorf("refused %+v", st.Refused)
	}
	if st := stateOf(t, s); st.Section == nil || st.Section.Revision != 2 || !st.Unbooted || st.Fallback == nil || st.Fallback.Revision != 1 {
		t.Errorf("state %+v; want revision 2 not booted, with revision 1 to fall back to", st)
	}
}

// A boot refusal during a stop still saves the fallback in the refused
// section's place.
func TestABootRefusalDuringAStopSavesTheFallback(t *testing.T) {
	f := newUpFixture(t)
	s := waitingOn(t, lanSelf(), f.platform())
	saveUnbooted(t, s, sectionOf(2, shadowedHost, smallJSON), sectionOf(1, lanHost, smallJSON))
	s.bootSaved(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = s.boot(ctx)
	if saved, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil || saved.Revision != 1 {
		t.Errorf("saved %+v, %v, %v; want revision 1", saved, ok, err)
	}
}

// A section delivered after a boot's refusal and before the boot asks
// again is taken once, so the next delivery to a waiting host is not held
// behind it.
func TestASectionDeliveredBeforeTheBootAsksIsTakenOnce(t *testing.T) {
	s := waitingOn(t, lanSelf(), newUpFixture(t).platform())
	put(t, s, sectionOf(1, lanHost, smallJSON))
	if got := s.awaitSection(t.Context()); got == nil || got.raw.Revision != 1 {
		t.Fatalf("boots on %+v, want revision 1", got)
	}
	s.sectionMu.Lock()
	s.running = nil
	s.phase.Store(phaseWaiting)
	s.sectionMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := apiOf(s).PutSection(t.Context(), sectionOf(2, lanHost, smallJSON, largeJSON))
		done <- err
	}()
	if got := s.awaitSection(t.Context()); got == nil || got.raw.Revision != 2 {
		t.Errorf("boots on %+v, want revision 2", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("PUT = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery to the waiting host did not return")
	}
}

// A first section whose boot fails after the facts check restarts the host,
// which then waits with the refusal and no section, refusing the same
// settings and booting corrected ones.
func TestAFirstSectionsBootStepFailureWaits(t *testing.T) {
	f := newUpFixture(t)
	p := rebootable(f)
	failingFirst(p, false)
	s := waitingOn(t, lanSelf(), p)
	done := bootAsync(t, s)
	put(t, s, sectionOf(1, dnsHost, smallJSON))
	select {
	case b := <-done:
		if !errors.Is(b.err, ErrRestart) {
			t.Fatalf("boot = %v, want ErrRestart", b.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the boot did not end")
	}
	if st := stateOf(t, s); st.Section != nil || st.Fallback != nil || st.Refused == nil {
		t.Errorf("state %+v, want no section and the refusal saved", st)
	}
	r := restarted(s, rebootable(newUpFixture(t)))
	r.bootSaved(t.Context())
	if st := r.sectionStatus(); st.State != hostapi.SectionWaiting || st.Refused == nil || st.Refused.Errors[0].Path != "hosts.lan.dns" {
		t.Fatalf("after the restart, section status = %+v; want waiting with the refusal", st)
	}
	_, err := apiOf(r).PutSection(t.Context(), sectionOf(2, dnsHost, smallJSON))
	if got := refusedAt(t, err); !slices.Equal(got, []string{"hosts.lan.dns"}) {
		t.Errorf("the same settings refused at %q", got)
	}
	if ans := put(t, r, sectionOf(3, lanHost, smallJSON)); ans.Applies != hostapi.SectionApplied {
		t.Errorf("a corrected PUT = %+v", ans)
	}
	if got := r.awaitSection(t.Context()); got == nil || got.raw.Revision != 3 {
		t.Errorf("boots on %+v, want revision 3", got)
	}
}

// A section accepted for a restart saves the one the host runs as its
// fallback, which any later section that does not restart the host drops.
func TestARestartKeepsTheRunningSectionAsFallback(t *testing.T) {
	before := sectionOf(1, lanHost, smallJSON)
	for _, tc := range []struct {
		name string
		then *hostapi.Section
	}{
		{"back on the running settings", sectionOf(3, lanHost, smallJSON)},
		{"a change applied at once", sectionOf(3, lanHost, smallJSON, largeJSON)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpFixture(t)
			f.kernel = newFakeIPT(true)
			s, _ := upOn(t, before, f.platform())
			restart := sectionOf(2, ipv6Host, smallJSON)
			put(t, s, restart)
			st := stateOf(t, s)
			if st.Section == nil || hashOf(t, st.Section) != hashOf(t, restart) || !st.Unbooted || st.Fallback == nil || hashOf(t, st.Fallback) != hashOf(t, before) || st.Fallback.Revision != 1 {
				t.Fatalf("state %+v; want the restart's section not booted, with revision 1 of the running section to fall back to", st)
			}
			put(t, s, tc.then)
			if !noFallback(stateOf(t, s)) {
				t.Error("the fallback outlived the restart it was kept for")
			}
		})
	}
}

// A new revision of the section a restart waits for keeps the restart's
// fallback.
func TestARestartDeliveredAgainKeepsItsFallback(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	put(t, s, sectionOf(2, dnsHost, smallJSON))
	if ans := put(t, s, sectionOf(3, dnsHost, smallJSON)); ans.Applies != hostapi.SectionPendingRestart {
		t.Fatalf("PUT = %+v, want the restart still pending", ans)
	}
	if st := stateOf(t, s); st.Section == nil || st.Section.Revision != 3 || !st.Unbooted || st.Fallback == nil || st.Fallback.Revision != 1 {
		t.Errorf("state %+v; want revision 3 not booted, with revision 1 to fall back to", st)
	}
}

// A section the host fails to save leaves the fallback of the restart it
// accepted, whichever way the new section applies.
func TestAFailedSaveKeepsTheFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		then *hostapi.Section
	}{
		{"back on the running settings", sectionOf(3, lanHost, smallJSON)},
		{"a change applied at once", sectionOf(3, lanHost, smallJSON, largeJSON)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
			put(t, s, sectionOf(2, dnsHost, smallJSON))
			failSaves(t, filesafe.PhaseRename)
			if _, err := apiOf(s).PutSection(t.Context(), tc.then); err == nil {
				t.Fatal("PUT succeeded with its save failing")
			}
			if st := stateOf(t, s); !st.Unbooted || st.Fallback == nil || st.Fallback.Revision != 1 || st.Section.Revision != 2 {
				t.Errorf("state %+v; want revision 2's restart with its fallback kept", st)
			}
		})
	}
}

// A boot refusal whose fallback is not saved in the refused section's place
// keeps the saved state and boots the fallback now; the next start is
// refused the same way and boots it again.
func TestABootRefusalThatFailsToSaveKeepsTheFallback(t *testing.T) {
	f := newUpFixture(t)
	p := rebootable(f)
	failingFirst(p, false)
	before, failing := sectionOf(1, lanHost, smallJSON), sectionOf(2, dnsHost, smallJSON)
	s := waitingOn(t, lanSelf(), p)
	saveUnbooted(t, s, failing, before)
	restore := failSaves(t, filesafe.PhaseTempSync)
	s.bootSaved(t.Context())
	bootUp(t, s)
	if s.running.hash != hashOf(t, before) || noFallback(stateOf(t, s)) {
		t.Fatalf("booted revision %d, state %+v; want revision 1 with the saved state kept", s.running.raw.Revision, stateOf(t, s))
	}
	restore()
	g := newUpFixture(t)
	q := rebootable(g)
	failingFirst(q, false)
	r := restarted(s, q)
	r.bootSaved(t.Context())
	bootUp(t, r)
	if r.running.hash != hashOf(t, before) {
		t.Errorf("the next start booted revision %d, want 1", r.running.raw.Revision)
	}
	if saved, ok, err := readSection(t.Context(), r.sectionPath); !ok || err != nil || saved.Revision != 1 {
		t.Errorf("saved %+v, %v, %v; want revision 1", saved, ok, err)
	}
}

// A boot that comes up on the section it restarted for drops the fallback.
func TestABootThatComesUpDropsTheFallback(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	if ans := put(t, s, sectionOf(2, dnsHost, smallJSON)); ans.Applies != hostapi.SectionPendingRestart {
		t.Fatalf("setup: the dns section = %+v, want a restart", ans)
	}
	r := restarted(s, newUpFixture(t).platform())
	r.bootSaved(t.Context())
	bootUp(t, r)
	if r.running.raw.Revision != 2 {
		t.Errorf("booted revision %d, want 2", r.running.raw.Revision)
	}
	if !noFallback(stateOf(t, r)) {
		t.Error("the fallback outlived the boot")
	}
}

// A boot that refuses both the saved section and its fallback waits with
// neither saved, reporting the fallback's refusal.
func TestABootRefusingItsFallbackToo(t *testing.T) {
	f := newUpFixture(t)
	s := waitingOn(t, lanSelf(), f.platform())
	saveUnbooted(t, s, sectionOf(2, ipv6Host, smallJSON), sectionOf(1, shadowedHost, smallJSON))
	s.bootSaved(t.Context())
	bootAsync(t, s)
	waitFor(t, "both refusals with neither section saved", func() bool {
		st := s.sectionStatus()
		return st.State == hostapi.SectionWaiting && st.Refused != nil && st.Refused.Errors[0].Path == "hosts.lan.network.subnet" &&
			stateOf(t, s).Section == nil && stateOf(t, s).Fallback == nil
	})
}

// A saved section with no fallback that the boot refuses leaves the host
// waiting with the refusal and nothing saved, never ending the run; the
// next section it accepts boots.
func TestABootRefusalWithNoFallbackWaits(t *testing.T) {
	f := newUpFixture(t)
	s := waitingOn(t, lanSelf(), f.platform())
	if err := writeSection(t.Context(), s.sectionPath, sectionOf(1, shadowedHost, smallJSON)); err != nil {
		t.Fatal(err)
	}
	s.bootSaved(t.Context())
	done := bootAsync(t, s)
	waitFor(t, "the boot's refusal with nothing saved", func() bool {
		st := s.sectionStatus()
		return st.State == hostapi.SectionWaiting && st.Refused != nil && stateOf(t, s).Section == nil
	})
	t.Run("waiting, with the refusal", func(t *testing.T) {
		if st := s.sectionStatus(); st.Refused.Errors[0].Path != "hosts.lan.network.subnet" || st.Hash != "" {
			t.Errorf("section status = %+v", st)
		}
		if len(f.events) != 0 {
			t.Errorf("events %q, want nothing loaded or started", f.events)
		}
	})
	if ans := put(t, s, sectionOf(2, lanHost, smallJSON)); ans.Applies != hostapi.SectionApplied {
		t.Fatalf("PUT = %+v", ans)
	}
	select {
	case b := <-done:
		if b.err != nil || b.n == nil {
			t.Fatalf("boot = %v, %v", b.n, b.err)
		}
		t.Cleanup(func() { _ = b.n.stop() })
		if s.running.raw.Revision != 2 || b.n.Subnet != pfx("172.18.0.0/16") {
			t.Errorf("booted revision %d on %v", s.running.raw.Revision, b.n.Subnet)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the boot did not go on with the delivered section")
	}
}

// A restart's subnet check leaves out the job bridge's own route, which
// the restart takes down with the job network.
func TestARestartMayReuseTheSubnetInUse(t *testing.T) {
	f := newUpFixture(t)
	s, _ := upOn(t, sectionOf(1, lanHost, smallJSON), f.platform())
	writeAt(t, f.root, "/proc/net/route", route4+"manade0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n")
	if ans := put(t, s, sectionOf(2, `{"network": {"subnet": "172.18.5.0/24"}}`, smallJSON)); ans.Applies != hostapi.SectionPendingRestart {
		t.Errorf("PUT = %+v, want a restart", ans)
	}
}
