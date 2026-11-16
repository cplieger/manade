package host

import (
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

// The host parts its network refuses at a boot on the fake kernel: route4's
// on-link 172.17.0.0/16 shadows the subnet, and the namespace has no IPv6
// address or default route.
const (
	shadowedHost = `{"network": {"subnet": "172.17.5.0/24"}}`
	ipv6Host     = `{"egress": {"ipv6": true}}`
)

// A first section the host's network refuses is answered 422 and saved
// nowhere, and the host keeps waiting, holding the refusal, for a section
// it boots on.
func TestAFirstSectionTheNetworkRefuses(t *testing.T) {
	for _, tc := range []struct{ name, host, path string }{
		{"a subnet an on-link route shadows", shadowedHost, "hosts.lan.network.subnet"},
		{"ipv6 with no IPv6 in the namespace", ipv6Host, "hosts.lan.egress.ipv6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpFixture(t)
			f.kernel = newFakeIPT(true)
			s := waitingOn(t, lanSelf(), f.platform())
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
	r := newSupervisor(lanSelf(), &Options{Log: slog.New(slog.DiscardHandler)}, s.sectionPath, g.platform())
	r.bootSaved(t.Context())
	n, err := r.boot(t.Context())
	if err != nil || n == nil {
		t.Fatalf("boot = %v, %v; want the section before booted", n, err)
	}
	t.Cleanup(func() { _ = n.stop() })
	r.net = n
	t.Run("it boots on the section it ran before, saved again", func(t *testing.T) {
		if r.running == nil || r.running.hash != hashOf(t, before) || g.proc.stopped != 0 || g.spec == nil {
			t.Errorf("running %+v, daemon started %v", r.running, g.spec != nil)
		}
		if saved, ok, err := readSection(t.Context(), r.sectionPath); !ok || err != nil || saved.Revision != 1 {
			t.Errorf("saved %+v, %v, %v; want revision 1", saved, ok, err)
		}
		if _, err := os.Stat(r.fallbackPath); !os.IsNotExist(err) {
			t.Errorf("the fallback is still saved: %v", err)
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
		if _, err := os.Stat(r.fallbackPath); !os.IsNotExist(err) {
			t.Errorf("a refused section saved a fallback: %v", err)
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
			put(t, s, sectionOf(2, ipv6Host, smallJSON))
			if fb, ok, err := readSection(t.Context(), s.fallbackPath); !ok || err != nil || hashOf(t, fb) != hashOf(t, before) || fb.Revision != 1 {
				t.Fatalf("fallback %+v, %v, %v; want revision 1 of the running section", fb, ok, err)
			}
			put(t, s, tc.then)
			if _, err := os.Stat(s.fallbackPath); !os.IsNotExist(err) {
				t.Errorf("the fallback outlived the restart it was kept for: %v", err)
			}
		})
	}
}

// A boot that comes up on the section it restarted for drops the fallback.
func TestABootThatComesUpDropsTheFallback(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	if ans := put(t, s, sectionOf(2, dnsHost, smallJSON)); ans.Applies != hostapi.SectionPendingRestart {
		t.Fatalf("setup: the dns section = %+v, want a restart", ans)
	}
	r := newSupervisor(lanSelf(), &Options{Log: slog.New(slog.DiscardHandler)}, s.sectionPath, newUpFixture(t).platform())
	r.bootSaved(t.Context())
	n, err := r.boot(t.Context())
	if err != nil || n == nil {
		t.Fatalf("boot = %v, %v", n, err)
	}
	t.Cleanup(func() { _ = n.stop() })
	if r.running.raw.Revision != 2 {
		t.Errorf("booted revision %d, want 2", r.running.raw.Revision)
	}
	if _, err := os.Stat(r.fallbackPath); !os.IsNotExist(err) {
		t.Errorf("the fallback outlived the boot: %v", err)
	}
}

// A boot that refuses both the saved section and its fallback waits with
// neither saved, reporting the fallback's refusal.
func TestABootRefusingItsFallbackToo(t *testing.T) {
	f := newUpFixture(t)
	s := waitingOn(t, lanSelf(), f.platform())
	if err := writeSection(t.Context(), s.sectionPath, sectionOf(2, ipv6Host, smallJSON)); err != nil {
		t.Fatal(err)
	}
	if err := writeSection(t.Context(), s.fallbackPath, sectionOf(1, shadowedHost, smallJSON)); err != nil {
		t.Fatal(err)
	}
	s.bootSaved(t.Context())
	go func() { _, _ = s.boot(t.Context()) }()
	waitFor(t, "both refusals", func() bool {
		st := s.sectionStatus()
		return st.State == hostapi.SectionWaiting && st.Refused != nil && st.Refused.Errors[0].Path == "hosts.lan.network.subnet"
	})
	for _, p := range []string{s.sectionPath, s.fallbackPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still saved: %v", p, err)
		}
	}
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
	type booted struct {
		n   *hostNet
		err error
	}
	done := make(chan booted, 1)
	go func() {
		n, err := s.boot(t.Context())
		done <- booted{n, err}
	}()
	waitFor(t, "the boot's refusal", func() bool {
		st := s.sectionStatus()
		return st.State == hostapi.SectionWaiting && st.Refused != nil
	})
	t.Run("waiting, with the refusal and nothing saved", func(t *testing.T) {
		if st := s.sectionStatus(); st.Refused.Errors[0].Path != "hosts.lan.network.subnet" || st.Hash != "" {
			t.Errorf("section status = %+v", st)
		}
		if _, ok, err := readSection(t.Context(), s.sectionPath); ok || err != nil {
			t.Errorf("the refused section is still saved: %v, %v", ok, err)
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
