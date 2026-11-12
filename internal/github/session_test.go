package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/manade/internal/obs"
)

const sessionLayout = `scopes:
  - repo: acme/app
profiles:
  small: {}
`

var appSmallSet = ScaleSet{obs.ScaleSet{Scope: "acme/app", Profile: "small"}, 1}

// listenFixture is one scale set on the broker, served by Listen until the
// handler's stop runs.
type listenFixture struct {
	b     *broker
	s     *ScaleSets
	m     *obs.Metrics
	rec   *capture.Recorder
	c     *clock
	waits *[]time.Duration
	h     *handler
}

func newListen(t *testing.T) *listenFixture {
	t.Helper()
	f := newFake(t)
	cfg := load(t, false, sessionLayout)
	b := newBroker(t, f, "acme/app")
	b.set(1, fakeSet{name: "small", labels: []string{"small"}, group: 1, disableUpdate: true})
	c := &clock{t: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)}
	waits := clockSleep(t, c)
	s, _, m, rec := newTestScaleSets(t, f, cfg, c)
	return &listenFixture{b: b, s: s, m: m, rec: rec, c: c, waits: waits, h: &handler{capacity: 1}}
}

// run serves the scale set until the n-th answered poll, where at runs
// after each one.
func (lf *listenFixture) run(t *testing.T, n int, at func(n int)) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lf.h.after = func(i int) {
		if at != nil {
			at(i)
		}
		if i == n {
			cancel()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lf.s.Listen(ctx, appSmallSet, lf.h)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Listen did not return")
	}
}

func TestListenPollsAndAcks(t *testing.T) {
	lf := newListen(t)
	lf.h.capacity = 7
	lf.b.initial = Stats{Assigned: 3, Running: 1}
	batch := make([]string, 50)
	for i := range batch {
		batch[i] = job("JobAssigned", int64(1000+i), "build "+strconv.Itoa(i), "", "")
	}
	lf.b.push(1, Stats{Assigned: 50, Running: 1}, batch...)
	lf.b.push(1, Stats{Assigned: 49, Running: 2},
		job("JobStarted", 1000, "build 0", "manade-small-1", ""),
		job("JobCompleted", 1001, "build 1", "manade-small-0", "succeeded"),
		`{"messageType":"JobCompleted","workflowRunId":1002,"jobDisplayName":"build 2","result":"canceled"}`)
	var acksAtFirst int
	lf.run(t, 3, func(n int) {
		if n == 1 {
			lf.b.do(func(b *broker) { acksAtFirst = len(b.acks) })
		}
	})
	h, b := lf.h, lf.b

	t.Run("openedBeforeTheFirstPoll", func(t *testing.T) {
		if len(h.opened) != 1 || h.opened[0] != (Stats{Assigned: 3, Running: 1}) {
			t.Errorf("opened = %+v, want the session's initial statistics once", h.opened)
		}
		if len(h.log) < 2 || h.log[0] != "opened" || h.log[1] != "capacity" {
			t.Errorf("handler calls = %v, want opened before the first capacity", h.log)
		}
	})
	t.Run("batchOfFifty", func(t *testing.T) {
		p := h.polls[0]
		if p.Idle || p.Stats == nil || *p.Stats != (Stats{Assigned: 50, Running: 1}) || len(p.Events) != 50 {
			t.Fatalf("first poll = idle %v, stats %+v, %d events; want the batch of 50 with its statistics", p.Idle, p.Stats, len(p.Events))
		}
		e := p.Events[49]
		if e.Kind != Assigned || e.RunID != 1049 || e.Name != "build 49" || !e.Queued.Equal(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) {
			t.Errorf("last event = %+v, want build 49 of run 1049 assigned", e)
		}
	})
	t.Run("startedAndCompleted", func(t *testing.T) {
		ev := h.polls[1].Events
		if len(ev) != 3 {
			t.Fatalf("second poll events = %+v, want 3", ev)
		}
		if ev[0].Kind != Started || ev[0].Runner != "manade-small-1" || ev[0].RunID != 1000 {
			t.Errorf("started = %+v", ev[0])
		}
		if ev[1].Kind != Completed || ev[1].Result != "succeeded" || ev[1].Runner != "manade-small-0" ||
			!ev[1].Finished.Equal(time.Date(2026, 11, 10, 8, 5, 0, 0, time.UTC)) {
			t.Errorf("completed = %+v", ev[1])
		}
		// A reassignment cancel names no runner and has no finish time.
		if ev[2].Kind != Completed || ev[2].Result != "canceled" || ev[2].Runner != "" || !ev[2].Finished.IsZero() {
			t.Errorf("reassignment cancel = %+v, want no runner and a zero finish time", ev[2])
		}
	})
	t.Run("idlePoll", func(t *testing.T) {
		if p := h.polls[2]; !p.Idle || p.Stats != nil || len(p.Events) != 0 {
			t.Errorf("third poll = %+v, want idle", p)
		}
	})
	t.Run("capacityAndAcks", func(t *testing.T) {
		b.do(func(b *broker) {
			want := []brokerPoll{{1, 7, 0}, {1, 7, 1}, {1, 7, 2}}
			if !slices.Equal(b.polls, want) {
				t.Errorf("polls = %v, want maxCapacity 7 and each acked message as lastMessageId", b.polls)
			}
			if !slices.Equal(b.acks, []int{1, 2}) {
				t.Errorf("acks = %v, want each message once", b.acks)
			}
			if b.closes != 1 {
				t.Errorf("closes = %d, want the shutdown's", b.closes)
			}
		})
		if h.closed != 1 {
			t.Errorf("Closed calls = %d, want the shutdown's", h.closed)
		}
		if acksAtFirst != 0 {
			t.Error("a message was acked before the pool had it")
		}
	})
	t.Run("pollRecorded", func(t *testing.T) {
		got := sample(t, lf.m, `manade_poll_last_success_timestamp_seconds{profile="small",scope="acme/app"}`)
		if want := float64(lf.c.now().Unix()); got != want {
			t.Errorf("poll timestamp = %v, want %v", got, want)
		}
		if h.beats != 7 {
			t.Errorf("beats = %d, want 7: the create, 3 polls, 2 acks and the close", h.beats)
		}
	})
}

