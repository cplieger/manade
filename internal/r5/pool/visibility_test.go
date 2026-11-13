package pool

import (
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/r5/github"
)

// skipRecheck makes the next proofs call skip the pass over every scope.
func (fx *fixture) skipRecheck() {
	fx.p.mu.Lock()
	fx.p.rechecked = fx.at
	fx.p.mu.Unlock()
}

func (fx *fixture) reads() []string {
	fx.gh.mu.Lock()
	defer fx.gh.mu.Unlock()
	out := fx.gh.reads
	fx.gh.reads = nil
	return out
}

// idlePoll is a scale set polling idle from phase on.
type idlePoll struct {
	ss    github.ScaleSet
	phase time.Duration
}

// pollIdle runs 50-second idle polls of each scale set for span, the time
// GitHub holds an idle poll, with runProofs' tick between. It returns each
// poll's figure per scale set, and how long after the start each
// visibility read of scope began.
func (fx *fixture) pollIdle(t *testing.T, span time.Duration, scope string, polls ...idlePoll) (map[github.ScaleSet][]int, []time.Duration) {
	t.Helper()
	const poll = 50 * time.Second
	base := fx.at
	figures := map[github.ScaleSet][]int{}
	begun := map[github.ScaleSet]time.Time{}
	var reads []time.Duration
	for at := time.Duration(0); at < span; at += proofTick {
		fx.at = base.Add(at)
		for _, ip := range polls {
			if at < ip.phase || (at-ip.phase)%poll != 0 {
				continue
			}
			if start, ok := begun[ip.ss]; ok {
				fx.polled(ip.ss, start, nil)
			}
			begun[ip.ss] = fx.at
			figures[ip.ss] = append(figures[ip.ss], fx.capacity(ip.ss))
		}
		fx.p.proofs(t.Context())
		for _, key := range fx.reads() {
			if key == scope {
				reads = append(reads, at)
			}
		}
	}
	return figures, reads
}

// A scope costs about one visibility read a minute: the refresh clock
// is the scope's, so four scale sets polling above their assignments on
// different phases read it no more often than one would, and every poll
// still finds the proof fresh.
func TestAScopeIsReadOnItsOwnClockWhateverItsScaleSets(t *testing.T) {
	cfg := load(t, `capacity: {cpus: 16, memory: 64GiB}
scopes:
  - repo: acme/app
profiles:
  p1: {cpus: 2, memory: 4GiB}
  p2: {cpus: 2, memory: 4GiB}
  p3: {cpus: 2, memory: 4GiB}
  p4: {cpus: 2, memory: 4GiB}
`)
	var polls []idlePoll
	for i, phase := range []time.Duration{0, 10 * time.Second, 25 * time.Second, 40 * time.Second} {
		polls = append(polls, idlePoll{ss: scaleSet("acme/app", "p"+strconv.Itoa(i+1), i+1), phase: phase})
	}
	fx := newFixtureFrom(t, cfg, polls[0].ss, polls[1].ss, polls[2].ss, polls[3].ss)
	fx.skipRecheck()
	figures, reads := fx.pollIdle(t, 10*time.Minute, "acme/app", polls...)
	for _, ip := range polls {
		if got := figures[ip.ss]; slices.Contains(got, 0) || len(got) < 11 {
			t.Errorf("%s figures = %v, want the standing slot on every poll", ip.ss.Profile, got)
		}
	}
	if len(reads) > 13 {
		t.Errorf("acme/app visibility reads = %d over 10 minutes, want at most 13", len(reads))
	}
	for i := 1; i < len(reads); i++ {
		if gap := reads[i] - reads[i-1]; gap < proofAge-refreshMargin {
			t.Errorf("reads at %v and %v are %v apart, want at least %v", reads[i-1], reads[i], gap, proofAge-refreshMargin)
		}
	}
}

func TestAStaleProofIsReadBeforeFunding(t *testing.T) {
	fx := newFixture(t)
	fx.skipRecheck()
	fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 1, 0)})
	fx.advance(proofAge)
	if got := fx.capacity(appDefault); got != 0 {
		t.Fatalf("capacity = %d on a stale proof, want 0", got)
	}
	fx.p.proofs(t.Context())
	if got := fx.reads(); !slices.Equal(got, []string{"acme/app"}) {
		t.Errorf("reads = %v, want only the scope the poll asked for", got)
	}
	// The budget left after the job also covers a standing slot.
	if got := fx.capacity(appDefault); got != 2 {
		t.Errorf("capacity = %d after the read, want the funded job and a standing slot", got)
	}
}

