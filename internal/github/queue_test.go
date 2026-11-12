package github

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/slogx/capture"
)

const queueLayout = `scopes:
  - repo: acme/app
profiles:
  small: {}
  large: {}
`

const (
	runsPath = "/repos/acme/app/actions/runs"
	listQ    = runsPath + "?status=queued&per_page=100"
	listIP   = runsPath + "?status=in_progress&per_page=100"
	listW    = runsPath + "?status=waiting&per_page=100"
)

func jobsPath(id int64) string { return fmt.Sprintf("%s/%d/jobs", runsPath, id) }

// newTestSweep builds a sweep over queueLayout on a settable clock, with
// every runs list empty until a test routes it.
func newTestSweep(t *testing.T, f *fakeGitHub) (*Sweep, *REST, *clock) {
	t.Helper()
	cfg := load(t, false, queueLayout)
	r, _, _ := newTestREST(t, f, cfg)
	for _, l := range []string{listQ, listIP, listW} {
		f.route(l, etagged(runs(0)))
	}
	s := NewSweep(cfg, r)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s.now = c.now
	return s, r, c
}

// captureSweep sends s's log lines to a recorder.
func captureSweep(t *testing.T, s *Sweep) *capture.Recorder {
	t.Helper()
	log, rec := capture.New()
	s.log = log
	return rec
}

func names(q Queue) string {
	var out []string
	for _, j := range q.Jobs {
		out = append(out, j.Name+":"+j.Profile)
	}
	return strings.Join(out, ",")
}

func TestSweepListsQueuedJobsInsideStartedRuns(t *testing.T) {
	f := newFake(t)
	s, r, _ := newTestSweep(t, f)
	f.route(listIP, etagged(runs(1, 7)))
	f.route(jobsPath(7), etagged(jobsPage(7,
		[]string{"build", "in_progress", "small"},
		[]string{"test", "queued", "small"},
		[]string{"lint", "queued", "small", "gpu"},
		[]string{"docs", "queued", "Large"},
		[]string{"other", "queued", "ubuntu-latest"},
		[]string{"done", "completed", "small"},
	)))
	qs := s.sweep(t.Context(), false)
	if len(qs) != 1 || !qs[0].Complete {
		t.Fatalf("sweep = %+v, want one complete read", qs)
	}
	if got := names(qs[0]); got != "test:small,docs:large" {
		t.Errorf("jobs = %q, want the queued jobs whose only label is a profile's name", got)
	}
	if j := qs[0].Jobs[0]; j.RunID != 7 || j.Created.IsZero() {
		t.Errorf("job = %+v, want run 7 with its created_at", j)
	}
	if got := sample(t, r.m, `manade_queue_read_last_success_timestamp_seconds{scope="acme/app"}`); got != float64(s.now().Unix()) {
		t.Errorf("queue read stamp = %v, want the sweep's time", got)
	}
}

func TestSweepRevalidatesUnchangedPages(t *testing.T) {
	f := newFake(t)
	s, r, _ := newTestSweep(t, f)
	f.route(listQ, etagged(runs(1, 3)))
	f.route(jobsPath(3), etagged(jobsPage(3, []string{"test", "queued", "small"})))
	first := s.sweep(t.Context(), false)
	second := s.sweep(t.Context(), false)
	if names(first[0]) != "test:small" || names(second[0]) != "test:small" || !second[0].Complete {
		t.Fatalf("sweeps = %+v then %+v, want the same complete read", first, second)
	}
	if got := sample(t, r.m, `manade_rest_reads_total{result="changed"}`); got != 4 {
		t.Errorf("changed reads = %v, want 4 (three runs lists and one jobs list)", got)
	}
	if got := sample(t, r.m, `manade_rest_reads_total{result="unchanged"}`); got != 4 {
		t.Errorf("unchanged reads = %v, want the second sweep's 4", got)
	}
	if q := s.quotas[r.auth.limit[s.scopes[0].Key]]; q.billed != 0 {
		t.Errorf("billed = %d, want no billed read in a sweep of 304s", q.billed)
	}
	if c := f.seen(jobsPath(3)); len(c) != 2 || c[1].header.Get("If-None-Match") == "" {
		t.Errorf("second jobs read = %+v, want it sent with the held ETag", c)
	}
}

