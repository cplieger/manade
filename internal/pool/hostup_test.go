package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

// A connected host that is not up answers its status and refuses every
// other call (§6.4, §7.1): the pool records that status, reads the host as
// waiting for its section, and counts no link failure, warns of nothing and
// probes nothing until the host is up.
func TestAHostThatIsNotUpWaitsForItsSection(t *testing.T) {
	t.Parallel()
	for _, notUp := range []error{hostapi.ErrWaitingForSection, hostapi.ErrStarting} {
		t.Run(notUp.Error(), func(t *testing.T) {
			t.Parallel()
			r := newRig(t, oneHost())
			r.pass()
			r.hosts[0].set(func(f *fakeHost) { f.notUp, f.probes = notUp, 0 })
			r.pass()
			wantWaiting(t, r)
			r.restart()
			r.pass()
			wantWaiting(t, r)
			r.hosts[0].set(func(f *fakeHost) { f.notUp = nil })
			r.pass()
			if !r.serving("default") {
				t.Errorf("once up, the host does not serve; view = %+v", r.view("default"))
			}
		})
	}
}

// wantWaiting checks r's host default as one waiting for its section.
func wantWaiting(t *testing.T, r *rig) {
	t.Helper()
	v := r.view("default")
	if v.Status == nil || v.Status.Section.State != hostapi.SectionWaiting || v.Serving {
		t.Errorf("view = %+v, want its waiting status and not serving", v)
	}
	got := r.hostLog.statesOf("default")
	if want := "settings_not_applied/" + string(hostapi.SectionWaiting); len(got) == 0 || got[len(got)-1] != want {
		t.Errorf("states = %v, want the last %q", got, want)
	}
	if n := r.m.linkErrorsOf("default"); n != 0 {
		t.Errorf("link errors = %d, want 0", n)
	}
	if r.hosts[0].probes != 0 {
		t.Errorf("probes = %d, want none of a host that is not up", r.hosts[0].probes)
	}
	if log := r.log.String(); strings.Contains(log, "level=WARN") || strings.Contains(log, "level=ERROR") {
		t.Errorf("log:\n%s\nwant no warning", log)
	}
}

// A host that is not up refuses its events stream as it refuses every
// other call, which is no link failure.
func TestTheEventsOfAHostThatIsNotUp(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) { f.notUp = hostapi.ErrWaitingForSection })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.p.watchEvents(ctx, 0)
		close(done)
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		r.hosts[0].mu.Lock()
		n := r.hosts[0].streams
		r.hosts[0].mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the events stream was never asked for")
		}
	}
	cancel()
	<-done
	if n := r.m.linkErrorsOf("default"); n != 0 {
		t.Errorf("link errors = %d, want 0", n)
	}
	if log := r.log.String(); strings.Contains(log, "events stream failed") {
		t.Errorf("log:\n%s\nwant no warning", log)
	}
}

// A host that went down to boot between its pass and a start created
// nothing, so the start frees its slot at once as a refusal does.
func TestAPOSTToAHostThatIsNotUpFreesItsSlotAtOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) { f.notUp = hostapi.ErrStarting })
	r.assign(r.small(), 1, 0)
	if n := r.runners(r.small()); n != 0 {
		t.Errorf("records after a 503 starting = %d, want 0", n)
	}
	if !r.gh.wasRemoved(1) {
		t.Error("the registration of a start the host did not take stays")
	}
	if r.m.starts["false"] != 1 {
		t.Errorf("failed starts = %d, want 1", r.m.starts["false"])
	}
}

// A host the first pass finds not yet connected is the start of a run,
// not a fault, so only a host lost after it answered, or one whose first
// answer failed, is warned of.
func TestOnlyALostHostIsWarnedOf(t *testing.T) {
	t.Parallel()
	r := newRig(t, oneHost())
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = true })
	r.restart()
	r.pass()
	const line = "A host does not answer"
	if log := r.log.String(); strings.Contains(log, line) {
		t.Errorf("log after a pass before the host connected:\n%s", log)
	}
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = false })
	r.pass()
	r.hosts[0].set(func(f *fakeHost) { f.disconnected = true })
	r.pass()
	r.pass()
	if n := strings.Count(r.log.String(), line); n != 1 {
		t.Errorf("warnings of the lost host = %d, want 1; log:\n%s", n, r.log.String())
	}
	r.hosts[0].set(func(f *fakeHost) { f.disconnected, f.down = false, true })
	r.restart()
	r.pass()
	if n := strings.Count(r.log.String(), line); n != 2 {
		t.Errorf("warnings after a first answer failed = %d, want 2; log:\n%s", n, r.log.String())
	}
}
