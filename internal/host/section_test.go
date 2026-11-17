package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/images"
	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/host/probe"
	"github.com/cplieger/manade/internal/hostapi"
)

// lanSelf is host lan's environment, whose ceiling lets it be open, use
// IPv6 and reach 10.0.0.0/8.
func lanSelf() *config.Self {
	return &config.Self{Name: "lan", Ceiling: config.Ceiling{
		Open: true, IPv6: true, Private: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}}
}

// The host parts and profiles of host lan's sections.
const (
	lanHost    = `{}`
	allowHost  = `{"egress": {"allow": [{"to": "10.0.0.50", "ports": [5432]}]}}`
	openHost   = `{"egress": {"open": true}}`
	closedHost = `{"egress": {"internet": false}}`
	dnsHost    = `{"dns": ["9.9.9.9"]}`
	smallJSON  = `"small": {"runner_image": "base", "cpus": 1, "memory": "512MiB", "scopes": ["acme/app"]}`
	largeJSON  = `"large": {"runner_image": "base", "cpus": 2, "memory": "512MiB", "scopes": ["acme/app"]}`
)

// sectionOf is host lan's section at revision, its host part host, serving
// profiles.
func sectionOf(revision uint64, host string, profiles ...string) *hostapi.Section {
	return &hostapi.Section{Revision: revision, Settings: []byte(`{"hosts": {"lan": ` + host + `}, "profiles": {` + strings.Join(profiles, ", ") + `}}`)}
}

// upSupervisor is host lan up on sec, brought up on the fake kernel, its
// section saved under a temporary directory; its run's cause reads why
// the run was cancelled.
func upSupervisor(t *testing.T, sec *hostapi.Section) (s *supervisor, f *upFixture, run context.Context) {
	t.Helper()
	f = newUpFixture(t)
	s, run = upOn(t, sec, f.platform())
	return s, f, run
}

// upOn is upSupervisor on platform p.
func upOn(t *testing.T, sec *hostapi.Section, p *seams) (s *supervisor, run context.Context) {
	t.Helper()
	return upAs(t, lanSelf(), sec, p)
}

// upAs is upOn for host lan with environment self.
func upAs(t *testing.T, self *config.Self, sec *hostapi.Section, p *seams) (s *supervisor, run context.Context) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	s = newSupervisor(self, &Options{Log: log}, filepath.Join(dir, "section.json"), p)
	first, err := check(s.self, sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSection(t.Context(), s.sectionPath, sec); err != nil {
		t.Fatal(err)
	}
	s.state = hostState{Section: sec}
	if s.oom, err = platform.LoadOOMTotal(t.Context(), filepath.Join(dir, "oom-kills.json")); err != nil {
		t.Fatal(err)
	}
	if s.net, err = bringUp(t.Context(), first.host, p, guardHooks{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.net.stop() })
	s.running = first
	return s, finishUp(t, s)
}

// finishUp makes s, whose network is up on the section it runs, a host
// that is up; its run's cause reads why the run was cancelled.
func finishUp(t *testing.T, s *supervisor) (run context.Context) {
	t.Helper()
	sv := s.newServed(s.running.cfg, s.running.host)
	s.served.Store(sv)
	s.prober = probe.NewRunner(&probe.Config{Log: s.log, Host: s.probeHost})
	s.images = images.New(&images.Config{Host: s.running.host, Log: s.log, Profiles: sv.Profiles})
	s.runners = newRunners(t.Context(), &runnersConfig{Served: sv, Images: s.images, Log: s.log, Cgroups: newFakeCgroups()})
	run, s.cancelRun = context.WithCancelCause(t.Context())
	s.sectionMu.Lock()
	s.phase.Store(phaseUp)
	s.publish()
	s.sectionMu.Unlock()
	return run
}

// changing runs s's posture hold and its change loop under run until the
// test ends.
func changing(t *testing.T, s *supervisor, run context.Context) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := s.net.hold(run); err != nil {
			t.Errorf("Hold = %v", err)
		}
	})
	wg.Go(func() { s.changes(run, s.cancelRun) })
	t.Cleanup(func() {
		s.cancelRun(nil)
		wg.Wait()
	})
}