func TestSweepFollowsLinkPages(t *testing.T) {
	page := func(n int) string { return listQ + "&page=" + fmt.Sprint(n) }
	link := func(n int) string { return fmt.Sprintf(`<%%s%s>; rel="next", <%%s%s>; rel="last"`, page(n), page(3)) }
	setup := func(t *testing.T, second http.HandlerFunc) (*Sweep, *fakeGitHub) {
		t.Helper()
		f := newFake(t)
		s, _, _ := newTestSweep(t, f)
		f.route(listQ, etagged(runs(3, 1), "Link", fmt.Sprintf(link(2), f.srv.URL, f.srv.URL)))
		f.route(page(2), second)
		f.route(page(3), etagged(runs(3, 3)))
		for id := int64(1); id <= 3; id++ {
			f.route(jobsPath(id), etagged(jobsPage(id, []string{fmt.Sprint("job", id), "queued", "small"})))
		}
		return s, f
	}
	t.Run("threePages", func(t *testing.T) {
		var s *Sweep
		var f *fakeGitHub
		s, f = setup(t, func(w http.ResponseWriter, r *http.Request) {
			etagged(runs(3, 2), "Link", fmt.Sprintf(link(3), f.srv.URL, f.srv.URL))(w, r)
		})
		if q := s.sweep(t.Context(), false)[0]; names(q) != "job1:small,job2:small,job3:small" || !q.Complete {
			t.Fatalf("first sweep = %+v, want the three pages' jobs", q)
		}
		if q := s.sweep(t.Context(), false)[0]; !q.Complete {
			t.Fatalf("second sweep = %+v, want complete", q)
		}
		var third int
		for _, c := range f.seen(runsPath) {
			if strings.HasSuffix(c.query, "page=3") {
				third++
			}
		}
		if third != 2 {
			t.Errorf("page 3 reads = %d, want 2: the 304 on page 2 continues through its held Link", third)
		}
	})
	t.Run("failedSecondPage", func(t *testing.T) {
		s, _ := setup(t, reply(http.StatusUnprocessableEntity, `{}`))
		log := captureSweep(t, s)
		q := s.sweep(t.Context(), false)[0]
		if !log.HasAttr("Queue reads are failing", "scope", "acme/app") {
			t.Errorf("logs = %v, want the failing-reads WARN", log.Messages())
		}
		if q.Complete || len(q.Jobs) != 0 {
			t.Errorf("sweep = %+v, want an incomplete read that funds nothing", q)
		}
		if got := sample(t, s.rest.m, `manade_github_errors_total{op="queue"}`); got != 1 {
			t.Errorf("queue errors = %v, want 1", got)
		}
		if got := sample(t, s.rest.m, `manade_queue_read_last_success_timestamp_seconds{scope="acme/app"}`); got == float64(s.now().Unix()) {
			t.Error("a lost page stamped the scope's queue read")
		}
	})
	t.Run("linkOffOrigin", func(t *testing.T) {
		f, other := newFake(t), newFake(t)
		s, _, _ := newTestSweep(t, f)
		other.route(runsPath, etagged(runs(3, 2)))
		f.route(listQ, etagged(runs(3, 1), "Link", "<"+other.srv.URL+runsPath+`?page=2>; rel="next"`))
		if q := s.sweep(t.Context(), false)[0]; q.Complete {
			t.Errorf("sweep = %+v, want a link off the API origin refused", q)
		}
		if c := other.seen(runsPath); len(c) != 0 {
			t.Errorf("the other origin received %d requests carrying the token", len(c))
		}
	})
}

