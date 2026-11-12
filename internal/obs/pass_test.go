package obs

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/slogx/capture"
)

func TestPassRecord(t *testing.T) {
	m, rec, c := newTest(t, defaultCapacity)
	sec := func(n int) time.Time { return start.Add(time.Duration(n) * time.Second) }
	passAt := func(n int, ok bool) {
		c.t = sec(n)
		m.Pass(t.Context(), ok)
	}
	m.PollOK(appSmall, sec(100))
	m.PollOK(orgSmall, sec(200))
	m.ScopeServing("acme/app", true)
	m.RunnerStarted(appSmall, true)
	m.RunnerStarted(orgSmall, true)
	m.RunnerStarted(appDind, false)
	m.RunnerExited(appSmall, ExitOOM)
	m.RunnerExited(appSmall, ExitFailed)
	m.QueueScopeRead("acme/app", sec(250))
	m.ImageChecked("small", sec(10))
	m.SetSource(func() Snapshot {
		return Snapshot{HeadSince: sec(60), Counts: []Counts{
			{ScaleSet: appSmall, AssignedUnstarted: 2},
			{ScaleSet: orgSmall, AssignedUnstarted: 1},
		}}
	})
	for _, n := range []int{30, 60, 90, 120} {
		passAt(n, true)
	}
	if rec.Len() != 0 {
		t.Fatalf("records before the fifth pass: %v", rec.Messages())
	}
	passAt(300, false)

	got := passRecord(t, rec, 1)
	if p := got["pool"]; p.String() != testPool {
		t.Errorf("field pool = %v, want the POOL_NAME %s", p, testPool)
	}
	delete(got, "pool")
	want := map[string]int64{
		"poll_age_seconds":       300, // appDind never polled since start
		"scopes_not_serving":     1,
		"starts":                 2,
		"start_errors":           1,
		"assigned_unstarted":     3,
		"head_wait_seconds":      240,
		"queue_read_age_seconds": 50,
		"oom_exits":              1,
		"reconcile_age_seconds":  180, // the fifth pass failed
		// dind's image was never checked, so its age runs from 0, as the
		// gauge's does.
		"image_check_age_seconds": sec(300).Unix(),
	}
	for k := range want {
		v, ok := got[k]
		if !ok || v.Kind() != slog.KindInt64 {
			t.Errorf("field %s = %v (present %v), want an integer", k, v, ok)
			continue
		}
		if v.Int64() != want[k] {
			t.Errorf("field %s = %d, want %d", k, v.Int64(), want[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("record has %d fields, want %d: %v", len(got), len(want), got)
	}

	// The counts restart with each record.
	m.ScopeServing("acme", true)
	for n := range 5 {
		passAt(310+n, true)
	}
	got = passRecord(t, rec, 2)
	for _, k := range []string{"starts", "start_errors", "oom_exits", "reconcile_age_seconds", "scopes_not_serving"} {
		if got[k].Int64() != 0 {
			t.Errorf("second record %s = %d, want 0", k, got[k].Int64())
		}
	}
}

// bootLine is one boot-phase line: when it was written, its message and its
// boot_seconds (-1 when the field is missing or not an integer).
type bootLine struct {
	at      time.Duration
	msg     string
	seconds int64
}

// bootLines answers rec's lines, timed from start.
func bootLines(rec *capture.Recorder, start time.Time) []bootLine {
	var out []bootLine
	for _, r := range rec.Records() {
		l := bootLine{at: r.Time.Sub(start), msg: r.Message, seconds: -1}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "boot_seconds" && a.Value.Kind() == slog.KindInt64 {
				l.seconds = a.Value.Int64()
			}
			return true
		})
		out = append(out, l)
	}
	return out
}

func TestBootRecordsTheBootUntilItEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log, rec := capture.New()
		start := time.Now()
		m, err := newAt(load(t, defaultCapacity), log, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if g := gather(t, m)["manade_booting{}"]; g != 1 {
			t.Errorf("manade_booting at the start = %v, want 1", g)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.Boot(ctx)
		}()
		synctest.Wait()
		// The start record precedes any wait, so a boot that fails at once
		// still leaves one.
		if got, want := bootLines(rec, start), []bootLine{{0, "manade is starting", -1}}; !slices.Equal(got, want) {
			t.Errorf("lines before any time passes = %v, want %v", got, want)
		}
		time.Sleep(5*time.Minute + time.Second)
		cancel()
		<-done
		if g := gather(t, m)["manade_booting{}"]; g != 1 {
			t.Errorf("manade_booting before Booted = %v, want 1", g)
		}
		m.Booted()
		if g := gather(t, m)["manade_booting{}"]; g != 0 {
			t.Errorf("manade_booting after Booted = %v, want 0", g)
		}
		// A boot record every bootEvery, with how long the boot has stood,
		// and the end line with its length.
		want := []bootLine{
			{0, "manade is starting", -1},
			{150 * time.Second, "manade is still starting", 150},
			{300 * time.Second, "manade is still starting", 300},
			{301 * time.Second, "manade finished starting", 301},
		}
		if got := bootLines(rec, start); !slices.Equal(got, want) {
			t.Errorf("lines = %v, want %v", got, want)
		}
	})
}

func TestPassRecordWithoutSource(t *testing.T) {
	m, rec, c := newTest(t, defaultCapacity)
	m.QueueScopeRead("acme/app", start)
	// A stamp past the record's clock, as after a wall-clock step back,
	// reads as age 0.
	m.ImageChecked("small", start.Add(2*time.Minute))
	m.ImageChecked("dind", start.Add(2*time.Minute))
	c.t = start.Add(time.Minute)
	for range 5 {
		m.Pass(t.Context(), true)
	}
	got := passRecord(t, rec, 1)
	for _, k := range []string{"assigned_unstarted", "head_wait_seconds"} {
		if v, ok := got[k]; !ok || v.Int64() != 0 {
			t.Errorf("%s = %v (present %v), want 0 with no source", k, v, ok)
		}
	}
	if got["image_check_age_seconds"].Int64() != 0 {
		t.Errorf("image_check_age_seconds = %v, want 0", got["image_check_age_seconds"])
	}
	if got["queue_read_age_seconds"].Int64() != 60 {
		t.Errorf("queue_read_age_seconds = %v, want 60", got["queue_read_age_seconds"])
	}
}

func TestPassSourceMayRecord(t *testing.T) {
	m, rec, _ := newTest(t, defaultCapacity)
	m.SetSource(func() Snapshot {
		m.PollOK(appSmall, start)
		return Snapshot{}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			m.Pass(t.Context(), true)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Pass deadlocked on a Source that records")
	}
	if rec.Len() != 1 {
		t.Errorf("records = %d, want 1", rec.Len())
	}
}
