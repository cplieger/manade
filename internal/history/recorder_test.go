package history

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

// A run's row is written at its assignment, gains its runner and host at
// its start and its result and exit at its end.
func TestARunFromAssignmentToEnd(t *testing.T) {
	at := queued.Add(10 * time.Second)
	rec, st := recorder(t, &at)
	write(t, rec, "", message(github.Assigned, 7, "", ""))
	t.Run("assigned", func(t *testing.T) {
		r, found := run(t, st, 7)
		want := store.Run{
			ID: 7, SeenAt: at, Scope: "acme", Profile: "small", Repository: "acme/app", RunID: 41, Job: "build", Event: "push",
			WorkflowRef: "acme/app/.github/workflows/ci.yaml@refs/heads/main", QueuedAt: queued, AssignedAt: queued.Add(10 * time.Second),
		}
		if !found || !equalRun(&r, &want) {
			t.Errorf("row = %+v, %v; want %+v", r, found, want)
		}
	})
	at = queued.Add(31 * time.Second)
	write(t, rec, "nas", message(github.Started, 7, "manade-small-a1", ""))
	t.Run("started", func(t *testing.T) {
		r, _ := run(t, st, 7)
		if r.Runner != "manade-small-a1" || r.Host != "nas" || !r.StartedAt.Equal(queued.Add(30*time.Second)) || r.WaitMS != 30000 || !r.EndedAt.IsZero() {
			t.Errorf("row = %+v, want started on nas after a 30 s wait and open", r)
		}
	})
	at = queued.Add(6 * time.Minute)
	write(t, rec, "nas", message(github.Completed, 7, "manade-small-a1", "Succeeded"))
	rec.Exited(t.Context(), exitOf("manade-small-a1", pool.ExitCompleted, 0, queued.Add(6*time.Minute)))
	write(t, rec, "nas", message(github.Completed, 7, "manade-small-a1", "Succeeded"))
	t.Run("ended", func(t *testing.T) {
		r, _ := run(t, st, 7)
		if r.Result != "succeeded" || r.Exit != "completed" || r.ExitCode == nil || *r.ExitCode != 0 ||
			!r.FinishedAt.Equal(queued.Add(5*time.Minute+30*time.Second)) || r.DurationMS != 300000 ||
			!r.EndedAt.Equal(r.FinishedAt) || !r.ExitedAt.Equal(queued.Add(6*time.Minute)) {
			t.Errorf("row = %+v, want succeeded and completed, ended at its finish after 5 minutes", r)
		}
	})
}

// GitHub's verdict on the job and the pool's on the runner are kept
// apart, in either order: a job that failed its tests on a runner that
// completed, and a lost runner whose job GitHub later fails.
func TestResultAndExitAreKeptApart(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	write(t, rec, "nas", message(github.Started, 1, "r1", ""))
	write(t, rec, "nas", message(github.Completed, 1, "r1", "failed"))
	rec.Exited(t.Context(), exitOf("r1", pool.ExitCompleted, 0, queued.Add(6*time.Minute)))
	write(t, rec, "nas", message(github.Started, 2, "r2", ""))
	rec.Exited(t.Context(), exitOf("r2", pool.ExitLost, 255, queued.Add(2*time.Minute)))
	write(t, rec, "nas", message(github.Completed, 2, "r2", "failed"))
	for _, tc := range []struct {
		name         string
		result, exit string
		id           uint64
	}{
		{name: "failedOnACompletedRunner", id: 1, result: "failed", exit: "completed"},
		{name: "lostThenFailed", id: 2, result: "failed", exit: "lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r, _ := run(t, st, tc.id); r.Result != tc.result || r.Exit != tc.exit {
				t.Errorf("row = result %q, exit %q; want %q and %q", r.Result, r.Exit, tc.result, tc.exit)
			}
		})
	}
	t.Run("lostEndsAtItsExit", func(t *testing.T) {
		if r, _ := run(t, st, 2); !r.EndedAt.Equal(queued.Add(2 * time.Minute)) {
			t.Errorf("ended at %v, want the exit's %v", r.EndedAt, queued.Add(2*time.Minute))
		}
	})
}

