package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/manade/internal/hostapi"
)

// The steps of a save that fail with the file left as it was.
var savePhases = []struct {
	name  string
	phase filesafe.WritePhase
}{
	{"the temporary file's sync", filesafe.PhaseTempSync},
	{"the rename", filesafe.PhaseRename},
}

// fileOf is the file at path, nil when there is none.
func fileOf(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// retried runs one of s's keepSaving retries.
func retried(t *testing.T, s *supervisor) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			s.keepSaving(ctx)
			close(done)
		}()
		time.Sleep(saveRetry + time.Second)
		synctest.Wait()
		cancel()
		<-done
	})
}

// settle boots s until it is up, or waits with a refusal, and answers the
// boot: nil and nil for a host left waiting.
func settle(t *testing.T, s *supervisor) (*hostNet, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan booted, 1)
	go func() {
		n, err := s.boot(ctx)
		done <- booted{n, err}
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case b := <-done:
			if b.n != nil {
				t.Cleanup(func() { _ = b.n.stop() })
			}
			return b.n, b.err
		case <-deadline:
			t.Fatal("the boot neither came up nor waited")
		case <-time.After(5 * time.Millisecond):
		}
		if st := s.sectionStatus(); st.State == hostapi.SectionWaiting && st.Refused != nil {
			cancel()
		}
	}
}

// A delivery whose save fails is not taken: it answers the failure, the
// file and the host stay as they were, the failure is counted, and the
// same delivery is taken once a save succeeds.
func TestADeliveryWhoseSaveFailsIsNotTaken(t *testing.T) {
	for _, ph := range savePhases {
		t.Run(ph.name, func(t *testing.T) {
			t.Run("a first section", func(t *testing.T) {
				s := waitingOn(t, lanSelf(), newUpFixture(t).platform())
				restore := failSaves(t, ph.phase)
				if _, err := apiOf(s).PutSection(t.Context(), sectionOf(1, lanHost, smallJSON)); err == nil {
					t.Fatal("PUT succeeded with its save failing")
				}
				if b := fileOf(t, s.sectionPath); b != nil {
					t.Errorf("saved %s", b)
				}
				if st := statusOf(t, s).Section; st.State != hostapi.SectionWaiting || st.SaveFailures != 1 {
					t.Errorf("section status = %+v, want waiting with one failed save", st)
				}
				restore()
				if retried(t, s); fileOf(t, s.sectionPath) != nil {
					t.Error("a retry saved the section the host did not take")
				}
				if ans := put(t, s, sectionOf(1, lanHost, smallJSON)); ans.Applies != hostapi.SectionApplied {
					t.Errorf("PUT = %+v", ans)
				}
				if st := stateOf(t, s); st.Section == nil || st.Section.Revision != 1 || !st.Unbooted {
					t.Errorf("state %+v, want revision 1 not booted", st)
				}
			})
			for _, tc := range []struct {
				name string
				next *hostapi.Section
			}{
				{"a restart", sectionOf(2, dnsHost, smallJSON)},
				{"a change applied at once", sectionOf(2, lanHost, smallJSON, largeJSON)},
				{"a new revision of the running settings", sectionOf(2, lanHost, smallJSON)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
					before := fileOf(t, s.sectionPath)
					restore := failSaves(t, ph.phase)
					if _, err := apiOf(s).PutSection(t.Context(), tc.next); err == nil {
						t.Fatal("PUT succeeded with its save failing")
					}
					if b := fileOf(t, s.sectionPath); !bytes.Equal(b, before) {
						t.Errorf("saved %s, want %s", b, before)
					}
					if st := statusOf(t, s).Section; st.Revision != 1 || st.PendingHash != "" || st.SaveFailures != 1 {
						t.Errorf("section status = %+v, want revision 1, nothing pending, one failed save", st)
					}
					restore()
					if retried(t, s); !bytes.Equal(fileOf(t, s.sectionPath), before) {
						t.Error("a retry saved the section the host did not take")
					}
					put(t, s, tc.next)
					if st := stateOf(t, s); st.Section == nil || st.Section.Revision != 2 {
						t.Errorf("state %+v, want revision 2", st)
					}
				})
			}
		})
	}
}

