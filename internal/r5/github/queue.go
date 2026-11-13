package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/obs"
)

// Sweep timing, from GitHub's REST best practices
// (https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)
// and its rate-limit rules
// (https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).
const (
	minSpacing  = 10 * time.Second
	coldRead    = time.Minute
	firstHold   = time.Minute
	maxBackoff  = 15 * time.Minute
	maxHold     = time.Hour // the primary limit's window; a longer server wait is cut to it
	runsWindow  = 1000      // GitHub lists at most this many results for a filtered search
	maxRunPages = runsWindow / 100
	maxJobPages = 50
)

// runStates are the run states whose runs can hold a queued job: a started
// run's later jobs, and a run held for an environment's approval, wait
// inside it.
var runStates = [...]string{"queued", "in_progress", "waiting"}

// QueuedJob is a queued job whose only label is a profile's name. Name is
// GitHub's text.
type QueuedJob struct {
	Created time.Time
	Name    string
	Profile string
	RunID   int64
}

// Queue is one repo scope's read in a sweep. Complete is false when a page
// was lost or the scope's reads are held back: Jobs is then part of the
// scope's queue at most, and funds nothing new.
type Queue struct {
	Scope    string
	Jobs     []QueuedJob
	Complete bool
}

// Sweep reads every repo scope's queued jobs, one request at a time. It is
// not safe for concurrent use: Run is its one caller.
type Sweep struct {
	rest     *REST
	log      *slog.Logger
	now      func() time.Time
	profiles map[string]string
	memo     map[memoKey]memo
	quotas   map[string]*quota
	failing  map[string]time.Time
	over     map[string]time.Time
	scopes   []*config.Scope
}

// memoKey names a scope's runs list in state, or else run's jobs list.
type memoKey struct {
	scope, state string
	run          int64
}

// memo is a list's last read: its run IDs or its jobs.
type memo struct {
	read, next time.Time
	runs       []int64
	jobs       []QueuedJob
}

// quota is one rate limit's standing (auth.limit): its last rate-limit
// headers, the billed reads of the current sweep, and a hold after a
// rate-limit error, which since marks as standing.
type quota struct {
	reset, since, until time.Time
	backoff             time.Duration
	remaining, limit    int
	billed              int
}

// NewSweep builds the sweep over cfg's repo scopes and profiles.
func NewSweep(cfg *config.Config, r *REST) *Sweep {
	s := &Sweep{
		rest: r, log: r.log, now: time.Now, profiles: map[string]string{}, memo: map[memoKey]memo{},
		quotas: map[string]*quota{}, failing: map[string]time.Time{}, over: map[string]time.Time{},
	}
	for i := range cfg.Scopes {
		s.quotas[r.auth.limit[cfg.Scopes[i].Key]] = &quota{}
	}
	for i := range cfg.Scopes {
		if cfg.Scopes[i].Kind == config.ScopeRepo {
			s.scopes = append(s.scopes, &cfg.Scopes[i])
		}
	}
	// GitHub matches runs-on labels without case.
	for i := range cfg.Profiles {
		s.profiles[strings.ToLower(cfg.Profiles[i].Name)] = cfg.Profiles[i].Name
	}
	return s
}

// Run sweeps until ctx ends, handing each sweep's reads to sink with the
// time the sweep ended. hot reports whether the pool has free budget, which
// reads every started run's jobs on every sweep. Each sweep starts at least
// 10 seconds after the previous one ended, later when a credential's rate
// limit needs it (spacing).
func (s *Sweep) Run(ctx context.Context, hot func() bool, sink func(ended time.Time, qs []Queue)) {
	for ctx.Err() == nil {
		qs := s.sweep(ctx, hot())
		sink(s.now(), qs)
		if sleep(ctx, s.spacing()) != nil {
			return
		}
	}
}

func (s *Sweep) sweep(ctx context.Context, hot bool) []Queue {
	for _, q := range s.quotas {
		q.billed = 0
	}
	out := make([]Queue, 0, len(s.scopes))
	for _, sc := range s.scopes {
		q := Queue{Scope: sc.Key}
		if !s.now().Before(s.quotas[s.rest.auth.limit[sc.Key]].until) {
			jobs, err := s.readScope(ctx, sc, hot)
			q.Jobs, q.Complete = jobs, err == nil
			s.settle(sc, err)
		}
		out = append(out, q)
	}
	return out
}

// settle logs a scope's failing reads as a standing state and stamps a
// whole read.
func (s *Sweep) settle(sc *config.Scope, err error) {
	since, failing := s.failing[sc.Key]
	switch {
	case err == nil && failing:
		s.log.Info("Queue reads recovered", "scope", sc.Key, "duration", s.now().Sub(since).Round(time.Second))
		delete(s.failing, sc.Key)
	case err != nil && !failing && !errors.Is(err, context.Canceled):
		s.log.Warn("Queue reads are failing, so the scope gets no new funding", "scope", sc.Key, "error", err)
		s.failing[sc.Key] = s.now()
	}
	if err == nil {
		s.rest.m.QueueScopeRead(sc.Key, s.now())
	}
}