// GitHub's words map to the run's results, an unknown one to other with
// its text beside it.
func TestResults(t *testing.T) {
	for _, tc := range []struct{ raw, res, text string }{
		{raw: "succeeded", res: "succeeded"},
		{raw: "Failed", res: "failed"},
		{raw: "canceled", res: "cancelled"},
		{raw: "skipped", res: "other", text: "skipped"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if res, text := result(tc.raw); res != tc.res || text != tc.text {
				t.Errorf("result(%q) = %q, %q; want %q, %q", tc.raw, res, text, tc.res, tc.text)
			}
		})
	}
}

// An exit can reach the control plane before its job's message names the
// runner: it is kept with its evidence, and the message that binds the
// runner takes both.
func TestAnExitBeforeItsJobsMessageReachesItsRun(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	x := exitOf("r1", pool.ExitFailed, 1, queued.Add(time.Minute))
	x.Log, x.Diag = []byte("boom\n"), []byte("tar")
	rec.Exited(t.Context(), x)
	if _, found := run(t, st, 7); found {
		t.Fatal("an exit wrote a row")
	}
	write(t, rec, "", message(github.Started, 7, "r1", ""))
	r, _ := run(t, st, 7)
	if r.Exit != "failed" || r.Host != "nas" || r.ExitCode == nil || *r.ExitCode != 1 {
		t.Errorf("row = %+v, want the kept failed exit on nas", r)
	}
	if b, found := blob(t, st, 7); !found || b.Log != "boom\n" || string(b.Diag) != "tar" {
		t.Errorf("evidence = %+v, %v; want the kept log and tar", b, found)
	}
	if err := st.UpdateHistory(t.Context(), func(h *store.HistoryTx) error {
		if _, kept, err := h.Exit("r1"); kept || err != nil {
			t.Errorf("the exit is still kept (%v)", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The first runner list of a process closes as lost every open run of
// that host whose runner it does not hold, and leaves the rest open.
func TestAListClosesTheRunsItsHostNoLongerHolds(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	write(t, rec, "nas", message(github.Started, 1, "r1", ""))
	write(t, rec, "nas", message(github.Started, 2, "r2", ""))
	write(t, rec, "box", message(github.Started, 3, "r3", ""))
	at = queued.Add(time.Hour)
	rec.Listed(t.Context(), "nas", []string{"r2"})
	for _, tc := range []struct {
		name string
		exit string
		id   uint64
	}{
		{name: "unlisted", id: 1, exit: "lost"},
		{name: "listed", id: 2},
		{name: "otherHost", id: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := run(t, st, tc.id)
			if r.Exit != tc.exit || (tc.exit != "") != r.ExitedAt.Equal(at) {
				t.Errorf("run %d exit = %q at %v, want %q", tc.id, r.Exit, r.ExitedAt, tc.exit)
			}
		})
	}
}

// GitHub's reassignment cancel removes a row no runner took; a cancel
// with a finish time ends it with no host.
func TestCancels(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	write(t, rec, "", message(github.Assigned, 1, "", ""))
	write(t, rec, "", github.Event{Kind: github.Completed, RequestID: 1, Result: "canceled"})
	if _, found := run(t, st, 1); found {
		t.Error("a reassignment cancel left its row")
	}
	write(t, rec, "", message(github.Assigned, 2, "", ""))
	write(t, rec, "", message(github.Completed, 2, "", "canceled"))
	r, found := run(t, st, 2)
	if !found || r.Result != "cancelled" || r.Host != "" || r.EndedAt.IsZero() {
		t.Errorf("row = %+v, %v; want ended cancelled with no host", r, found)
	}
}

// Evidence is kept only for an exit that carries it: the log tail with its
// escapes gone, no row for a clean exit.
func TestEvidenceOnlyAtAnUncleanExit(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	write(t, rec, "nas", message(github.Started, 1, "r1", ""))
	write(t, rec, "nas", message(github.Started, 2, "r2", ""))
	x := exitOf("r1", pool.ExitOOM, 137, queued.Add(time.Minute))
	x.Log, x.Diag = []byte("\x1b[31mKilled\x1b[0m\r\n"), []byte("tar")
	rec.Exited(t.Context(), x)
	rec.Exited(t.Context(), exitOf("r2", pool.ExitCompleted, 0, queued.Add(time.Minute)))
	if b, found := blob(t, st, 1); !found || b.Log != "Killed\n" || string(b.Diag) != "tar" || !b.StoredAt.Equal(x.At) {
		t.Errorf("evidence = %+v, %v; want the stripped tail and the tar", b, found)
	}
	if _, found := blob(t, st, 2); found {
		t.Error("a clean exit kept evidence")
	}
}

// An assignment's reason lands on the run row, later messages with none
// keep it, and a later assignment's replaces it.
func TestTheLongestWaitReasonLandsOnTheRun(t *testing.T) {
	at := queued.Add(10 * time.Second)
	rec, st := recorder(t, &at)
	waited := func(e github.Event, reason pool.JobReason) {
		rec.write(t.Context(), &jobMsg{at: at, e: e, ss: ss, host: "nas", waited: reason})
	}
	reason := func() string {
		r, _ := run(t, st, 7)
		return r.WaitReason
	}
	waited(message(github.Assigned, 7, "", ""), pool.JobWaitingForSlot)
	if got := reason(); got != "waiting_for_slot" {
		t.Errorf("after the assignment, reason %q; want waiting_for_slot", got)
	}
	waited(message(github.Started, 7, "r7", ""), "")
	if got := reason(); got != "waiting_for_slot" {
		t.Errorf("after the start, reason %q; want the assignment's waiting_for_slot kept", got)
	}
	waited(message(github.Assigned, 7, "", ""), pool.JobNoHostServing)
	if got := reason(); got != "no_host_serving" {
		t.Errorf("after a second assignment, reason %q; want its no_host_serving", got)
	}
}

// Job queues messages for Run, which writes them in order, with their
// reasons, and those still queued at shutdown; a message naming no request
// is no run's.
func TestRunWritesTheQueuedMessages(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	rec.Job(ss, "nas", new(message(github.Started, 0, "r0", "")), "")
	rec.Job(ss, "", new(message(github.Assigned, 1, "", "")), pool.JobProfileLimit)
	rec.Job(ss, "nas", new(message(github.Started, 1, "r1", "")), "")
	rec.Job(ss, "nas", new(message(github.Completed, 1, "r1", "succeeded")), "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec.Run(ctx)
	if r, _ := run(t, st, 1); r.Result != "succeeded" || r.Runner != "r1" || r.WaitReason != "profile_limit" {
		t.Errorf("row = %+v, want assigned after profile_limit, started, then succeeded", r)
	}
	if _, found := run(t, st, 0); found {
		t.Error("a message with no request wrote a row")
	}
}

// The log tail keeps the last 200 lines and the last 64 KiB.
func TestLogTailIsCapped(t *testing.T) {
	var b strings.Builder
	for i := range 300 {
		b.WriteString(strings.Repeat("x", i%7) + "\n")
	}
	if got := strings.Count(logTail([]byte(b.String())), "\n"); got != 200 {
		t.Errorf("lines = %d, want 200", got)
	}
	long := strings.Repeat("y", 100<<10) + "\nend\n"
	got := logTail([]byte(long))
	if len(got) != 64<<10 || !strings.HasSuffix(got, "\nend\n") {
		t.Errorf("tail of a long line = %d bytes ending %q, want 64 KiB ending the log", len(got), got[max(0, len(got)-5):])
	}
}

// ECMA-48's sequences go, and the text around them stays.
func TestStripEscapes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{name: "sgr", in: "\x1b[1;31mred\x1b[0m", want: "red"},
		{name: "c1Csi", in: "a\u009b2Kb", want: "ab"},
		{name: "oscBel", in: "\x1b]0;title\x07after", want: "after"},
		{name: "oscSt", in: "\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\", want: "link"},
		{name: "dcs", in: "\x1bPq#0\x1b\\done", want: "done"},
		{name: "twoByte", in: "\x1b=keypad\x1b(Bascii", want: "keypadascii"},
		{name: "brokenCsiKeepsTheLine", in: "\x1b[12\nnext", want: "\nnext"},
		{name: "unterminated", in: "ok\x1b]0;never", want: "ok"},
		{name: "plain", in: "tab\there\nnew line", want: "tab\there\nnew line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripEscapes(tc.in); got != tc.want {
				t.Errorf("stripEscapes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func equalRun(a, b *store.Run) bool {
	return a.ID == b.ID && a.SeenAt.Equal(b.SeenAt) && a.Scope == b.Scope && a.Profile == b.Profile && a.Repository == b.Repository &&
		a.RunID == b.RunID && a.Job == b.Job && a.Event == b.Event && a.WorkflowRef == b.WorkflowRef &&
		a.QueuedAt.Equal(b.QueuedAt) && a.AssignedAt.Equal(b.AssignedAt) && a.StartedAt.Equal(b.StartedAt) &&
		a.Runner == b.Runner && a.Host == b.Host && a.Result == b.Result && a.Exit == b.Exit && a.EndedAt.Equal(b.EndedAt)
}

// usageExit is runner name's completed exit at at, reporting cpu of CPU
// time over a 5-minute life and a peak of peak.
func usageExit(name string, cpu time.Duration, peak int64, at time.Time) *pool.RunnerExit {
	x := exitOf(name, pool.ExitCompleted, 0, at)
	x.Usage = &hostapi.Usage{CPU: cpu, Ran: 5*time.Minute + 1500*time.Microsecond, PeakMemory: peak}
	return x
}

// A runner's usage lands on its run in whole milliseconds, kept with an
// exit that came first.
func TestUsageReachesTheRun(t *testing.T) {
	at := queued
	rec, st := recorder(t, &at)
	write(t, rec, "nas", message(github.Started, 1, "r1", ""))
	write(t, rec, "nas", message(github.Completed, 1, "r1", "succeeded"))
	rec.Exited(t.Context(), usageExit("r1", 90*time.Second+400*time.Microsecond, 300<<20, queued.Add(6*time.Minute)))
	rec.Exited(t.Context(), usageExit("r2", 30*time.Second, 200<<20, queued.Add(7*time.Minute)))
	write(t, rec, "nas", message(github.Started, 2, "r2", ""))
	write(t, rec, "nas", message(github.Completed, 2, "r2", "succeeded"))
	rec.Exited(t.Context(), usageExit("r1", time.Hour, 1<<30, queued.Add(8*time.Minute)))
	for _, tc := range []struct {
		name string
		want store.Usage
		id   uint64
	}{
		{name: "an exit after its messages", id: 1, want: store.Usage{CPUMS: 90000, RanMS: 300001, PeakMemory: 300 << 20}},
		{name: "an exit kept until its message", id: 2, want: store.Usage{CPUMS: 30000, RanMS: 300001, PeakMemory: 200 << 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := run(t, st, tc.id)
			if r.Usage == nil || *r.Usage != tc.want {
				t.Errorf("usage = %+v, want %+v", r.Usage, tc.want)
			}
		})
	}
	t.Run("an exit with no usage", func(t *testing.T) {
		write(t, rec, "nas", message(github.Started, 3, "r3", ""))
		rec.Exited(t.Context(), exitOf("r3", pool.ExitFailed, 1, queued.Add(9*time.Minute)))
		if r, _ := run(t, st, 3); r.Usage != nil {
			t.Errorf("usage = %+v, want none", r.Usage)
		}
	})
}