// running makes runner name a running container of the host.
func running(s *supervisor, name string) {
	s.runners.mu.Lock()
	defer s.runners.mu.Unlock()
	s.runners.ctrs[name] = &hostapi.Container{Name: name, State: docker.StateRunning}
}

// exit ends runner name.
func exit(s *supervisor, name string) {
	s.runners.mu.Lock()
	defer s.runners.mu.Unlock()
	s.runners.ctrs[name].State = docker.StateExited
	s.runners.changed(name)
}

func deliver(t *testing.T, s *supervisor, sec *hostapi.Section) hostapi.SectionAnswer {
	t.Helper()
	ans, err := s.deliver(t.Context(), sec)
	if err != nil {
		t.Fatalf("deliver = %v", err)
	}
	return ans
}

// heldFor reports a start refused because the host holds its starts for
// reason.
func heldFor(err error, reason hostapi.Reason) bool {
	nr, ok := errors.AsType[*hostapi.NotReadyError](err)
	return ok && nr.Reason == reason
}

// pendingFails is how often the pending section's apply has failed.
func pendingFails(s *supervisor) int {
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	if s.pending == nil {
		return 0
	}
	return s.pending.fails
}

func hashOf(t *testing.T, sec *hostapi.Section) string {
	t.Helper()
	h, err := sec.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// readSection answers the section the state at path boots, false when it
// boots none.
func readSection(ctx context.Context, path string) (*hostapi.Section, bool, error) {
	st, _, err := readState(ctx, path)
	return st.Section, st.Section != nil, err
}

// writeSection saves sec at path as a section that booted on the host.
func writeSection(ctx context.Context, path string, sec *hostapi.Section) error {
	return writeState(ctx, path, &hostState{Section: sec})
}

// stateOf is the state s saved.
func stateOf(t *testing.T, s *supervisor) hostState {
	t.Helper()
	st, _, err := readState(t.Context(), s.sectionPath)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// noFallback reports whether st boots its section as booted.
func noFallback(st hostState) bool { return st.Fallback == nil && !st.Unbooted }

func TestASectionIsSavedBeforeItsAnswer(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	next := sectionOf(2, lanHost, smallJSON, largeJSON)
	deliver(t, s, next)
	t.Run("the delivered section, whole", func(t *testing.T) {
		got, ok, err := readSection(t.Context(), s.sectionPath)
		if err != nil || !ok || got.Revision != 2 || hashOf(t, got) != hashOf(t, next) {
			t.Errorf("saved %+v, %v, %v; want revision 2 of the delivered settings", got, ok, err)
		}
	})
	t.Run("readable by the host alone", func(t *testing.T) {
		if st, err := os.Stat(s.sectionPath); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, %v", st, err)
		}
	})
	t.Run("a refused section leaves it", func(t *testing.T) {
		before, err := os.ReadFile(s.sectionPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.deliver(t.Context(), sectionOf(3, `{"capacity": {"cpus": 1, "memory": "1GiB"}}`)); err == nil {
			t.Fatal("a section with capacity was accepted")
		}
		if after, _ := os.ReadFile(s.sectionPath); string(after) != string(before) {
			t.Errorf("the refused section changed the saved one: %s", after)
		}
	})
}

func TestABootWithNoControlPlaneRunsTheSavedSection(t *testing.T) {
	sec := sectionOf(7, allowHost, smallJSON)
	s := waitingAs(t, lanSelf())
	if err := writeSection(t.Context(), s.sectionPath, sec); err != nil {
		t.Fatal(err)
	}
	s.bootSaved(t.Context())
	got := s.awaitSection(t.Context())
	if got == nil || got.hash != hashOf(t, sec) || len(got.host.Egress.Allow) != 1 || got.raw.Revision != 7 {
		t.Fatalf("boot section = %+v, want the saved one", got)
	}
	if st := s.sectionStatus(); st.State != hostapi.SectionRunning || st.Hash != got.hash || st.Revision != 7 {
		t.Errorf("section status = %+v", st)
	}
}

func TestAFirstBootWaitsForItsSection(t *testing.T) {
	s := waitingAs(t, lanSelf())
	s.opts.Version = "v1.0.0-dev.21"
	b := &backend{s: s}
	booted := make(chan *accepted, 1)
	go func() { booted <- s.awaitSection(t.Context()) }()
	t.Run("waiting, it reports so and serves no runner", func(t *testing.T) {
		st := b.Status(t.Context())
		if st.Section.State != hostapi.SectionWaiting || st.Section.Hash != "" || st.Version != "v1.0.0-dev.21" || st.Profiles != nil {
			t.Errorf("status = %+v", st)
		}
		if _, err := b.Runners(t.Context()); !errors.Is(err, hostapi.ErrWaitingForSection) {
			t.Errorf("Runners = %v", err)
		}
		if _, err := b.CreateRunner(t.Context(), request("r1", "small")); !errors.Is(err, hostapi.ErrWaitingForSection) {
			t.Errorf("CreateRunner = %v", err)
		}
		if _, err := b.Probe(t.Context()); !errors.Is(err, hostapi.ErrWaitingForSection) {
			t.Errorf("Probe = %v", err)
		}
		if _, err := b.Subscribe(t.Context()); !errors.Is(err, hostapi.ErrWaitingForSection) {
			t.Errorf("Subscribe = %v", err)
		}
	})
	select {
	case <-booted:
		t.Fatal("the boot went on with no section")
	default:
	}
	t.Run("a refused first section changes nothing", func(t *testing.T) {
		if _, err := b.PutSection(t.Context(), sectionOf(1, `{"bogus": 1}`)); err == nil {
			t.Fatal("refused nothing")
		}
		if st := b.Status(t.Context()).Section; st.State != hostapi.SectionWaiting || st.Refused == nil || st.Refused.Errors[0].Path != "hosts.lan.bogus" {
			t.Errorf("section status = %+v", st)
		}
	})
	sec := sectionOf(1, lanHost, smallJSON)
	ans, err := b.PutSection(t.Context(), sec)
	if err != nil || ans.Applies != hostapi.SectionApplied || ans.Hash != hashOf(t, sec) {
		t.Fatalf("PutSection = %+v, %v", ans, err)
	}
	if got := <-booted; got == nil || got.hash != hashOf(t, sec) {
		t.Fatalf("booted on %+v", got)
	}
	t.Run("the first section is saved", func(t *testing.T) {
		if saved, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil || saved.Revision != 1 {
			t.Errorf("saved %+v, %v, %v", saved, ok, err)
		}
	})
	t.Run("booting, it answers that it starts", func(t *testing.T) {
		if _, err := b.PutSection(t.Context(), sectionOf(2, lanHost)); !errors.Is(err, hostapi.ErrStarting) {
			t.Errorf("PutSection = %v", err)
		}
		if _, err := b.PutSection(t.Context(), sectionOf(3, `{"bogus": 1}`)); !errors.Is(err, hostapi.ErrStarting) {
			t.Errorf("PutSection of a section the parser refuses = %v, want ErrStarting and nothing judged", err)
		}
		if _, err := b.Runners(t.Context()); !errors.Is(err, hostapi.ErrStarting) {
			t.Errorf("Runners = %v", err)
		}
		if st := b.Status(t.Context()).Section; st.State != hostapi.SectionRunning || st.Refused != nil {
			t.Errorf("section status = %+v", st)
		}
	})
}

// A saved section the build no longer accepts is reported, and the host
// waits for the control plane's.
func TestASavedSectionThatNoLongerValidates(t *testing.T) {
	s := waitingAs(t, lanSelf())
	if err := writeSection(t.Context(), s.sectionPath, sectionOf(3, `{"address": "10.0.0.1:7464"}`)); err != nil {
		t.Fatal(err)
	}
	s.bootSaved(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if got := s.awaitSection(ctx); got != nil {
		t.Fatalf("booted on %+v", got)
	}
	if st := s.sectionStatus(); st.State != hostapi.SectionWaiting || st.Refused == nil || st.Refused.Errors[0].Path != "hosts.lan.address" {
		t.Errorf("section status = %+v", st)
	}
}

// Each row, delivered to a host with no runner.
func TestEachRowApplies(t *testing.T) {
	t.Run("profiles at once, the rules kept", func(t *testing.T) {
		s, f, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		rules, policy := f.kernel.save(false), egress.PolicyHash(s.net.section())
		ans := deliver(t, s, sectionOf(2, lanHost, smallJSON, largeJSON))
		if ans.Applies != hostapi.SectionApplied || len(s.served.Load().Profiles) != 2 || len(s.images.Status()) != 2 {
			t.Errorf("answer %+v, %d profiles served", ans, len(s.served.Load().Profiles))
		}
		if string(f.kernel.save(false)) != string(rules) || egress.PolicyHash(s.net.section()) != policy {
			t.Error("a profile change touched the rules or the policy hash")
		}
	})
	t.Run("images.refresh at once, the policy hash kept", func(t *testing.T) {
		s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		policy := egress.PolicyHash(s.net.section())
		sec := &hostapi.Section{Revision: 2, Settings: []byte(`{"hosts": {"lan": {}}, "profiles": {` + smallJSON + `}, "images": {"refresh": "30m"}}`)}
		if ans := deliver(t, s, sec); ans.Applies != hostapi.SectionApplied || s.host().ImageRefresh != 30*time.Minute {
			t.Errorf("answer %+v, refresh %s", ans, s.host().ImageRefresh)
		}
		if egress.PolicyHash(s.net.section()) != policy {
			t.Error("images.refresh changed the policy hash")
		}
	})
	t.Run("a socket host's dial address applied, no rule changed", func(t *testing.T) {
		s, f, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		rules, policy := f.kernel.save(false), egress.PolicyHash(s.net.section())
		sec := &hostapi.Section{Revision: 2, Settings: []byte(`{"hosts": {"lan": {}}, "profiles": {` + smallJSON + `}, "pool": {"host_address": "cp.example:7464"}}`)}
		if ans := deliver(t, s, sec); ans.Applies != hostapi.SectionApplied || s.running.raw.Revision != 2 {
			t.Errorf("answer %+v, running revision %d", ans, s.running.raw.Revision)
		}
		if string(f.kernel.save(false)) != string(rules) || egress.PolicyHash(s.net.section()) != policy {
			t.Error("the dial address touched a socket host's rules or policy hash")
		}
	})
	t.Run("allow swapped at once", func(t *testing.T) {
		s, f, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		policy := egress.PolicyHash(s.net.section())
		if ans := deliver(t, s, sectionOf(2, allowHost, smallJSON)); ans.Applies != hostapi.SectionApplied {
			t.Errorf("answer %+v", ans)
		}
		if !slices.ContainsFunc(f.kernel.chain(false, "filter", "MANADE-FWD"), func(r string) bool { return strings.Contains(r, "10.0.0.50/32") }) {
			t.Error("the allow entry was not loaded")
		}
		if egress.PolicyHash(s.net.section()) == policy || len(s.host().Egress.Allow) != 1 {
			t.Error("the swap left the policy hash or the section served")
		}
	})
	t.Run("a swap that fails keeps the old rules and holds starts", func(t *testing.T) {
		s, f, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		f.kernel.failOnce[platform.LegacyDir+"/iptables-restore"] = errors.New("refused")
		ans := deliver(t, s, sectionOf(2, allowHost, smallJSON))
		if ans.Applies != hostapi.SectionFailed || ans.Detail == "" || len(s.host().Egress.Allow) != 0 {
			t.Errorf("answer %+v, allow %v", ans, s.host().Egress.Allow)
		}
		if st := s.sectionStatus(); st.State != hostapi.SectionEgressFailed || st.Hash == st.PendingHash || st.RetryAt.IsZero() {
			t.Errorf("section status = %+v", st)
		}
		if st := s.runners.profileStatus(); st[0].Ready || st[0].Reason != hostapi.ReasonEgressChangeFailed {
			t.Errorf("profiles = %+v", st)
		}
	})
	t.Run("open waits for no runner and rebuilds", func(t *testing.T) {
		s, _, run := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		changing(t, s, run)
		deliver(t, s, sectionOf(2, openHost, smallJSON))
		waitFor(t, "the open posture", func() bool { return s.net.section().Egress.Open && s.host().Egress.Open })
		waitFor(t, "the section applied", func() bool { return s.sectionStatus().State == hostapi.SectionRunning })
	})
	t.Run("dns restarts the host once no runner runs", func(t *testing.T) {
		s, _, run := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		changing(t, s, run)
		ans := deliver(t, s, sectionOf(2, dnsHost, smallJSON))
		if ans.Applies != hostapi.SectionPendingRestart {
			t.Errorf("answer %+v", ans)
		}
		<-run.Done()
		if !errors.Is(context.Cause(run), ErrRestart) || !closed(s.restarting) {
			t.Errorf("cause %v, announced %v", context.Cause(run), closed(s.restarting))
		}
		if saved, _, _ := readSection(t.Context(), s.sectionPath); saved == nil || saved.Revision != 2 {
			t.Errorf("saved %+v, want the section the restart boots on", saved)
		}
	})
}

// A drain row with a runner running: no start until it ends, then the row's
// apply, each answer and status naming what it waits for.
func TestADrainWaitsForTheRunners(t *testing.T) {
	for name, tc := range map[string]struct {
		host    string
		applies hostapi.SectionApplies
		state   hostapi.SectionState
		reason  hostapi.Reason
	}{
		"internet":                          {closedHost, hostapi.SectionPendingDrain, hostapi.SectionEgressPending, hostapi.ReasonEgressChangePending},
		"subnet":                            {`{"network": {"subnet": "10.250.1.0/24"}}`, hostapi.SectionPendingRestart, hostapi.SectionRestartPending, hostapi.ReasonRestartPending},
		"dns":                               {dnsHost, hostapi.SectionPendingRestart, hostapi.SectionRestartPending, hostapi.ReasonRestartPending},
		"the strictest of allow and open":   {`{"egress": {"open": true}}`, hostapi.SectionPendingDrain, hostapi.SectionEgressPending, hostapi.ReasonEgressChangePending},
		"the strictest of dns and internet": {`{"dns": ["9.9.9.9"], "egress": {"internet": false}}`, hostapi.SectionPendingRestart, hostapi.SectionRestartPending, hostapi.ReasonRestartPending},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, run := upSupervisor(t, sectionOf(1, allowHost, smallJSON))
			changing(t, s, run)
			running(s, "r1")
			ans := deliver(t, s, sectionOf(2, tc.host, smallJSON))
			if ans.Applies != tc.applies || !slices.Equal(ans.Waiting, []string{"r1"}) {
				t.Errorf("answer %+v", ans)
			}
			if st := s.sectionStatus(); st.State != tc.state || !slices.Equal(st.Waiting, []string{"r1"}) || st.PendingRevision != 2 || st.Revision != 1 {
				t.Errorf("section status = %+v", st)
			}
			if st := s.runners.profileStatus(); st[0].Ready || st[0].Reason != tc.reason || !strings.Contains(st[0].Detail, "r1") {
				t.Errorf("profiles = %+v", st)
			}
			if _, err := s.runners.accept(t.Context(), request("r2", "small")); !heldFor(err, tc.reason) {
				t.Errorf("a start while the section waits = %v", err)
			}
			time.Sleep(50 * time.Millisecond)
			if s.running.raw.Revision != 1 || run.Err() != nil {
				t.Fatal("the section applied with r1 running")
			}
			exit(s, "r1")
			if tc.applies == hostapi.SectionPendingRestart {
				<-run.Done()
				if !errors.Is(context.Cause(run), ErrRestart) {
					t.Errorf("cause = %v", context.Cause(run))
				}
				return
			}
			waitFor(t, "the section applied", func() bool { return s.sectionStatus().State == hostapi.SectionRunning })
			if s.running.raw.Revision != 2 {
				t.Errorf("running revision %d", s.running.raw.Revision)
			}
		})
	}
}

func TestAWaitingSection(t *testing.T) {
	waiting := func(t *testing.T, host string) *supervisor {
		t.Helper()
		s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		running(s, "r1")
		deliver(t, s, sectionOf(2, host, smallJSON))
		return s
	}
	t.Run("a newer section replaces it and keeps its runners", func(t *testing.T) {
		s := waiting(t, closedHost)
		ans := deliver(t, s, sectionOf(3, dnsHost, smallJSON))
		if ans.Applies != hostapi.SectionPendingRestart || !slices.Equal(ans.Waiting, []string{"r1"}) || s.pending.next.raw.Revision != 3 {
			t.Errorf("answer %+v", ans)
		}
	})
	t.Run("a section back to the running values cancels the wait", func(t *testing.T) {
		s := waiting(t, closedHost)
		ans := deliver(t, s, sectionOf(3, lanHost, smallJSON))
		if ans.Applies != hostapi.SectionApplied || s.pending != nil || s.runners.egressIdle() != nil {
			t.Errorf("answer %+v, pending %+v", ans, s.pending)
		}
		if st := s.sectionStatus(); st.State != hostapi.SectionRunning || st.PendingHash != "" || st.Revision != 3 {
			t.Errorf("section status = %+v", st)
		}
		if _, err := s.runners.accept(t.Context(), request("r2", "small")); heldFor(err, hostapi.ReasonEgressChangePending) {
			t.Errorf("a start after the cancel = %v", err)
		}
	})
	t.Run("a section back on the running posture with another profile applies at once", func(t *testing.T) {
		s := waiting(t, closedHost)
		if ans := deliver(t, s, sectionOf(3, lanHost, smallJSON, largeJSON)); ans.Applies != hostapi.SectionApplied || s.pending != nil {
			t.Errorf("answer %+v", ans)
		}
	})
	t.Run("the same section again changes nothing", func(t *testing.T) {
		s := waiting(t, closedHost)
		before := s.pending
		if ans := deliver(t, s, sectionOf(4, closedHost, smallJSON)); ans.Applies != hostapi.SectionPendingDrain || s.pending != before || before.next.raw.Revision != 4 {
			t.Errorf("answer %+v", ans)
		}
	})
}

func TestARefusedSection(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	_, err := s.deliver(t.Context(), sectionOf(2, `{"egress": {"allow": [{"to": "172.18.3.4", "ports": [5432]}]}}`, smallJSON))
	sr, ok := errors.AsType[*hostapi.SectionRefusedError](err)
	if !ok || len(sr.Errors) != 1 || sr.Errors[0].Path != "hosts.lan.egress.allow[0].to" {
		t.Fatalf("err = %v", err)
	}
	if st := s.sectionStatus(); st.State != hostapi.SectionRefused || st.Revision != 1 || st.Refused == nil || st.Refused.Errors[0].Path != "hosts.lan.egress.allow[0].to" {
		t.Errorf("section status = %+v", st)
	}
	if len(s.host().Egress.Allow) != 0 {
		t.Error("the refused section was applied")
	}
	deliver(t, s, sectionOf(3, lanHost, smallJSON, largeJSON))
	if st := s.sectionStatus(); st.State != hostapi.SectionRunning || st.Refused != nil {
		t.Errorf("after an accepted one: %+v", st)
	}
}

// TestARefusalIsLoggedClean delivers a section whose unknown key holds a
// bidi rune, which the refusal's path carries.
func TestARefusalIsLoggedClean(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	var logs bytes.Buffer
	s.log = slog.New(slog.NewJSONHandler(&logs, nil))
	_, err := s.deliver(t.Context(), sectionOf(2, `{"x\u202ey": 1}`, smallJSON))
	if _, ok := errors.AsType[*hostapi.SectionRefusedError](err); !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
	var rec struct{ Msg, Error string }
	if err := json.Unmarshal(logs.Bytes(), &rec); err != nil || strings.Count(logs.String(), "\n") != 1 {
		t.Fatalf("logs = %q (%v), want one JSON line", logs.String(), err)
	}
	if rec.Msg != "The host refused a section and runs the one it had" || rec.Error != "the host refused the section: hosts.lan.x y: unknown key" {
		t.Errorf("logged %+v, want the refusal with its bidi rune replaced", rec)
	}
}

func TestRetryAfter(t *testing.T) {
	var got []time.Duration
	for fails := 1; fails <= 6; fails++ {
		got = append(got, retryAfter(fails))
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	if !slices.Equal(got, want) {
		t.Errorf("retries = %v, want %v", got, want)
	}
}

// A failed rebuild is tried again on its own schedule and at once on a PUT
// of the same section, and ends ready with a boot's rules.
func TestAFailedRebuildIsRetried(t *testing.T) {
	failing := func(t *testing.T) (*supervisor, *upFixture, *atomic.Bool) {
		t.Helper()
		s, f, run := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
		fail := &atomic.Bool{}
		fail.Store(true)
		s.net.seams.FetchDomains = func(context.Context) ([]byte, error) {
			if fail.Load() {
				return nil, errors.New("403 rate limit exceeded")
			}
			return []byte(metaBody), nil
		}
		changing(t, s, run)
		deliver(t, s, sectionOf(2, closedHost, smallJSON))
		waitFor(t, "the rebuild failed", func() bool { return s.sectionStatus().State == hostapi.SectionEgressFailed })
		return s, f, fail
	}
	t.Run("failed, it names why and when it tries again", func(t *testing.T) {
		s, _, _ := failing(t)
		st := s.sectionStatus()
		if !strings.Contains(st.Failure, "rate limit") || st.RetryAt.Sub(st.FailedAt) != time.Minute || st.Revision != 1 || st.PendingRevision != 2 {
			t.Errorf("section status = %+v", st)
		}
		if p := s.runners.profileStatus(); p[0].Ready || p[0].Reason != hostapi.ReasonEgressChangeFailed || !strings.Contains(p[0].Detail, "rate limit") {
			t.Errorf("profiles = %+v", p)
		}
		if !s.net.section().Egress.Internet {
			t.Error("the failed rebuild left the new posture")
		}
	})
	t.Run("a PUT of the same section retries at once", func(t *testing.T) {
		s, _, fail := failing(t)
		fail.Store(false)
		deliver(t, s, sectionOf(3, closedHost, smallJSON))
		waitFor(t, "the retry applied", func() bool { return s.sectionStatus().State == hostapi.SectionRunning })
		if s.net.section().Egress.Internet || s.host().Egress.Internet || s.running.raw.Revision != 3 {
			t.Error("the retry did not apply the section")
		}
		if p := s.runners.profileStatus(); p[0].Reason == hostapi.ReasonEgressChangeFailed {
			t.Errorf("profiles = %+v, want the hold lifted", p)
		}
	})
	t.Run("on its schedule, ending with a boot's rules", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, f, fail := failing(t)
			start := time.Now()
			for range 2 {
				time.Sleep(retryAfter(pendingFails(s)) + time.Second)
				synctest.Wait()
			}
			if n := pendingFails(s); n != 3 {
				t.Fatalf("%d failures, want three", n)
			}
			if got := time.Since(start); got < 3*time.Minute {
				t.Errorf("two retries in %s, want after 1 and 2 minutes", got)
			}
			fail.Store(false)
			time.Sleep(retryAfter(3) + time.Second)
			synctest.Wait()
			if st := s.sectionStatus(); st.State != hostapi.SectionRunning {
				t.Fatalf("section status = %+v", st)
			}
			fresh := newUpFixture(t)
			upNet(t, fresh, s.running.host)
			if got, want := kernelState(t, f, true), kernelState(t, fresh, true); got != want {
				t.Errorf("after the retry:\n%s\nwant a fresh boot's:\n%s", got, want)
			}
		})
	})
}