// At boot a conflict is a crashed predecessor's claim: it is retried every
// 5 seconds, never stops serving, and raises one ERROR once it has lasted 2
// minutes.
func TestListenBootConflict(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conflicts int
		errors    int
	}{
		{name: "insideTwoMinutes", conflicts: 24},
		{name: "twoMinutes", conflicts: 25, errors: 1},
		{name: "longer", conflicts: 40, errors: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := newListen(t)
			lf.b.conflict = tc.conflicts
			lf.run(t, 1, nil)
			if len(lf.h.held) != 0 {
				t.Errorf("held = %v, want never at boot", lf.h.held)
			}
			if n := len(*lf.waits); n != tc.conflicts || slices.ContainsFunc(*lf.waits, func(d time.Duration) bool { return d != 5*time.Second }) {
				t.Errorf("waits = %v, want %d of 5 seconds", *lf.waits, tc.conflicts)
			}
			if n := lf.rec.CountLevel(slog.LevelError, "after 2 minutes"); n != tc.errors {
				t.Errorf("2-minute ERROR lines = %d, want %d", n, tc.errors)
			}
			if n := lf.rec.CountLevel(slog.LevelInfo, "opened after failing"); n != tc.errors {
				t.Errorf("end lines = %d, want one for each ERROR", n)
			}
			if v := sample(t, lf.m, `manade_github_errors_total{op="session"}`); v != float64(tc.conflicts) {
				t.Errorf("session errors = %v, want %d", v, tc.conflicts)
			}
		})
	}
}

// replaceAfterGap answers poll 1 with a 61-second gap before the next one,
// so the session is replaced, and sets what the replacement meets.
func replaceAfterGap(lf *listenFixture, closeStatus, conflicts int) func(int) {
	return func(n int) {
		if n == 1 {
			lf.b.do(func(b *broker) { b.closeSt, b.conflict = closeStatus, conflicts })
			lf.c.advance(maxGap + time.Second)
		}
	}
}