// endlessRuns routes a queued runs list of total runs that names a next page
// on every page, each page holding per runs no other page holds. Every
// run's jobs list is empty, with a poll interval past the test's sweeps.
func endlessRuns(f *fakeGitHub, per, total int) {
	endless := func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n = max(n, 1)
		ids := make([]int64, per)
		for i := range ids {
			ids[i] = int64((n-1)*per + i + 1)
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s%s&page=%d>; rel="next"`, f.srv.URL, listQ, n+1))
		_, _ = fmt.Fprint(w, runs(total, ids...))
	}
	f.route(listQ, endless)
	f.route(runsPath, endless)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallback = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Poll-Interval", "600")
		_, _ = fmt.Fprint(w, jobsPage(0))
	}
}

func TestSweepRanksNewestRunsPastWindow(t *testing.T) {
	f := newFake(t)
	s, _, c := newTestSweep(t, f)
	log := captureSweep(t, s)
	endlessRuns(f, 100, 1500)
	if q := s.sweep(t.Context(), false)[0]; !q.Complete {
		t.Fatalf("sweep = %+v, want a complete read of the newest 1,000 runs", q)
	}
	if got := len(f.seen(runsPath)) - 2; got != 10 {
		t.Errorf("queued pages read = %d, want GitHub's window of 10", got)
	}
	if !log.HasAttr("passed GitHub's list limit", "runs", "1500") || !log.HasAttr("passed GitHub's list limit", "state", "queued") {
		t.Errorf("logs = %v, want the window WARN with the state and count", log.Messages())
	}
	s.sweep(t.Context(), false)
	if got := log.Count("passed GitHub's list limit"); got != 1 {
		t.Errorf("window WARN lines = %d, want 1 while the state stands", got)
	}
	f.route(listQ, etagged(runs(3)))
	c.advance(90 * time.Second)
	s.sweep(t.Context(), false)
	if !log.HasAttr("back inside GitHub's list limit", "duration", "1m30s") {
		t.Errorf("logs = %v, want the window's end line with its duration", log.Messages())
	}
}

// GitHub's window is 1,000 runs, not 10 pages: a list still going at its
// page bound with fewer runs read lost runs, so the scope's read is
// incomplete.
func TestSweepRefusesShortPagesAtTheBound(t *testing.T) {
	f := newFake(t)
	s, _, _ := newTestSweep(t, f)
	log := captureSweep(t, s)
	endlessRuns(f, 50, 900)
	if q := s.sweep(t.Context(), false)[0]; q.Complete {
		t.Errorf("sweep = %+v, want an incomplete read after 500 of the list's runs", q)
	}
	if got := len(f.seen(runsPath)); got != 10 {
		t.Errorf("queued pages read = %d, want the bound of 10", got)
	}
	if !log.HasAttr("Queue reads are failing", "scope", "acme/app") || log.Count("passed GitHub's list limit") != 0 {
		t.Errorf("logs = %v, want the failing-reads WARN and no window WARN", log.Messages())
	}
}

func TestSweepHoldsOnRateLimit(t *testing.T) {
	for _, tc := range []struct {
		header     func(c *clock) []string
		name, body string
		code       int
		waits      []time.Duration
	}{
		{
			name: "primaryUntilReset", code: http.StatusForbidden, waits: []time.Duration{5 * time.Minute, 5 * time.Minute},
			header: func(c *clock) []string {
				return []string{"X-RateLimit-Remaining", "0", "X-RateLimit-Limit", "5000", "X-RateLimit-Reset", c.unix(5 * time.Minute)}
			},
		},
		{
			name: "retryAfter", code: http.StatusTooManyRequests, waits: []time.Duration{2 * time.Minute, 2 * time.Minute},
			header: func(*clock) []string { return []string{"Retry-After", "120"} },
		},
		{
			name: "secondaryDoubles", code: http.StatusForbidden, waits: []time.Duration{time.Minute, 2 * time.Minute},
			body:   `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			header: func(*clock) []string { return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			s, _, c := newTestSweep(t, f)
			log := captureSweep(t, s)
			for i, wait := range tc.waits {
				f.route(listQ, reply(tc.code, cmp.Or(tc.body, `{"message":"limited"}`), tc.header(c)...))
				before := len(f.seen(runsPath))
				if q := s.sweep(t.Context(), false)[0]; q.Complete || len(f.seen(runsPath)) != before+1 {
					t.Fatalf("limited sweep %d = %+v, want one refused read", i, q)
				}
				c.advance(wait - time.Second)
				if q := s.sweep(t.Context(), false)[0]; q.Complete || len(f.seen(runsPath)) != before+1 {
					t.Fatalf("sweep %d a second before the wait sent a read", i)
				}
				c.advance(time.Second)
			}
			f.route(listQ, etagged(runs(0)))
			if q := s.sweep(t.Context(), false)[0]; !q.Complete {
				t.Fatalf("sweep after the wait = %+v, want complete", q)
			}
			if log.CountLevel(slog.LevelWarn, "GitHub rate limit reached") != 1 || !log.Contains("Queue reads resumed after the rate limit") {
				t.Errorf("logs = %v, want one rate-limit WARN and the resume line", log.Messages())
			}
			if got := sample(t, s.rest.m, `manade_github_errors_total{op="queue"}`); got != 2 {
				t.Errorf("queue errors = %v, want each refusal counted", got)
			}
			// The success ended the doubling: the next refusal waits the first wait.
			f.route(listQ, reply(tc.code, cmp.Or(tc.body, `{}`), tc.header(c)...))
			s.sweep(t.Context(), false)
			c.advance(tc.waits[0])
			before := len(f.seen(runsPath))
			s.sweep(t.Context(), false)
			if len(f.seen(runsPath)) == before {
				t.Errorf("a refusal after a success held reads past %v", tc.waits[0])
			}
		})
	}
}

