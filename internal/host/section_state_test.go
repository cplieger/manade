package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
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
	inner := writeState
	writeState = func(ctx context.Context, path string, st *hostState) error {
		if err := inner(ctx, path, st); err != nil {
			return err
		}
		return errNotDurable
	}
	t.Cleanup(func() { writeState = inner })
	if ans := put(t, s, sectionOf(2, lanHost, smallJSON, largeJSON)); ans.Applies != hostapi.SectionApplied {
		t.Fatalf("PUT = %+v", ans)
	}
	if saved, ok, err := readSection(t.Context(), s.sectionPath); !ok || err != nil || saved.Revision != 2 {
		t.Errorf("saved %+v, %v, %v; want revision 2", saved, ok, err)
	}
	retried(t, s)
	if n := s.sectionStatus().SaveFailures; n != 2 {
		t.Errorf("%d failed saves, want the delivery's and the retry's", n)
	}
	writeState = inner
	retried(t, s)
	retried(t, s)
	if n := s.sectionStatus().SaveFailures; n != 2 {
		t.Errorf("%d failed saves after a durable save, want 2", n)
	}
}

// No state the host fails to save makes it restart in a loop: three
// starts with every save failing each settle, up or waiting with the
// refusal, and once saves succeed at most one more restart comes.
func TestAStateThatIsNotSavedLoopsNoRestart(t *testing.T) {
	refused := &hostapi.SectionRefusal{Hash: "a1", Errors: []hostapi.FieldError{{Path: "hosts.lan.dns", Message: "no answer"}}}
	// lookups fail the dns section's boot: 1 the first lookup of a start,
	// 2 every lookup; the last start's lookups all answer.
	for _, tc := range []struct {
		name    string
		state   *hostState
		lookups int
		up      uint64
		ipv6    bool
	}{
		{
			"a first section whose boot fails a step",
			&hostState{Section: sectionOf(1, dnsHost, smallJSON), Unbooted: true}, 2, 0, false,
		},
		{
			"a restart that lost IPv6 its fallback needs",
			&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, ipv6Host, smallJSON), Unbooted: true}, 1, 0, true,
		},
		{
			"a restart whose fallback boots",
			&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, lanHost, smallJSON), Unbooted: true}, 1, 1, false,
		},
		{
			"a section that comes up",
			&hostState{Section: sectionOf(2, dnsHost, smallJSON), Fallback: sectionOf(1, lanHost, smallJSON), Unbooted: true}, 0, 2, false,
		},
		{
			"a carried refusal",
			&hostState{Section: sectionOf(1, lanHost, smallJSON), Refused: refused}, 0, 1, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starts := 0
			start := func(prev *supervisor) *supervisor {
				starts++
				f := newUpFixture(t)
				if tc.ipv6 {
					withIPv6(t, f)
				}
				p := rebootable(f)
				if tc.lookups > 0 && starts < 5 {
					failingFirst(p, tc.lookups == 2)
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
			s = start(s)
			s.bootSaved(t.Context())
			if _, err := settle(t, s); err != nil {
				t.Errorf("the second start with saves working: boot = %v, want it settled", err)
			}
		})
	}
}