func TestListenReplacesAfterGap(t *testing.T) {
	t.Run("withinTheGap", func(t *testing.T) {
		lf := newListen(t)
		lf.run(t, 2, func(int) { lf.c.advance(maxGap) })
		lf.b.do(func(b *broker) {
			if b.creates != 1 {
				t.Errorf("creates = %d, want 1: a 60-second gap keeps the session", b.creates)
			}
		})
	})
	t.Run("expiredCloseCounts", func(t *testing.T) {
		lf := newListen(t)
		lf.run(t, 2, replaceAfterGap(lf, http.StatusBadRequest, 2))
		if !slices.Equal(lf.h.held, []bool{true, false}) {
			t.Errorf("held = %v: after a counted close, a conflict is another process until it clears", lf.h.held)
		}
		lf.b.do(func(b *broker) {
			if b.creates != 4 || b.closes != 2 {
				t.Errorf("creates, closes = %d, %d; want 4 (the first, two conflicts, the reopen) and 2", b.creates, b.closes)
			}
		})
		if i := slices.Index(lf.h.log, "closed"); i < 0 || slices.Index(lf.h.log[i:], "opened") < 0 {
			t.Errorf("handler calls = %v, want the replaced session closed before the next opened", lf.h.log)
		}
		if n := lf.rec.CountLevel(slog.LevelWarn, "already lapsed"); n != 2 {
			t.Errorf("lapsed-close WARN lines = %d, want 2", n)
		}
		if v := sample(t, lf.m, `manade_github_errors_total{op="session"}`); v != 2 {
			t.Errorf("session errors = %v, want the 2 conflicts and no close", v)
		}
		if n := lf.rec.CountLevel(slog.LevelError, "holds the scale set's session"); n != 1 {
			t.Errorf("held ERROR lines = %d, want 1", n)
		}
		if !lf.rec.HasAttr("no longer held elsewhere", "duration", "10s") {
			t.Errorf("logs = %v, want the held state's end line after its two 5-second retries", lf.rec.Messages())
		}
		if len(lf.h.opened) != 2 {
			t.Errorf("opened = %d, want the reopened session's statistics too", len(lf.h.opened))
		}
	})
	t.Run("failedCloseCounts", func(t *testing.T) {
		lf := newListen(t)
		lf.run(t, 2, replaceAfterGap(lf, http.StatusForbidden, 20))
		if !slices.Equal(lf.h.held, []bool{true, false}) {
			t.Errorf("held = %v, want held once the conflict outlasted the lapse, then cleared", lf.h.held)
		}
		if v := sample(t, lf.m, `manade_github_errors_total{op="session"}`); v != 22 {
			t.Errorf("session errors = %v, want both failed closes and the 20 conflicts", v)
		}
	})
}

// The conflict after a failed close may be the session's own claim, so the
// scale set stays served until the conflict outlasts the lapse.
func TestListenHeldOnlyPastTheLapse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conflicts int
		held      bool
	}{
		{name: "insideTheLapse", conflicts: 16},
		{name: "pastTheLapse", conflicts: 17, held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := newListen(t)
			lf.run(t, 2, replaceAfterGap(lf, http.StatusForbidden, tc.conflicts))
			if got := len(lf.h.held) > 0; got != tc.held {
				t.Errorf("held = %v after %d conflicts 5 seconds apart, want held %v", lf.h.held, tc.conflicts, tc.held)
			}
		})
	}
}

func TestListenPollFailure(t *testing.T) {
	lf := newListen(t)
	lf.b.pollFail = 2
	lf.run(t, 1, nil)
	if v := sample(t, lf.m, `manade_github_errors_total{op="poll"}`); v != 2 {
		t.Errorf("poll errors = %v, want 2", v)
	}
	if n := lf.rec.CountLevel(slog.LevelWarn, "Polls are failing"); n != 1 {
		t.Errorf("failing WARN lines = %d, want 1 while the state stands", n)
	}
	if n := lf.rec.CountLevel(slog.LevelInfo, "Polls recovered"); n != 1 || !lf.rec.HasAttr("Polls recovered", "duration", "10s") {
		t.Errorf("recovered INFO lines = %d, want 1 with the 10 seconds of failures", n)
	}
	if !slices.Equal(*lf.waits, []time.Duration{5 * time.Second, 5 * time.Second}) {
		t.Errorf("waits = %v, want 5 seconds after each failure", *lf.waits)
	}
	if lf.h.beats != 5 {
		t.Errorf("beats = %d, want the create, all three poll attempts and the close", lf.h.beats)
	}
}