// A 403 that is no rate limit, such as an SSO or permission refusal, fails
// its own scope's read and holds no other scope on the same credential.
func TestSweepPlainRefusalHoldsNothing(t *testing.T) {
	f := newFake(t)
	cfg := load(t, false, "scopes: [{repo: acme/app}, {repo: other/web}]\nprofiles: {small: {}}\n")
	r, _, _ := newTestREST(t, f, cfg)
	s := NewSweep(cfg, r)
	log := captureSweep(t, s)
	for _, l := range []string{listQ, listIP, listW} {
		f.route(l, reply(http.StatusForbidden, `{"message":"Resource protected by organization SAML enforcement."}`))
	}
	web := "/repos/other/web/actions/runs"
	f.route(web, etagged(runs(0)))
	for range 2 {
		qs := s.sweep(t.Context(), false)
		if len(qs) != 2 || qs[0].Complete || !qs[1].Complete {
			t.Fatalf("sweep = %+v, want acme/app failed and other/web read whole", qs)
		}
	}
	if n := len(f.seen(runsPath)); n != 2 {
		t.Errorf("acme/app reads = %d, want one per sweep with no hold", n)
	}
	if n := len(f.seen(web)); n != 6 {
		t.Errorf("other/web reads = %d, want its three lists in each sweep", n)
	}
	if log.Contains("rate limit") || !log.HasAttr("Queue reads are failing", "scope", "acme/app") {
		t.Errorf("logs = %v, want the scope's failing WARN and no rate-limit line", log.Messages())
	}
}

// An installation's rate limit is the account's, so a hold on one repo
// scope's reads holds every repo scope of that account and no other.
func TestSweepHoldsTheAccountsInstallation(t *testing.T) {
	f := newFake(t)
	cfg := load(t, true, appLayout)
	r, _, _ := newTestREST(t, f, cfg)
	f.app(42, "/repos/acme/app/installation", "/repos/acme/web/installation")
	f.app(77)
	f.route(listQ, reply(http.StatusTooManyRequests, `{"message":"limited"}`, "Retry-After", "120"))
	for _, repo := range []string{"acme/web", "solo/tool"} {
		for _, state := range runStates {
			f.route("/repos/"+repo+"/actions/runs?status="+state+"&per_page=100", etagged(runs(0)))
		}
	}
	qs := NewSweep(cfg, r).sweep(t.Context(), false)
	if len(qs) != 3 || qs[0].Complete || qs[1].Complete || !qs[2].Complete {
		t.Fatalf("sweep = %+v, want acme/app limited, acme/web held, solo/tool read whole", qs)
	}
	if n := len(f.seen("/repos/acme/web/actions/runs")); n != 0 {
		t.Errorf("acme/web reads = %d, want none while its installation is held", n)
	}
}

// A kept job without created_at would rank oldest in the one queue, so it
// fails the scope's read; a job no profile takes needs none.
func TestSweepRefusesAJobWithoutCreated(t *testing.T) {
	for _, tc := range []struct {
		name, job string
		complete  bool
	}{
		{name: "absent", job: `{"name":"build","status":"queued","labels":["small"]}`},
		{name: "null", job: `{"name":"build","status":"queued","labels":["small"],"created_at":null}`},
		{name: "notKept", job: `{"name":"build","status":"queued","labels":["gpu"]}`, complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			s, _, _ := newTestSweep(t, f)
			f.route(listQ, etagged(runs(1, 3)))
			f.route(jobsPath(3), jsonOK(`{"total_count":1,"jobs":[`+tc.job+`]}`))
			if q := s.sweep(t.Context(), false)[0]; q.Complete != tc.complete || len(q.Jobs) != 0 {
				t.Errorf("sweep = %+v, want complete %v with no job", q, tc.complete)
			}
		})
	}
}