// The rows, the strictest a change touches winning.
func TestRowOf(t *testing.T) {
	v6 := func(block6 ...string) *config.Host {
		h := v6Section()
		for _, b := range block6 {
			h.Egress.Block6 = append(h.Egress.Block6, pfx(b))
		}
		return h
	}
	for name, tc := range map[string]struct {
		run, next *config.Host
		want      row
	}{
		"nothing the rules read": {section(), func() *config.Host { h := section(); h.ImageRefresh = time.Hour; return h }(), rowAtOnce},
		"allow":                  {section(), section("10.0.0.50/32"), rowSwap},
		"block6":                 {v6(), v6("fd00:1::/48"), rowSwap},
		"internet":               {section(), closedSection(), rowPosture},
		"open":                   {section(), openSection(), rowPosture},
		"ipv6":                   {section(), v6Section(), rowRestart},
		"dns":                    {section(), func() *config.Host { h := section(); h.DNS = config.DefaultDNS[:1]; return h }(), rowRestart},
		"subnet":                 {section(), func() *config.Host { h := section(); h.Subnet = pfx("10.250.1.0/24"); return h }(), rowRestart},
		"allow and internet":     {section(), func() *config.Host { h := section("10.0.0.50/32"); h.Egress.Internet = false; return h }(), rowPosture},
		"internet and dns":       {section(), func() *config.Host { h := closedSection(); h.DNS = config.DefaultDNS[:1]; return h }(), rowRestart},
	} {
		t.Run(name, func(t *testing.T) {
			if got := rowOf(tc.run, tc.next); got != tc.want {
				t.Errorf("row = %d, want %d", got, tc.want)
			}
		})
	}
}