// A failed poll never reached GitHub's claim, so polls failing for more than
// 60 seconds replace the session although each attempt began within it.
func TestListenReplacesAfterFailingPolls(t *testing.T) {
	lf := newListen(t)
	lf.b.pollFail = 13
	lf.run(t, 1, nil)
	lf.b.do(func(b *broker) {
		if b.creates != 2 || b.closes != 2 {
			t.Errorf("creates, closes = %d, %d; want the 13th failure at 65 seconds to replace the session", b.creates, b.closes)
		}
	})
	if len(*lf.waits) != 13 {
		t.Errorf("waits = %v, want 5 seconds after each failure", *lf.waits)
	}
	if v := sample(t, lf.m, `manade_github_errors_total{op="poll"}`); v != 13 {
		t.Errorf("poll errors = %v, want 13", v)
	}
}

// A poll held past 60 seconds may have outlived the session's claim, so its
// message reaches no handler and is not acked, and the session is replaced.
func TestListenDropsALatePoll(t *testing.T) {
	lf := newListen(t)
	lf.b.push(1, Stats{Assigned: 1}, job("JobAssigned", 7, "build", "", ""))
	lf.b.onPoll = func() {
		lf.b.onPoll = nil
		lf.c.advance(70 * time.Second)
	}
	lf.run(t, 1, nil)
	if len(lf.h.polls) != 1 || !lf.h.polls[0].Idle {
		t.Errorf("polls = %+v, want only the new session's idle poll", lf.h.polls)
	}
	lf.b.do(func(b *broker) {
		if len(b.acks) != 0 || b.creates != 2 {
			t.Errorf("acks %v, creates %d; want no ack and the session replaced", b.acks, b.creates)
		}
	})
	if len(*lf.waits) != 0 {
		t.Errorf("waits = %v, want the replacement at once", *lf.waits)
	}
	if !lf.rec.HasAttr("Polls recovered", "duration", "0s") || lf.rec.CountLevel(slog.LevelWarn, "Polls are failing") != 1 {
		t.Errorf("logs = %v, want the late poll logged as a failure, then recovered", lf.rec.Messages())
	}
}