func TestSweepSpacingKeepsReserve(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining string
		reset     time.Duration
		want      time.Duration
	}{
		{name: "plenty", remaining: "4999", reset: time.Hour, want: minSpacing},
		// 1,000 left less a reserve of 500 fits 125 sweeps of 4 billed
		// reads in the hour before the reset.
		{name: "paced", remaining: "1000", reset: time.Hour, want: 3600 * time.Second / 125},
		{name: "spent", remaining: "503", reset: 30 * time.Minute, want: 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			s, _, c := newTestSweep(t, f)
			h := []string{"X-RateLimit-Remaining", tc.remaining, "X-RateLimit-Limit", "5000", "X-RateLimit-Reset", c.unix(tc.reset)}
			for _, l := range []string{listQ, listIP, listW} {
				f.route(l, etagged(runs(1, 9), h...))
			}
			f.route(jobsPath(9), etagged(jobsPage(9, []string{"a", "queued", "small"}), h...))
			s.sweep(t.Context(), false)
			if got := s.spacing(); got != tc.want {
				t.Errorf("spacing = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSweepReadsColdStartedRunsOncePerMinute(t *testing.T) {
	f := newFake(t)
	s, _, c := newTestSweep(t, f)
	f.route(listIP, etagged(runs(2, 4, 5)))
	f.route(jobsPath(4), etagged(jobsPage(4, []string{"build", "in_progress", "small"})))
	f.route(jobsPath(5), etagged(jobsPage(5, []string{"build", "in_progress", "small"}, []string{"test", "queued", "small"})))
	for i, step := range []struct {
		advance time.Duration
		hot     bool
		cold    int
	}{
		{0, false, 1}, {30 * time.Second, false, 1}, {0, true, 2}, {59 * time.Second, false, 2}, {time.Second, false, 3},
	} {
		c.advance(step.advance)
		s.sweep(t.Context(), step.hot)
		if got := len(f.seen(jobsPath(4))); got != step.cold {
			t.Errorf("step %d: cold run reads = %d, want %d", i, got, step.cold)
		}
		if got := len(f.seen(jobsPath(5))); got != i+1 {
			t.Errorf("step %d: run with a waiting job read %d times, want every sweep", i, got)
		}
	}
}

func TestSweepHonoursPollInterval(t *testing.T) {
	f := newFake(t)
	s, _, c := newTestSweep(t, f)
	f.route(listQ, etagged(runs(1, 6), "X-Poll-Interval", "60"))
	f.route(jobsPath(6), etagged(jobsPage(6, []string{"a", "queued", "small"})))
	s.sweep(t.Context(), false)
	c.advance(59 * time.Second)
	if q := s.sweep(t.Context(), false)[0]; names(q) != "a:small" || !q.Complete {
		t.Errorf("sweep inside the interval = %+v, want the last read's jobs", q)
	}
	c.advance(time.Second)
	s.sweep(t.Context(), false)
	var queued int
	for _, call := range f.seen(runsPath) {
		if strings.HasPrefix(call.query, "status=queued") {
			queued++
		}
	}
	if queued != 2 {
		t.Errorf("queued list reads = %d, want 2: none inside x-poll-interval", queued)
	}
}

func TestSweepRunSpacesSweeps(t *testing.T) {
	f := newFake(t)
	s, _, _ := newTestSweep(t, f)
	waits := noSleep(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var sweeps int
	s.Run(ctx, func() bool { return false }, func(time.Time, []Queue) {
		if sweeps++; sweeps == 3 {
			cancel()
		}
	})
	if sweeps != 3 || len(*waits) != 3 || (*waits)[0] != minSpacing {
		t.Errorf("sweeps = %d with waits %v, want each sweep followed by the 10-second spacing", sweeps, *waits)
	}
}
