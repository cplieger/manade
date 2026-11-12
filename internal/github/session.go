package github

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/cplieger/manade/internal/obs"
)

// Session timing, from what github.com was seen to do: a claim whose holder
// stops lapses about 70 seconds after its last poll began (75 at most), and
// an idle poll returns after about 50.
const (
	retryEvery    = 5 * time.Second
	conflictAlarm = 2 * time.Minute
	lapse         = 75 * time.Second
	maxGap        = 60 * time.Second
	closeTimeout  = 10 * time.Second
)

// Stats are a scale set's statistics as GitHub last reported them.
type Stats struct{ Assigned, Running int }

// EventKind is a job message's kind.
type EventKind int

// The job messages a scale set receives.
const (
	Assigned EventKind = iota + 1
	Started
	Completed
)

// Event is one job message. Name, Runner and Result are GitHub's text.
type Event struct {
	Queued, RunnerAssigned, Finished time.Time
	// Name is the job's display name, the REST job's name.
	Name   string
	Runner string
	Result string
	RunID  int64
	Kind   EventKind
}

// Poll is one poll GitHub answered. Stats is nil when the answer carried
// none, an idle poll's included. A message's events come grouped by kind in
// the order of EventKind, because scaleset parses them so.
type Poll struct {
	Started, Ended time.Time
	Stats          *Stats
	Events         []Event
	Idle           bool
}

// Handler is the pool's side of one scale set's session. Listen calls it
// from that scale set's goroutine, and every call must return at once: the
// next poll waits on it.
type Handler interface {
	// Capacity answers the maxCapacity the poll beginning at start reports.
	Capacity(ss ScaleSet, start time.Time) int
	// Opened hands over a new session's initial statistics, zero when GitHub
	// sent none, before its first poll.
	Opened(ss ScaleSet, st Stats)
	// Polled hands over an answered poll, before its message is acked. A
	// poll answered more than 60 seconds after it began never arrives.
	Polled(ss ScaleSet, p *Poll)
	// Held reports whether another process holds the scale set's session,
	// which stops the scale set being served.
	Held(ss ScaleSet, held bool)
	// Closed reports that the scale set's session ended, closed or not,
	// before any new one opens.
	Closed(ss ScaleSet)
	// Beat marks the end of each session create, poll, ack and close,
	// answered or not, and each 5 seconds of a rate limit's wait. Two beats
	// are at most 5 seconds and one such call apart; a poll or a create
	// lasts at most 60 seconds, an ack or a close 10.
	Beat(ss ScaleSet)
}

// ended is how a scale set's previous session ended, which decides what a
// conflict on the next create means.
type ended int

const (
	booting  ended = iota // no session yet: a conflict is a crashed predecessor's claim
	closed                // closed: a conflict is another process
	unclosed              // the close failed: a conflict may be this session's own claim
)

// listen is one scale set's session loop. last is when the last answered
// poll began, or the session was created; failing is when polls began to
// fail, zero while they answer. c is the loop's own client, built for the
// scope's client generation gen.
type listen struct {
	last    time.Time
	failing time.Time
	s       *ScaleSets
	log     *slog.Logger
	h       Handler
	sc      *scopeClient
	c       *scaleset.Client
	sess    *scaleset.MessageSessionClient
	ss      ScaleSet
	gen     int
	id      int
	prev    ended
}

// Listen serves ss's session until ctx ends, then closes it, and returns
// once the close is done. Every failure is counted, logged and retried.
// Before each poll, when more than 60 seconds have passed since the last
// answered poll began, or the scope's client was rebuilt (Recheck), it
// replaces the session: a holder whose claim lapsed still receives every
// message beside its successor.
func (s *ScaleSets) Listen(ctx context.Context, ss ScaleSet, h Handler) {
	l := &listen{s: s, h: h, sc: s.byKey[ss.Scope], ss: ss, log: s.rest.log.With("scope", ss.Scope, "profile", ss.Profile)}
	defer l.close(ctx)
	for ctx.Err() == nil {
		if l.sess == nil && !l.open(ctx) {
			return
		}
		start := s.now()
		if _, _, gen := l.sc.get(); gen != l.gen || start.Sub(l.last) > maxGap {
			l.prev = l.close(ctx)
			continue
		}
		l.poll(ctx, start)
	}
}

// opening is what one open has logged and reported so far: since is its
// first attempt, held when the scale set was found held, zero until then,
// and doubt when the last create that may have made a session GitHub never
// answered for ended.
type opening struct {
	since, held, doubt time.Time
	alarmed, warned    bool
}