// scaleset's retries are off for a poll, so a refusal waits in Listen: a
// rate limit for its Retry-After, at least a minute and at most an hour, in
// 5-second steps that each beat; anything else 5 seconds. Each create, poll
// and close beats too; a wait past 60 seconds replaces the session.
func TestListenPollRefusalWaits(t *testing.T) {
	for _, tc := range []struct {
		fail           http.HandlerFunc
		name           string
		wait           time.Duration
		creates, beats int
	}{
		{name: "serverError", wait: retryEvery, creates: 1, beats: 4, fail: reply(http.StatusServiceUnavailable, `{"message":"unavailable"}`)},
		{name: "retryAfter", wait: 90 * time.Second, creates: 2, beats: 24, fail: reply(http.StatusTooManyRequests, `{"message":"slow down"}`, "Retry-After", "90")},
		{name: "secondaryLimit", wait: time.Minute, creates: 1, beats: 16, fail: reply(http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit."}`)},
		{name: "retryAfterADay", wait: time.Hour, creates: 2, beats: 726, fail: reply(http.StatusTooManyRequests, `{"message":"slow down"}`, "Retry-After", "86400")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := newListen(t)
			lf.b.pollFail, lf.b.failWith = 1, tc.fail
			lf.run(t, 1, nil)
			steps := slices.Repeat([]time.Duration{retryEvery}, int(tc.wait/retryEvery))
			if !slices.Equal(*lf.waits, steps) {
				t.Errorf("waits = %d of %v, want %v in 5-second steps", len(*lf.waits), *lf.waits, tc.wait)
			}
			if lf.h.beats != tc.beats {
				t.Errorf("beats = %d, want %d", lf.h.beats, tc.beats)
			}
			lf.b.do(func(b *broker) {
				if len(b.polls) != 2 || b.creates != tc.creates {
					t.Errorf("polls %d, creates %d; want 2 and %d", len(b.polls), b.creates, tc.creates)
				}
			})
		})
	}
}

// A poll GitHub holds open past the cut ends there, unanswered, and its
// session is replaced at once.
func TestListenCutsAHeldPoll(t *testing.T) {
	prev := pollCut
	pollCut = 50 * time.Millisecond
	t.Cleanup(func() { pollCut = prev })
	lf := newListen(t)
	lf.b.pollFail, lf.b.failWith = 1, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	lf.run(t, 1, nil)
	if v := sample(t, lf.m, `manade_github_errors_total{op="poll"}`); v != 1 {
		t.Errorf("poll errors = %v, want the cut poll", v)
	}
	if len(lf.h.polls) != 1 || !lf.h.polls[0].Idle {
		t.Errorf("polls = %+v, want only the new session's idle poll", lf.h.polls)
	}
	lf.b.do(func(b *broker) {
		if len(b.acks) != 0 || b.creates != 2 || b.closes != 2 {
			t.Errorf("acks %v, creates %d, closes %d; want no ack and the session replaced", b.acks, b.creates, b.closes)
		}
	})
	if len(*lf.waits) != 0 {
		t.Errorf("waits = %v, want the replacement at once", *lf.waits)
	}
	if lf.h.beats != 6 {
		t.Errorf("beats = %d, want 6: 2 creates, 2 polls and 2 closes", lf.h.beats)
	}
}

// A create GitHub holds open past its timeout ends there, unanswered, and
// is retried after 5 seconds.
func TestListenCutsAHeldCreate(t *testing.T) {
	prev := createTimeout
	createTimeout = 50 * time.Millisecond
	t.Cleanup(func() { createTimeout = prev })
	lf := newListen(t)
	lf.b.createFail, lf.b.createWith = 1, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	lf.run(t, 1, nil)
	if !slices.Equal(*lf.waits, []time.Duration{retryEvery}) {
		t.Errorf("waits = %v, want one of 5 seconds", *lf.waits)
	}
	if lf.h.beats != 4 {
		t.Errorf("beats = %d, want 4: 2 creates, the poll and the close", lf.h.beats)
	}
	lf.b.do(func(b *broker) {
		if b.creates != 2 {
			t.Errorf("creates = %d, want the cut one and its retry", b.creates)
		}
	})
}

// scaleset's retries are off for a create too, so a refused create waits
// in Listen as a refused poll does, and scaleset sends it only once.
func TestListenCreateRefusalWaits(t *testing.T) {
	for _, tc := range []struct {
		fail           http.HandlerFunc
		name           string
		wait           time.Duration
		creates, beats int
	}{
		{name: "retryAfter", wait: 90 * time.Second, creates: 2, beats: 22, fail: reply(http.StatusTooManyRequests, `{"message":"slow down"}`, "Retry-After", "90")},
		{name: "retryAfterInsideTheCreate", wait: time.Minute, creates: 2, beats: 16, fail: reply(http.StatusTooManyRequests, `{"message":"slow down"}`, "Retry-After", "20")},
		{name: "serverErrorRetryAfter", wait: retryEvery, creates: 2, beats: 4, fail: reply(http.StatusServiceUnavailable, `{"message":"unavailable"}`, "Retry-After", "20")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := newListen(t)
			lf.b.createFail, lf.b.createWith = 1, tc.fail
			lf.run(t, 1, nil)
			steps := slices.Repeat([]time.Duration{retryEvery}, int(tc.wait/retryEvery))
			if !slices.Equal(*lf.waits, steps) {
				t.Errorf("waits = %d of %v, want %v in 5-second steps", len(*lf.waits), *lf.waits, tc.wait)
			}
			if lf.h.beats != tc.beats {
				t.Errorf("beats = %d, want %d", lf.h.beats, tc.beats)
			}
			lf.b.do(func(b *broker) {
				if b.creates != tc.creates {
					t.Errorf("creates = %d, want %d", b.creates, tc.creates)
				}
			})
			if v := sample(t, lf.m, `manade_github_errors_total{op="session"}`); v != 1 {
				t.Errorf("session errors = %v, want the refused create", v)
			}
		})
	}
}