// A section not booted here that comes up while its save fails runs; the
// file keeps marking it not booted until a retry saves it, after which a
// step failure the host causes ends the next start's run, never a refusal.
func TestABootThatComesUpWhoseSaveFails(t *testing.T) {
	for _, ph := range savePhases {
		t.Run(ph.name, func(t *testing.T) {
			s := waitingOn(t, lanSelf(), rebootable(newUpFixture(t)))
			saveUnbooted(t, s, sectionOf(2, dnsHost, smallJSON), sectionOf(1, lanHost, smallJSON))
			before := fileOf(t, s.sectionPath)
			restore := failSaves(t, ph.phase)
			s.bootSaved(t.Context())
			bootUp(t, s)
			if s.running.raw.Revision != 2 {
				t.Errorf("booted revision %d, want 2", s.running.raw.Revision)
			}
			if b := fileOf(t, s.sectionPath); !bytes.Equal(b, before) {
				t.Errorf("saved %s, want %s", b, before)
			}
			if n := s.sectionStatus().SaveFailures; n != 1 {
				t.Errorf("%d failed saves, want 1", n)
			}
			restore()
			retried(t, s)
			if st := stateOf(t, s); st.Section == nil || st.Section.Revision != 2 || !noFallback(st) {
				t.Fatalf("after the retry, state %+v; want revision 2 booted", st)
			}
			p := rebootable(newUpFixture(t))
			failingFirst(p, true)
			r := restarted(s, p)
			r.bootSaved(t.Context())
			n, err := r.boot(t.Context())
			if n != nil || err == nil || errors.Is(err, ErrRestart) {
				t.Errorf("next start's boot = %v, %v; want the run ended by the lookup", n, err)
			}
			if st := r.sectionStatus(); st.Refused != nil {
				t.Errorf("refused %+v, want the host's own failure", st.Refused)
			}
			if saved, ok, err := readSection(t.Context(), r.sectionPath); !ok || err != nil || saved.Revision != 2 {
				t.Errorf("saved %+v, %v, %v; want revision 2 kept", saved, ok, err)
			}
		})
	}
}

// A boot's refusal whose save fails never restarts the host, since the
// next start would boot the refused section again: the host runs the
// fallback it holds or waits with the refusal, the file as it was.
func TestABootRefusalWhoseSaveFails(t *testing.T) {
	for _, ph := range savePhases {
		t.Run(ph.name, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				state    *hostState
				prepare  func(t *testing.T, f *upFixture, p *seams)
				fallback uint64
			}{
				{
					"a fallback in the same run",
					&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, lanHost, smallJSON), Unbooted: true},
					func(_ *testing.T, _ *upFixture, p *seams) { failingFirst(p, false) }, 1,
				},
				{
					"a first section, which restarts with no fallback",
					&hostState{Section: sectionOf(1, dnsHost, smallJSON), Unbooted: true},
					func(_ *testing.T, _ *upFixture, p *seams) { failingFirst(p, false) }, 0,
				},
				{
					"a boot that lost IPv6 its fallback needs",
					&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, ipv6Host, smallJSON), Unbooted: true},
					func(t *testing.T, f *upFixture, p *seams) {
						withIPv6(t, f)
						failingFirst(p, false)
					}, 0,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newUpFixture(t)
					p := rebootable(f)
					tc.prepare(t, f, p)
					s := waitingOn(t, lanSelf(), p)
					if err := writeState(t.Context(), s.sectionPath, tc.state); err != nil {
						t.Fatal(err)
					}
					before := fileOf(t, s.sectionPath)
					restore := failSaves(t, ph.phase)
					s.bootSaved(t.Context())
					n, err := settle(t, s)
					if err != nil {
						t.Fatalf("boot = %v, want no restart and no failure", err)
					}
					st := s.sectionStatus()
					switch {
					case tc.fallback != 0:
						if n == nil || s.running.raw.Revision != tc.fallback {
							t.Errorf("up %v, want the host up on revision %d", n != nil, tc.fallback)
						}
					case n != nil || st.State != hostapi.SectionWaiting:
						t.Errorf("up %v, section status %+v; want the host waiting", n != nil, st)
					}
					if st.Refused == nil || st.Refused.Hash != hashOf(t, tc.state.Section) || st.Refused.Errors[0].Path != "hosts.lan.dns" {
						t.Errorf("refused %+v, want the section at hosts.lan.dns", st.Refused)
					}
					if st.SaveFailures != 1 {
						t.Errorf("%d failed saves, want 1", st.SaveFailures)
					}
					if b := fileOf(t, s.sectionPath); !bytes.Equal(b, before) {
						t.Errorf("saved %s, want %s", b, before)
					}
					restore()
					if tc.fallback != 0 {
						return
					}
					if _, err := apiOf(s).PutSection(t.Context(), sectionOf(3, dnsHost, smallJSON)); !slices.Equal(refusedAt(t, err), []string{"hosts.lan.dns"}) {
						t.Errorf("the refused settings delivered again = %v", err)
					}
					if ans := put(t, s, sectionOf(4, lanHost, smallJSON)); ans.Applies != hostapi.SectionApplied {
						t.Errorf("a corrected PUT = %+v", ans)
					}
				})
			}
		})
	}
}