func TestAFailedVisibilityReadIsRetriedAMinuteLater(t *testing.T) {
	fx := newFixture(t)
	fx.skipRecheck()
	fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 1, 2, 0)})
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.advance(proofAge)
	fx.capacity(appDefault)
	fx.gh.private = map[string]error{"acme/app": errFake}
	fx.p.proofs(t.Context())
	if got := fx.capacity(appDefault); got != 1 {
		t.Errorf("capacity = %d after a failed read, want only the committed slot", got)
	}
	if !strings.Contains(scrape(t, fx.m), `manade_scope_serving{scope="acme/app"} 0`) {
		t.Error("acme/app reads as serving after a failed read")
	}
	fx.p.mu.Lock()
	open := fx.p.set(appDefault).open
	fx.p.mu.Unlock()
	if !open {
		t.Error("a failed read closed the session")
	}
	fx.reads()
	fx.advance(proofRetry - time.Second)
	fx.p.proofs(t.Context())
	if got := fx.reads(); len(got) != 0 {
		t.Errorf("reads = %v within a minute of the failure, want none", got)
	}
	fx.advance(time.Second)
	fx.p.proofs(t.Context())
	if got := fx.reads(); !slices.Equal(got, []string{"acme/app"}) {
		t.Errorf("reads = %v a minute after the failure, want the retry", got)
	}
	fx.gh.private = nil
	fx.advance(proofRetry)
	fx.p.proofs(t.Context())
	if got := fx.reads(); !slices.Equal(got, []string{"acme/app"}) {
		t.Errorf("reads = %v a minute after the second failure, want the retry", got)
	}
	if !strings.Contains(scrape(t, fx.m), `manade_scope_serving{scope="acme/app"} 1`) {
		t.Error("acme/app does not serve again after a good read")
	}
	if got := fx.capacity(appDefault); got != 3 {
		t.Errorf("capacity = %d after the good read, want the slot, the funded job and a standing slot", got)
	}
	if fx.rec.CountLevel(slog.LevelWarn, "Reading a scope's visibility failed") != 1 || fx.rec.CountLevel(slog.LevelInfo, "visibility read succeeded again") != 1 {
		t.Errorf("logs = %v, want one WARN at the failure and one INFO at the recovery", fx.rec.Messages())
	}
}

func TestAScopeTurnedPublicStopsServing(t *testing.T) {
	fx := newFixture(t)
	fx.skipRecheck()
	name, _ := fx.startOne(t, appDefault)
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1, Running: 1}, github.Event{Kind: github.Started, Runner: name, RunID: 1, Name: "job"})
	idleName, _ := fx.startOne(t, appDefault)
	fx.gh.public = map[string]bool{"acme/app": true}
	fx.advance(proofAge)
	fx.capacity(appDefault)
	fx.sweep(map[string][]github.QueuedJob{"acme/app": jobs("default", 2, 1, 0)})
	fx.capacity(appDefault)
	fx.p.proofs(t.Context())
	select {
	case <-fx.p.died:
	default:
		t.Error("the public verdict woke no reconciler pass to remove its idle runners")
	}
	if got := fx.capacity(appDefault); got != 0 {
		t.Errorf("capacity = %d once proven public, want 0", got)
	}
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 3, Running: 1}, assigned(3, "late"))
	if got := fx.startAll(t); got != 0 {
		t.Errorf("started %d runners for a public scope, want none", got)
	}
	if fx.rec.CountLevel(slog.LevelError, "stops serving it") != 1 {
		t.Error("no ERROR line for the public scope")
	}
	if !strings.Contains(scrape(t, fx.m), `manade_scope_serving{scope="acme/app"} 0`) {
		t.Error("a public scope reads as serving")
	}
	fx.reconcile(t)
	if fx.dk.box(idleName) != nil {
		t.Error("the public scope's idle runner is kept")
	}
	if fx.dk.box(name) == nil {
		t.Error("the public scope's busy runner was removed")
	}
	fx.reads()
	fx.advance(recheckEvery)
	fx.p.proofs(t.Context())
	if got := fx.reads(); slices.Contains(got, "acme/app") {
		t.Errorf("reads = %v, want a public scope never read again", got)
	}
}

// idleRunner starts an acme/app runner whose assignment GitHub then
// withdrew, so it is idle past what the assignments hold.
func (fx *fixture) idleRunner(t *testing.T) {
	t.Helper()
	fx.startOne(t, appDefault)
	fx.polled(appDefault, fx.at, &github.Stats{})
}