// A create GitHub failed may still have made the session, so after a
// counted close a conflict is held only once it outlasts the lapse from the
// end of the last failed create. Each failure is a 500 that GitHub sends
// took after the create began; conflicts follow 5 seconds apart.
func TestListenFailedCreateMayHoldTheClaim(t *testing.T) {
	for _, tc := range []struct {
		name             string
		took             time.Duration
		fails, conflicts int
		held             bool
	}{
		{name: "insideTheLapse", fails: 2, conflicts: 15},
		{name: "pastTheLapse", fails: 2, conflicts: 16, held: true},
		// The first conflict comes 60 seconds after the create began, the
		// 16th 135, which is 80 after it ended.
		{name: "slowFailureInsideTheLapse", took: 55 * time.Second, fails: 1, conflicts: 15},
		{name: "slowFailurePastTheLapse", took: 55 * time.Second, fails: 1, conflicts: 16, held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := newListen(t)
			gap := replaceAfterGap(lf, 0, tc.conflicts)
			fail := func(w http.ResponseWriter, r *http.Request) {
				lf.c.advance(tc.took)
				reply(http.StatusInternalServerError, `{"message":"timeout"}`)(w, r)
			}
			lf.run(t, 2, func(n int) {
				gap(n)
				if n == 1 {
					lf.b.do(func(b *broker) { b.createFail, b.createWith = tc.fails, fail })
				}
			})
			if got := len(lf.h.held) > 0; got != tc.held {
				t.Errorf("held = %v after %d failed creates and %d conflicts, want held %v", lf.h.held, tc.fails, tc.conflicts, tc.held)
			}
		})
	}
}

// A refusal GitHub answered made no session, so after a counted close the
// conflict that follows it is another process at once.
func TestListenAnsweredRefusalMakesNoClaim(t *testing.T) {
	for _, tc := range []struct {
		fail http.HandlerFunc
		name string
	}{
		{name: "rateLimited", fail: reply(http.StatusTooManyRequests, `{"message":"slow down"}`, "Retry-After", "60")},
		{name: "unauthorized", fail: reply(http.StatusUnauthorized, `{"message":"Bad credentials"}`)},
		{name: "badRequest", fail: func(w http.ResponseWriter, _ *http.Request) { exception(w, http.StatusBadRequest, "ArgumentException") }},
		{name: "notFound", fail: func(w http.ResponseWriter, _ *http.Request) {
			exception(w, http.StatusNotFound, "RunnerScaleSetNotFoundException")
		}},
		// scaleset v0.5.0 types neither status; these rows pin the error text
		// unanswered reads them from.
		{name: "forbidden", fail: reply(http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)},
		{name: "unprocessable", fail: reply(http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := newListen(t)
			gap := replaceAfterGap(lf, 0, 1)
			lf.run(t, 2, func(n int) {
				gap(n)
				if n == 1 {
					lf.b.do(func(b *broker) { b.createFail, b.createWith = 1, tc.fail })
				}
			})
			if !slices.Equal(lf.h.held, []bool{true, false}) {
				t.Errorf("held = %v, want held at the one conflict, then cleared", lf.h.held)
			}
		})
	}
}

// The session's close never waits on another caller of the scope: a JIT
// whose token refresh GitHub holds open leaves the close, which must refresh
// its own token, to reach GitHub at once.
func TestListenCloseOutlastsNoJIT(t *testing.T) {
	lf := newListen(t)
	f := lf.b.f
	// Every call refreshes a token that lapses inside scaleset's minute.
	f.route("/api/v3/actions/runner-registration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"url":%q,"token":%q}`, f.srv.URL+"/svc", adminToken(30*time.Second))
	})
	var armed atomic.Bool
	held, release := make(chan struct{}), make(chan struct{})
	token := reply(http.StatusCreated, `{"token":"registration","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	f.route("/api/v3/repos/acme/app/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		if armed.CompareAndSwap(true, false) {
			close(held)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		token(w, r)
	})
	jit := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lf.h.after = func(n int) {
		if n != 1 {
			return
		}
		armed.Store(true)
		go func() {
			_, err := lf.s.JIT(t.Context(), appSmallSet, "manade-small-1")
			jit <- err
		}()
		<-held
		cancel()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lf.s.Listen(ctx, appSmallSet, lf.h)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Listen's close waited on the JIT's held token refresh")
	}
	lf.b.do(func(b *broker) {
		if b.closes != 1 {
			t.Errorf("closes = %d while the JIT's refresh was held, want 1", b.closes)
		}
	})
	close(release)
	<-done
	if err := <-jit; err != nil {
		t.Errorf("JIT: %v", err)
	}
	if n := len(f.seen("/api/v3/repos/acme/app/actions/runners/registration-token")); n != 3 {
		t.Errorf("registration token requests = %d, want the create's, the JIT's and the close's", n)
	}
}