// A refusal carried across a restart holds for that start even when the
// host cannot take it out of the file, and a retry takes it out.
func TestACarriedRefusalWhoseClearFails(t *testing.T) {
	for _, ph := range savePhases {
		t.Run(ph.name, func(t *testing.T) {
			s := waitingOn(t, lanSelf(), rebootable(newUpFixture(t)))
			refused := &hostapi.SectionRefusal{
				At: time.Now(), Hash: hashOf(t, sectionOf(2, dnsHost, smallJSON)),
				Errors: []hostapi.FieldError{{Path: "hosts.lan.dns", Message: "no answer"}},
			}
			if err := writeState(t.Context(), s.sectionPath, &hostState{Section: sectionOf(1, lanHost, smallJSON), Refused: refused}); err != nil {
				t.Fatal(err)
			}
			before := fileOf(t, s.sectionPath)
			restore := failSaves(t, ph.phase)
			s.bootSaved(t.Context())
			bootUp(t, s)
			finishUp(t, s)
			if st := s.sectionStatus(); st.Refused == nil || st.Refused.Hash != refused.Hash || st.SaveFailures != 1 {
				t.Errorf("section status = %+v, want the carried refusal and one failed save", st)
			}
			if b := fileOf(t, s.sectionPath); !bytes.Equal(b, before) {
				t.Errorf("saved %s, want %s", b, before)
			}
			if _, err := apiOf(s).PutSection(t.Context(), sectionOf(3, dnsHost, smallJSON)); !slices.Equal(refusedAt(t, err), []string{"hosts.lan.dns"}) {
				t.Errorf("the refused settings delivered again = %v", err)
			}
			restore()
			retried(t, s)
			if st := stateOf(t, s); st.Refused != nil || st.Section == nil || st.Section.Revision != 1 {
				t.Errorf("after the retry, state %+v; want revision 1 and no refusal", st)
			}
		})
	}
}

// A save whose directory sync fails has its state at its path: the
// delivery is taken, the failure counted, and the save tried again until
// one is durable.
func TestASaveThatIsNotDurableIsTriedAgain(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	inner := writeFile
	writeFile = func(ctx context.Context, path string, b []byte, opts ...filesafe.WriteOption) (filesafe.Result, error) {
		res, err := inner(ctx, path, b, opts...)
		res.Durable = false
		return res, err
	}
	t.Cleanup(func() { writeFile = inner })
	if ans := put(t, s, sectionOf(2, lanHost, smallJSON, largeJSON)); ans.Applies != hostapi.SectionApplied {
		t.Fatalf("PUT = %+v", ans)
	}
	if saved, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil || saved.Revision != 2 {
		t.Errorf("saved %+v, %v, %v; want revision 2", saved, ok, err)
	}
	if n := s.sectionStatus().SaveFailures; n != 1 {
		t.Errorf("%d failed saves, want the delivery's", n)
	}
	retried(t, s)
	if n := s.sectionStatus().SaveFailures; n != 2 {
		t.Errorf("%d failed saves, want the delivery's and the retry's", n)
	}
	writeFile = inner
	retried(t, s)
	retried(t, s)
	if n := s.sectionStatus().SaveFailures; n != 2 {
		t.Errorf("%d failed saves after a durable save, want 2", n)
	}
}