// open creates the session, retrying after each refusal (pause), and
// answers false only when ctx ends.
func (l *listen) open(ctx context.Context) bool {
	o := opening{since: l.s.now()}
	for {
		sess, err := l.create(ctx)
		l.h.Beat(l.ss)
		if err == nil {
			now := l.s.now()
			if !o.held.IsZero() {
				l.log.Info("The scale set's session is no longer held elsewhere, so it is served again", "duration", now.Sub(o.held).Round(time.Second))
				l.h.Held(l.ss, false)
			}
			if o.warned || o.alarmed {
				l.log.Info("The scale set's session opened after failing", "duration", now.Sub(o.since).Round(time.Second))
			}
			l.sess, l.id, l.last = sess, 0, now
			l.h.Opened(l.ss, *cmp.Or(statsOf(sess.Session().Statistics), &Stats{}))
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		l.refused(err, &o)
		if l.pause(ctx, err) != nil {
			return false
		}
	}
}

// createTimeout bounds one session create, the token requests it sends
// included; a test seam.
var createTimeout = time.Minute

// create opens a session on the loop's own client, built again when the
// scope's installation changed. The client is never shared: scaleset holds
// a client's token lock across the token requests and their retries,
// ignoring ctx (https://github.com/actions/scaleset/blob/v0.5.0/client.go#L1083),
// so a shared one makes a create or close wait out another caller's refresh.
func (l *listen) create(ctx context.Context) (*scaleset.MessageSessionClient, error) {
	ctx, cancel := context.WithTimeout(scaleset.WithRequestRetry(ctx, noRetry), createTimeout)
	defer cancel()
	if _, err := l.s.client(ctx, l.sc); err != nil {
		return nil, err
	}
	if _, id, gen := l.sc.get(); l.c == nil || gen != l.gen {
		c, err := l.s.build(l.sc.scope, id)
		if err != nil {
			return nil, err
		}
		l.c, l.gen = c, gen
	}
	sess, err := l.c.MessageSessionClient(ctx, l.ss.ID, l.s.cfg.PoolName)
	if err != nil {
		l.s.rest.failure(ctx, obs.GitHubSession)
	}
	return sess, err
}

// unanswered reports whether a failed session request may have made a
// session. An answered 4xx made none; scaleset v0.5.0 types only some, so
// the rest are read from the status its error text carries first
// (https://github.com/actions/scaleset/blob/v0.5.0/errors.go#L190).
func unanswered(err error) bool {
	var rl *scaleset.RateLimitError
	if errors.As(err, &rl) || errors.Is(err, scaleset.UnauthorizedError) ||
		errors.Is(err, scaleset.BadRequestError) || errors.Is(err, scaleset.NotFoundError) {
		return false
	}
	_, status, found := strings.Cut(err.Error(), `(status="`)
	return !found || !strings.HasPrefix(status, "4")
}

// refused decides what a failed create means. A conflict is another process
// at once after a counted close; after a failed close or a create that may
// have left this pool's own claim, only once it outlasts the lapse; and
// never at boot.
func (l *listen) refused(err error, o *opening) {
	conflict, now := errors.Is(err, scaleset.ConflictError), l.s.now()
	// A failed create's possible claim dates from doubt, not since.
	own, from := l.prev == unclosed || !o.doubt.IsZero(), cmp.Or(o.doubt, o.since)
	switch {
	case conflict && o.held.IsZero() && l.prev != booting && (!own || now.Sub(from) > lapse):
		o.held = now
		l.log.Error("Another process holds the scale set's session, so the pool stops serving it")
		l.h.Held(l.ss, true)
	case conflict && l.prev == booting && !o.alarmed && now.Sub(o.since) >= conflictAlarm:
		o.alarmed = true
		l.log.Error("The scale set's session is still held elsewhere after 2 minutes, so another process may be serving it")
	case !conflict && !o.warned:
		o.warned = true
		l.log.Warn("Creating the scale set's session failed, so it is retried every 5 seconds, or once GitHub's rate limit allows",
			"error", failed("session", err))
	}
	// A claim the create made dates from its request, which ended by now.
	if !conflict && unanswered(err) {
		o.doubt = now
	}
}

var errLate = errors.New("the poll ran past 60 seconds, so its answer is dropped and the session replaced")

// pollCut is when a poll's context ends, a test seam over maxGap.
var pollCut = maxGap

// noRetry turns scaleset's own retries off for a poll and a create, whose
// default waits out GitHub's Retry-After in full, up to 4 times, with no
// beat. Listen retries them itself through pause, a poll after its gap
// check.
func noRetry(c *scaleset.RetryConfig) { c.Max = 0 }

// poll sends one poll and hands its answer over, then acks its message. A
// poll held past maxGap is cut off, and an answer that comes back past it
// is dropped unread, because the claim may have lapsed while it was open.
func (l *listen) poll(ctx context.Context, start time.Time) {
	pctx, cancel := context.WithTimeout(scaleset.WithRequestRetry(ctx, noRetry), pollCut)
	msg, err := l.sess.GetMessage(pctx, l.id, l.h.Capacity(l.ss, start))
	end := l.s.now()
	late := pctx.Err() != nil || end.Sub(start) > maxGap
	cancel()
	l.h.Beat(l.ss)
	if ctx.Err() != nil {
		return
	}
	if err == nil && late {
		err = errLate
	}
	if err != nil {
		l.pollFailed(ctx, err, late)
		return
	}
	if !l.failing.IsZero() {
		l.log.Info("Polls recovered", "duration", end.Sub(l.failing).Round(time.Second))
		l.failing = time.Time{}
	}
	l.last = start
	l.s.rest.m.PollOK(l.ss.ScaleSet, end)
	p := Poll{Started: start, Ended: end, Idle: msg == nil}
	if msg == nil {
		l.h.Polled(l.ss, &p)
		return
	}
	p.Stats = statsOf(msg.Statistics)
	for _, j := range msg.JobAssignedMessages {
		p.Events = append(p.Events, event(Assigned, &j.JobMessageBase, "", ""))
	}
	for _, j := range msg.JobStartedMessages {
		p.Events = append(p.Events, event(Started, &j.JobMessageBase, j.RunnerName, ""))
	}
	for _, j := range msg.JobCompletedMessages {
		p.Events = append(p.Events, event(Completed, &j.JobMessageBase, j.RunnerName, j.Result))
	}
	l.h.Polled(l.ss, &p)
	l.id = msg.MessageID
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	err = l.sess.DeleteMessage(actx, msg.MessageID)
	l.h.Beat(l.ss)
	if err != nil {
		l.s.rest.m.GitHubError(obs.GitHubPoll)
		l.log.Warn("Acking a message failed", "error", failed("ack", err))
	}
}

// pollFailed counts a failed poll, logs failing polls as a standing state,
// and then closes a late poll's session at once, or pauses.
func (l *listen) pollFailed(ctx context.Context, err error, late bool) {
	l.s.rest.m.GitHubError(obs.GitHubPoll)
	if l.failing.IsZero() {
		l.failing = l.s.now()
		l.log.Warn("Polls are failing, so the pool sees none of the scale set's job messages", "error", failed("poll", err))
	}
	if late {
		l.prev = l.close(ctx)
		return
	}
	_ = l.pause(ctx, err)
}

// pause waits after a refused create or poll: a rate limit's Retry-After,
// at least a minute and at most maxHold
// (https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api),
// in 5-second steps that each beat; anything else 5 seconds. It fails only
// when ctx ends.
func (l *listen) pause(ctx context.Context, err error) error {
	var rl *scaleset.RateLimitError
	if !errors.As(err, &rl) {
		return sleep(ctx, retryEvery)
	}
	for d := min(max(time.Minute, rl.RetryAfter), maxHold); d > 0; d -= retryEvery {
		if err := sleep(ctx, min(d, retryEvery)); err != nil {
			return err
		}
		l.h.Beat(l.ss)
	}
	return nil
}

func statsOf(st *scaleset.RunnerScaleSetStatistic) *Stats {
	if st == nil {
		return nil
	}
	return &Stats{Assigned: st.TotalAssignedJobs, Running: st.TotalRunningJobs}
}

func event(k EventKind, b *scaleset.JobMessageBase, runner, result string) Event {
	return Event{
		Kind: k, RunID: b.WorkflowRunID, Name: b.JobDisplayName, Runner: runner, Result: result,
		Queued: b.QueueTime, RunnerAssigned: b.RunnerAssignTime, Finished: b.FinishTime,
	}
}

// close closes the session, if any, and answers how it ended. A close
// answered 400 because the session already lapsed counts as closed.
func (l *listen) close(ctx context.Context) ended {
	if l.sess == nil {
		return l.prev
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	err := l.sess.Close(ctx)
	l.sess = nil
	l.h.Closed(l.ss)
	l.h.Beat(l.ss)
	switch {
	case err == nil:
		return closed
	case errors.Is(err, scaleset.BadRequestError) && strings.Contains(err.Error(), "RunnerScaleSetSessionExpiredException"):
		l.log.Warn("The scale set's session had already lapsed on GitHub, so it counts as closed")
		return closed
	}
	l.s.rest.m.GitHubError(obs.GitHubSession)
	l.log.Warn("Closing the scale set's session failed, so a conflict counts as its own claim until that lapses", "error", failed("close", err))
	return unclosed
}