// A figure above what the assignments hold needs a proof under a
// minute old, an idle runner's slot included, and a failed read funds
// nothing above them.
func TestAnIdleRunnerNeedsAFreshProof(t *testing.T) {
	t.Run("aFreshProofReportsIt", func(t *testing.T) {
		fx := newFixture(t)
		fx.skipRecheck()
		fx.idleRunner(t)
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d, want the idle runner's slot", got)
		}
	})
	t.Run("itsScopeIsReadBeforeTheProofLapses", func(t *testing.T) {
		fx := newFixture(t)
		fx.skipRecheck()
		fx.idleRunner(t)
		fx.advance(30 * time.Second)
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d, want the idle runner's slot", got)
		}
		fx.p.proofs(t.Context())
		if got := fx.reads(); len(got) != 0 {
			t.Errorf("reads = %v on a 30-second proof, want none yet", got)
		}
		fx.advance(proofAge - refreshMargin - 30*time.Second)
		fx.p.proofs(t.Context())
		if got := fx.reads(); !slices.Equal(got, []string{"acme/app"}) {
			t.Errorf("reads = %v %v before the proof lapses, want acme/app", got, refreshMargin)
		}
	})
	t.Run("aStaleProofCapsIt", func(t *testing.T) {
		fx := newFixture(t)
		fx.skipRecheck()
		fx.idleRunner(t)
		fx.advance(proofAge)
		if got := fx.capacity(appDefault); got != 0 {
			t.Errorf("capacity = %d on a stale proof, want 0", got)
		}
		if !fx.p.scopes["acme/app"].wanted {
			t.Error("no visibility read was asked for")
		}
	})
	t.Run("aFailedReadCapsIt", func(t *testing.T) {
		fx := newFixture(t)
		fx.skipRecheck()
		fx.idleRunner(t)
		fx.p.mu.Lock()
		fx.p.prove(fx.p.scopes["acme/app"], false, errFake, fx.at)
		fx.p.mu.Unlock()
		if got := fx.capacity(appDefault); got != 0 {
			t.Errorf("capacity = %d after a failed read, want 0", got)
		}
		if !strings.Contains(scrape(t, fx.m), `manade_scope_serving{scope="acme/app"} 0`) {
			t.Error("acme/app reads as serving after a failed read")
		}
	})
	t.Run("aBusyRunnerKeepsItsSlot", func(t *testing.T) {
		fx := newFixture(t)
		fx.skipRecheck()
		name, _ := fx.startOne(t, appDefault)
		fx.take(appDefault, name)
		fx.advance(proofAge)
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d on a stale proof, want the slot GitHub counts", got)
		}
		fx.p.mu.Lock()
		fx.p.prove(fx.p.scopes["acme/app"], false, errFake, fx.at)
		fx.p.mu.Unlock()
		if got := fx.capacity(appDefault); got != 1 {
			t.Errorf("capacity = %d after a failed read, want the slot GitHub counts", got)
		}
	})
}

// A repo that turns public is served for at most 60 seconds plus one
// poll, so no poll beginning a minute after the last proof reports more
// than the assignments hold. Reads run only at poll begins here, so on the
// later phases Capacity's cap, not the refresh clock, holds the bound.
func TestAScopeTurnedPublicIsServedAtMostAMinutePlusAPoll(t *testing.T) {
	for _, tc := range []struct {
		setup func(*testing.T, *fixture)
		name  string
	}{
		{name: "standingSlot", setup: func(*testing.T, *fixture) {}},
		{name: "idleRunner", setup: func(t *testing.T, fx *fixture) { fx.idleRunner(t) }},
	} {
		for _, phase := range []time.Duration{0, 4 * time.Second, 15 * time.Second, 45 * time.Second} {
			t.Run(tc.name+"At"+phase.String(), func(t *testing.T) {
				fx := newFixture(t)
				fx.skipRecheck()
				tc.setup(t, fx)
				proven := fx.at
				fx.gh.public = map[string]bool{"acme/app": true}
				fx.advance(phase)
				for range 4 {
					start := fx.at
					got := fx.capacity(appDefault)
					if start.Sub(proven) >= proofAge && got > 0 {
						t.Errorf("a poll %v after the last proof reports %d, want 0", start.Sub(proven), got)
					}
					fx.p.proofs(t.Context())
					fx.advance(50 * time.Second)
					fx.polled(appDefault, start, nil)
				}
			})
		}
	}
}

func TestEveryTenMinutesEveryScopeIsReadAndRechecked(t *testing.T) {
	fx := newFixture(t)
	fx.p.proofs(t.Context())
	if got := fx.reads(); !slices.Equal(got, []string{"acme/app", "acme/lib", "acme"}) {
		t.Errorf("reads = %v, want every scope in the first pass", got)
	}
	fx.advance(recheckEvery - time.Second)
	fx.p.proofs(t.Context())
	if got := fx.reads(); len(got) != 0 || fx.gh.rechecks != 1 {
		t.Errorf("reads = %v and rechecks = %d before ten minutes, want none and 1", got, fx.gh.rechecks)
	}
	fx.gh.recheckErr = errFake
	fx.advance(time.Second)
	fx.p.proofs(t.Context())
	if got := fx.reads(); len(got) != 3 || fx.gh.rechecks != 2 {
		t.Errorf("reads = %v and rechecks = %d at ten minutes, want every scope and 2", got, fx.gh.rechecks)
	}
	if fx.rec.CountLevel(slog.LevelWarn, "Resolving the scopes' installations failed") != 1 {
		t.Error("a failed recheck logged no WARN")
	}
}