func (s *Sweep) readScope(ctx context.Context, sc *config.Scope, hot bool) ([]QueuedJob, error) {
	started := map[int64]bool{}
	for _, state := range runStates {
		ids, err := s.runs(ctx, sc, state)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			started[id] = started[id] || state == "in_progress"
		}
	}
	var jobs []QueuedJob
	for _, id := range slices.Sorted(maps.Keys(started)) {
		js, err := s.jobs(ctx, sc, id, hot || !started[id])
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, js...)
	}
	maps.DeleteFunc(s.memo, func(k memoKey, _ memo) bool {
		_, listed := started[k.run]
		return k.scope == sc.Key && k.run != 0 && !listed
	})
	return jobs, nil
}

// runs lists the IDs of a scope's runs in state, newest first, up to
// GitHub's window.
func (s *Sweep) runs(ctx context.Context, sc *config.Scope, state string) ([]int64, error) {
	key := memoKey{scope: sc.Key, state: state}
	if m, ok := s.memo[key]; ok && s.now().Before(m.next) {
		return m.runs, nil
	}
	var page struct {
		Runs []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
		Total int `json:"total_count"`
	}
	m, total := memo{read: s.now()}, 0
	err := s.pages(ctx, sc, accountPath(sc)+"/actions/runs?status="+state+"&per_page=100", maxRunPages, &m, func(body []byte) error {
		page.Runs = nil
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, r := range page.Runs {
			m.runs = append(m.runs, r.ID)
		}
		total = page.Total
		return nil
	})
	// GitHub's window ends a longer list at its bound, but only a full
	// window is that cut: short pages before it would drop runs unseen.
	if errors.Is(err, errPages) && len(m.runs) >= runsWindow {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	s.window(sc, state, total)
	s.memo[key] = m
	return m.runs, nil
}

// window logs a runs list past GitHub's window as a standing state.
func (s *Sweep) window(sc *config.Scope, state string, total int) {
	key := sc.Key + "/" + state
	since, was := s.over[key]
	switch over := total > runsWindow; {
	case over && !was:
		s.log.Warn("Queued runs passed GitHub's list limit of 1,000, so the pool orders this scope by its newest 1,000 runs",
			"scope", sc.Key, "state", state, "runs", total)
		s.over[key] = s.now()
	case !over && was:
		s.log.Info("Queued runs are back inside GitHub's list limit of 1,000", "scope", sc.Key, "state", state, "runs", total,
			"duration", s.now().Sub(since).Round(time.Second))
		delete(s.over, key)
	}
}

// jobs lists run id's queued jobs a profile can take. A run whose last read
// showed none, and that need not be fresh, is read again only after
// coldRead, because a started run's list changes with every step and each
// read is billed.
func (s *Sweep) jobs(ctx context.Context, sc *config.Scope, id int64, fresh bool) ([]QueuedJob, error) {
	key := memoKey{scope: sc.Key, run: id}
	if m, ok := s.memo[key]; ok && (s.now().Before(m.next) || (!fresh && len(m.jobs) == 0 && s.now().Sub(m.read) < coldRead)) {
		return m.jobs, nil
	}
	m := memo{read: s.now()}
	err := s.pages(ctx, sc, accountPath(sc)+"/actions/runs/"+strconv.FormatInt(id, 10)+"/jobs?filter=latest&per_page=100", maxJobPages, &m, func(body []byte) error {
		return s.queued(id, body, &m)
	})
	if err != nil {
		return nil, err
	}
	s.memo[key] = m
	return m.jobs, nil
}

var errNoCreated = errors.New("a queued job carries no created_at")

// queued keeps a jobs page's queued jobs whose only label is a profile's. A
// kept job without created_at fails the page: its zero time would rank it
// oldest in the one queue.
func (s *Sweep) queued(id int64, body []byte, m *memo) error {
	var page struct {
		Jobs []struct {
			Created time.Time `json:"created_at"`
			Name    string    `json:"name"`
			Status  string    `json:"status"`
			Labels  []string  `json:"labels"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return err
	}
	for _, j := range page.Jobs {
		if len(j.Labels) != 1 || j.Status != "queued" {
			continue
		}
		if p, ok := s.profiles[strings.ToLower(j.Labels[0])]; ok {
			if j.Created.IsZero() {
				return errNoCreated
			}
			m.jobs = append(m.jobs, QueuedJob{Created: j.Created, Name: j.Name, Profile: p, RunID: id})
		}
	}
	return nil
}

var errPages = errors.New("github: the list did not end within its page bound")

// pages reads path and each page its Link header names next, up to most
// pages, handing each body to each. The longest x-poll-interval among the
// pages sets m.next. A list still going at its bound fails with errPages.
func (s *Sweep) pages(ctx context.Context, sc *config.Scope, path string, most int, m *memo, each func([]byte) error) error {
	u := s.rest.api + path
	for n := 1; ; n++ {
		a, err := s.read(ctx, sc, u)
		if err != nil {
			return err
		}
		if secs, _ := strconv.Atoi(a.header.Get("X-Poll-Interval")); secs > 0 {
			if t := m.read.Add(min(time.Duration(secs)*time.Second, maxHold)); t.After(m.next) {
				m.next = t
			}
		}
		if derr := each(a.body); derr != nil {
			s.rest.m.GitHubError(obs.GitHubQueue)
			return fmt.Errorf("github: decoding a queue page: %w", derr)
		}
		switch u, err = nextPage(s.rest.api, a.header); {
		case err != nil:
			s.rest.m.GitHubError(obs.GitHubQueue)
			return err
		case u == "":
			return nil
		case n == most:
			return errPages
		}
	}
}

// read sends one queue read through the revalidation cache, counting its
// result and keeping its rate limit's headers. A rate-limit
// error holds every read on that rate limit (holdFor); any other refusal
// fails this scope's read alone.
func (s *Sweep) read(ctx context.Context, sc *config.Scope, u string) (answer, error) {
	src, q := s.rest.auth.source(sc), s.quotas[s.rest.auth.limit[sc.Key]]
	hits := s.rest.cache.State().Hits
	a, err := s.rest.get(ctx, s.rest.cached, src, u)
	if err == nil {
		q.track(a.header)
		if rateLimited(&a) {
			s.holdFor(sc, q, a.header)
		}
		err = a.err()
	}
	if err != nil {
		if s.rest.failure(ctx, obs.GitHubQueue) {
			s.rest.m.QueueRead(obs.ReadError)
		}
		return a, err
	}
	if !q.since.IsZero() {
		s.log.Info("Queue reads resumed after the rate limit", "scope", sc.Key, "wait", s.now().Sub(q.since).Round(time.Second))
		q.since, q.until, q.backoff = time.Time{}, time.Time{}, 0
	}
	if s.rest.cache.State().Hits > hits {
		s.rest.m.QueueRead(obs.ReadUnchanged)
	} else {
		s.rest.m.QueueRead(obs.ReadChanged)
		q.billed++
	}
	return a, nil
}

// holdFor holds q's reads for the answer's Retry-After, else until its
// reset when no request is left, else for a minute doubling on each repeat
// up to 15 minutes. A success ends the hold and its doubling.
func (s *Sweep) holdFor(sc *config.Scope, q *quota, h http.Header) {
	now := s.now()
	wait := httpclient.ParseRetryAfterResponse(&http.Response{Header: h})
	if wait <= 0 && h.Get("X-RateLimit-Remaining") == "0" && !q.reset.IsZero() {
		wait = q.reset.Sub(now)
	}
	if wait <= 0 {
		q.backoff = min(max(2*q.backoff, firstHold), maxBackoff)
		wait = q.backoff
	}
	q.until = now.Add(min(wait, maxHold))
	if q.since.IsZero() {
		q.since = now
		s.log.Warn("GitHub rate limit reached, so queue reads wait until the reset time", "scope", sc.Key, "reset", q.until)
	}
}

// secondaryLimit matches the message of GitHub's secondary rate limit.
var secondaryLimit = regexp.MustCompile(`(?i)\bsecondary rate\b`)

// rateLimited reports whether a is GitHub's rate-limit error: a 429, or a
// 403 with Retry-After, no request left, or the secondary limit's message
// (rate-limit rules, above). A 403 for SSO or a missing permission is none.
func rateLimited(a *answer) bool {
	switch a.status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return a.header.Get("Retry-After") != "" || a.header.Get("X-RateLimit-Remaining") == "0" || secondaryLimit.Match(a.body)
	}
	return false
}

func (q *quota) track(h http.Header) {
	remaining, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	limit, err2 := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	reset, err3 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 == nil && err2 == nil && err3 == nil {
		q.remaining, q.limit, q.reset = remaining, limit, time.Unix(reset, 0)
	}
}

// spacing answers the wait before the next sweep: minSpacing, or longer so
// the billed reads one sweep cost fit before each rate limit's reset in what
// it has left, less a tenth of its limit kept for the other REST reads.
func (s *Sweep) spacing() time.Duration {
	wait, now := minSpacing, s.now()
	for _, q := range s.quotas {
		n, left, until := q.billed, q.remaining-q.limit/10, q.reset.Sub(now)
		switch {
		case n == 0 || until <= 0:
		case left < n:
			wait = max(wait, until)
		default:
			wait = max(wait, until*time.Duration(n)/time.Duration(left))
		}
	}
	return min(wait, maxHold)
}