// A restart a boot refusal owes, held while its state failed to save, ends
// the run once a retry saves that state, and the next start reads the
// refusal. A corrected section delivered first is booted instead, and a
// later save of its state ends nothing.
func TestARestartOwedIsTakenOnceItsStateIsSaved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    *hostState
		fallback uint64
		ipv6     bool
	}{
		{"a first section", &hostState{Section: sectionOf(1, dnsHost, smallJSON), Unbooted: true}, 0, false},
		{
			"a boot that lost IPv6 its fallback needs",
			&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, ipv6Host, smallJSON), Unbooted: true}, 1, true,
		},
	} {
		for _, then := range []string{"a retry", "a corrected delivery"} {
			t.Run(tc.name+" then "+then, func(t *testing.T) {
				f := newUpFixture(t)
				if tc.ipv6 {
					withIPv6(t, f)
				}
				p := rebootable(f)
				failingFirst(p, false)
				s := waitingOn(t, lanSelf(), p)
				if err := writeState(t.Context(), s.sectionPath, tc.state); err != nil {
					t.Fatal(err)
				}
				restore := failSaves(t, filesafe.PhaseRename)
				s.bootSaved(t.Context())
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				s.cancelRun = cancel
				done := make(chan booted, 1)
				go func() {
					n, err := s.boot(ctx)
					done <- booted{n, err}
				}()
				waitFor(t, "the boot's refusal", func() bool {
					st := s.sectionStatus()
					return st.State == hostapi.SectionWaiting && st.Refused != nil
				})
				restore()
				if then == "a corrected delivery" {
					restoreBooted := failBootedSaves(t)
					put(t, s, sectionOf(3, lanHost, smallJSON))
					b := <-done
					if b.err != nil || b.n == nil {
						t.Fatalf("boot = %v, %v; want the corrected section up", b.n, b.err)
					}
					t.Cleanup(func() { _ = b.n.stop() })
					restoreBooted()
					retried(t, s)
					if st := stateOf(t, s); st.Section == nil || st.Section.Revision != 3 || st.Unbooted {
						t.Errorf("saved %+v, want revision 3 booted", st)
					}
					if err := context.Cause(ctx); err != nil {
						t.Errorf("the run ended with %v once the booted section was saved", err)
					}
					return
				}
				retried(t, s)
				select {
				case b := <-done:
					if b.n != nil || b.err != nil || !errors.Is(context.Cause(ctx), ErrRestart) {
						t.Fatalf("boot = %v, %v with the run's cause %v; want the run ended by ErrRestart", b.n, b.err, context.Cause(ctx))
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the boot still waits after its state was saved")
				}
				st := stateOf(t, s)
				var boots uint64
				if st.Section != nil {
					boots = st.Section.Revision
				}
				if st.Refused == nil || st.Refused.Hash != hashOf(t, tc.state.Section) || boots != tc.fallback {
					t.Errorf("saved %+v, want the refusal booting revision %d", st, tc.fallback)
				}
				r := restarted(s, rebootable(newUpFixture(t)))
				r.bootSaved(t.Context())
				if st := r.sectionStatus(); st.Refused == nil || st.Refused.Hash != hashOf(t, tc.state.Section) {
					t.Errorf("next start's section status = %+v, want the refusal", st)
				}
			})
		}
	}
}

// failBootedSaves makes each save of a state that marks no section as not
// booted fail, until the test ends or it calls the restore answered.
func failBootedSaves(t *testing.T) (restore func()) {
	t.Helper()
	inner := writeFile
	writeFile = func(ctx context.Context, path string, b []byte, opts ...filesafe.WriteOption) (filesafe.Result, error) {
		if !bytes.Contains(b, []byte(`"unbooted":true`)) {
			return filesafe.Result{}, &filesafe.WriteError{Phase: filesafe.PhaseRename, Err: syscall.ENOSPC}
		}
		return inner(ctx, path, b, opts...)
	}
	restore = func() { writeFile = inner }
	t.Cleanup(restore)
	return restore
}

// No state the host fails to save makes it restart in a loop: three
// starts with every save failing each settle, up or waiting with the
// refusal, and once saves succeed at most one more restart comes.
func TestAStateThatIsNotSavedLoopsNoRestart(t *testing.T) {
	refused := &hostapi.SectionRefusal{Hash: "a1", Errors: []hostapi.FieldError{{Path: "hosts.lan.dns", Message: "no answer"}}}
	for _, tc := range []struct {
		name  string
		state *hostState
		fault lookupFault
		up    uint64
		ipv6  bool
	}{
		{
			"a first section whose boot fails a step",
			&hostState{Section: sectionOf(1, dnsHost, smallJSON), Unbooted: true}, everyLookup, 0, false,
		},
		{
			"a restart that lost IPv6 its fallback needs",
			&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, ipv6Host, smallJSON), Unbooted: true}, firstLookup, 0, true,
		},
		{
			"a restart whose fallback boots",
			&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, lanHost, smallJSON), Unbooted: true}, firstLookup, 1, false,
		},
		{
			"a section that comes up",
			&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, lanHost, smallJSON), Unbooted: true}, noFault, 2, false,
		},
		{
			"a carried refusal",
			&hostState{Section: sectionOf(1, lanHost, smallJSON), Refused: refused}, noFault, 1, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			healed := false
			start := func(prev *supervisor) *supervisor {
				f := newUpFixture(t)
				if tc.ipv6 {
					withIPv6(t, f)
				}
				p := rebootable(f)
				if tc.fault != noFault && !healed {
					failingFirst(p, tc.fault == everyLookup)
				}
				if prev == nil {
					return waitingOn(t, lanSelf(), p)
				}
				return restarted(prev, p)
			}
			s := start(nil)
			if err := writeState(t.Context(), s.sectionPath, tc.state); err != nil {
				t.Fatal(err)
			}
			restore := failSaves(t, filesafe.PhaseRename)
			for i := range 3 {
				if i > 0 {
					s = start(s)
				}
				s.bootSaved(t.Context())
				n, err := settle(t, s)
				if err != nil {
					t.Fatalf("start %d: boot = %v, want no restart and no failure", i+1, err)
				}
				switch st := s.sectionStatus(); {
				case tc.up != 0 && (n == nil || s.running.raw.Revision != tc.up):
					t.Errorf("start %d: up %v, want revision %d up", i+1, n != nil, tc.up)
				case tc.up == 0 && (n != nil || st.State != hostapi.SectionWaiting || st.Refused == nil):
					t.Errorf("start %d: up %v, section status %+v; want waiting with the refusal", i+1, n != nil, st)
				}
			}
			restore()
			s = start(s)
			s.bootSaved(t.Context())
			if _, err := settle(t, s); err != nil && !errors.Is(err, ErrRestart) {
				t.Fatalf("the first start with saves working: boot = %v", err)
			}
			healed = true
			s = start(s)
			s.bootSaved(t.Context())
			if _, err := settle(t, s); err != nil {
				t.Errorf("the second start with saves working: boot = %v, want it settled", err)
			}
		})
	}
}

// lookupFault is how a start's lookups through the host's own resolver
// fail a dns section's boot.
type lookupFault int

const (
	noFault lookupFault = iota
	firstLookup
	everyLookup
)

// A section of the largest size a delivery takes, dense in the characters
// JSON escapes for HTML, is saved and read back whole beside its fallback.
func TestAStateAtTheDeliveryCapIsReadBack(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	next := sectionOf(2, dnsHost, smallJSON, `"amp": {"runner_image": "base", "cpus": 1, "memory": "512MiB", "scopes": ["acme/app"], "command": ["/bin/sh", "-c", ""]}`)
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	fill := hostapi.SectionLimit - len(body) - 2
	next.Settings = bytes.Replace(next.Settings, []byte(`"-c", ""`), []byte(`"-c", "`+strings.Repeat("&", fill)+`"`), 1)
	if ans := put(t, s, next); ans.Applies != hostapi.SectionPendingRestart {
		t.Fatalf("PUT = %+v", ans)
	}
	st := stateOf(t, s)
	if st.Section == nil || hashOf(t, st.Section) != hashOf(t, next) || st.Fallback == nil || st.Fallback.Revision != 1 {
		t.Errorf("read back section %v and fallback %v, want revision 2 over revision 1", st.Section != nil, st.Fallback)
	}
}

// A state larger than a start reads is refused before anything is
// written, so the state saved before stays whole.
func TestAStateTooLargeToReadIsNotWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "section.json")
	small := &hostState{Section: sectionOf(1, lanHost, smallJSON)}
	if err := writeState(t.Context(), path, small); err != nil {
		t.Fatal(err)
	}
	huge := &hostapi.Section{Revision: 2, Settings: []byte(`"` + strings.Repeat("a", stateLimit) + `"`)}
	if err := writeState(t.Context(), path, &hostState{Section: huge}); !errors.Is(err, filesafe.ErrFileTooLarge) {
		t.Errorf("writing a state over the bound = %v, want filesafe.ErrFileTooLarge", err)
	}
	if st, ok, err := readState(t.Context(), path); !ok || err != nil || st.Section == nil || st.Section.Revision != 1 {
		t.Errorf("read %+v, %v, %v; want revision 1", st, ok, err)
	}
}

// A state file holding anything but one state is an unreadable state,
// logged as one, and the host waits for the control plane's section.
func TestAStateFileOfAnotherShapeIsUnreadable(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"a section of another shape", `{"settings":{},"revision":1}`},
		{"two states", `{}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			s := newSupervisor(lanSelf(), &Options{Log: slog.New(slog.NewTextHandler(&logged, nil))}, filepath.Join(t.TempDir(), "section.json"), rebootable(newUpFixture(t)))
			if err := os.WriteFile(s.sectionPath, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			s.bootSaved(t.Context())
			if !strings.Contains(logged.String(), `level=ERROR msg="The host's state could not be read`) {
				t.Errorf("logged %q, want the state reported unreadable", logged.String())
			}
			if st := s.sectionStatus(); st.State != hostapi.SectionWaiting {
				t.Errorf("section status = %+v, want waiting", st)
			}
		})
	}
}
