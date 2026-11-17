package github

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/config"
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
	// createTimeout bounds one session create, the token requests it sends
	// included.
	createTimeout = time.Minute
)

// createCut and pollCut bound a create's and a poll's context; test seams
// through which a test cuts only the call its fake holds.
var createCut, pollCut = context.WithTimeout, context.WithTimeout

// Stats are a scale set's statistics as GitHub last reported them:
// Available counts its queued jobs no runner acquired yet, and Assigned
// those assigned to it, Running among them.
type Stats struct{ Available, Assigned, Running int }

// EventKind is a job message's kind.
type EventKind int

// The job messages a scale set receives.
const (
	Assigned EventKind = iota + 1
	Started
	Completed
)

// Event is one job message. Name, Runner, Result, Repository,
// WorkflowRef and Trigger are GitHub's text.
type Event struct {
	// Assigned is when GitHub gave the job to the scale set, RunnerAssigned
	// when a runner took it.
	Queued, Assigned, RunnerAssigned, Finished time.Time
	// Name is the job's display name, the REST job's name.
	Name   string
	Runner string
	Result string
	// Repository is owner/name; Trigger is the event that started the
	// workflow run.
	Repository, WorkflowRef, Trigger string
	// RequestID is the runner request, unique per job attempt.
	RequestID int64
	RunID     int64
	Kind      EventKind
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
	// which stops the scale set being served, and owner, the holder GitHub
	// named: empty when held is false or GitHub named none.
	Held(ss ScaleSet, held bool, owner string)
	// Lost reports that GitHub no longer has the scale set: its session
	// create answered not found. Listen returns after it.
	Lost(ss ScaleSet)
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
	failing time.Time
	last    time.Time
	h       Handler
	c       *scaleset.Client
	log     *slog.Logger
	sc      *scopeClient
	s       *ScaleSets
	sess    *scaleset.MessageSessionClient
	owner   string
	ss      ScaleSet
	gen     int
	id      int
	prev    ended
}

// Listen serves the session of ss, a scale set of scope, under owner (the
// pool name, which GitHub names the holder by) until ctx ends, then closes
// it and returns once the close is done; a create answered not found ends
// it at once through Handler.Lost. Every other failure is counted, logged
// and retried. Before each poll, when more than 60 seconds have passed
// since the last answered poll began, or the scope's client was rebuilt
// (Recheck), it replaces the session: a holder whose claim lapsed still
// receives every message beside its successor.
func (s *ScaleSets) Listen(ctx context.Context, scope *config.Scope, ss ScaleSet, owner string, h Handler) {
	l := &listen{s: s, h: h, sc: s.scope(scope), ss: ss, owner: owner, log: s.rest.log.With("scope", ss.Scope, "profile", ss.Profile)}
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
// answers false when ctx ends or the scale set is lost.
func (l *listen) open(ctx context.Context) bool {
	o := opening{since: l.s.now()}
	for {
		sess, err := l.create(ctx)
		if err == nil {
			l.opened(sess, &o)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if errors.Is(err, scaleset.NotFoundError) {
			l.log.Warn("GitHub no longer has the scale set, so the pool ensures it again under its name", "id", l.ss.ID)
			l.h.Lost(l.ss)
			return false
		}
		l.refused(err, &o)
		if l.pause(ctx, err) != nil {
			return false
		}
	}
}

// opened takes the session an open created, ending the states o logged.
func (l *listen) opened(sess *scaleset.MessageSessionClient, o *opening) {
	now := l.s.now()
	if !o.held.IsZero() {
		l.log.Info("The scale set's session is no longer held elsewhere, so it is served again", "duration", now.Sub(o.held).Round(time.Second))
		l.h.Held(l.ss, false, "")
	}
	if o.warned || o.alarmed {
		l.log.Info("The scale set's session opened after failing", "duration", now.Sub(o.since).Round(time.Second))
	}
	l.sess, l.id, l.last = sess, 0, now
	l.h.Opened(l.ss, *cmp.Or(statsOf(sess.Session().Statistics), &Stats{}))
}

// create opens a session on the loop's own client, built again when the
// scope's installation changed. The client is never shared: scaleset holds
// a client's token lock across the token requests and their retries,
// ignoring ctx (https://github.com/actions/scaleset/blob/v0.5.0/client.go#L1083),
// so a shared one makes a create or close wait out another caller's refresh.
func (l *listen) create(ctx context.Context) (*scaleset.MessageSessionClient, error) {
	ctx, cancel := createCut(scaleset.WithRequestRetry(ctx, noRetry), createTimeout)
	defer cancel()
	if _, err := l.s.client(ctx, l.sc); err != nil {
		return nil, err
	}
	if _, id, gen := l.sc.get(); l.c == nil || gen != l.gen {
		c, err := l.s.build(l.ss.Scope, id)
		if err != nil {
			return nil, err
		}
		l.c, l.gen = c, gen
	}
	sess, err := l.c.MessageSessionClient(ctx, l.ss.ID, l.owner)
	if err != nil {
		l.s.rest.failure(ctx, OpSession)
	}
	return sess, err
}

// unanswered reports whether a failed session request may have made a
// session. An answered 4xx made none; scaleset v0.5.0 types only some, so
// the rest are read from the status its error text carries first
// (https://github.com/actions/scaleset/blob/v0.5.0/errors.go#L190).
func unanswered(err error) bool {
	var rl *scaleset.RateLimitError
	if errors.As(err, &rl) || errors.Is(err, scaleset.UnauthorizedError) || errors.Is(err, scaleset.BadRequestError) {
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
		owner := holderOf(err)
		l.log.Error("Another process holds the scale set's session, so the pool stops serving it", "owner", owner)
		l.h.Held(l.ss, true, owner)
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

// holderOf reads the owner a session conflict's message names ("… already
// has an active session for owner <owner>.") as one bounded line, empty
// when it names none.
func holderOf(err error) string {
	_, owner, found := strings.Cut(err.Error(), "active session for owner ")
	if !found {
		return ""
	}
	return textsafe.SanitizeSingleLineBounded(strings.TrimSuffix(strings.TrimSpace(owner), "."), 64)
}

var errLate = errors.New("the poll ran past 60 seconds, so its answer is dropped and the session replaced")

// noRetry turns scaleset's own retries off for a poll and a create, whose
// default waits out GitHub's Retry-After in full, up to 4 times, inside
// the one call and past a poll's 60-second gap. Listen retries them itself
// through pause, a poll after its gap check.
func noRetry(c *scaleset.RetryConfig) { c.Max = 0 }

// poll sends one poll and hands its answer over, then acks its message. A
// poll held past maxGap is cut off, and an answer that comes back past it
// is dropped unread, because the claim may have lapsed while it was open.
func (l *listen) poll(ctx context.Context, start time.Time) {
	pctx, cancel := pollCut(scaleset.WithRequestRetry(ctx, noRetry), maxGap)
	msg, err := l.sess.GetMessage(pctx, l.id, l.h.Capacity(l.ss, start))
	end := l.s.now()
	late := pctx.Err() != nil || end.Sub(start) > maxGap
	cancel()
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
	if err != nil {
		l.s.rest.rec.GitHubError(OpPoll)
		l.log.Warn("Acking a message failed", "error", failed("ack", err))
	}
}

// pollFailed counts a failed poll, logs failing polls as a standing state,
// and then closes a late poll's session at once, or pauses.
func (l *listen) pollFailed(ctx context.Context, err error, late bool) {
	l.s.rest.rec.GitHubError(OpPoll)
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
// (https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api);
// anything else 5 seconds. It fails only when ctx ends.
func (l *listen) pause(ctx context.Context, err error) error {
	var rl *scaleset.RateLimitError
	if !errors.As(err, &rl) {
		return sleep(ctx, retryEvery)
	}
	return sleep(ctx, min(max(time.Minute, rl.RetryAfter), maxHold))
}

func statsOf(st *scaleset.RunnerScaleSetStatistic) *Stats {
	if st == nil {
		return nil
	}
	return &Stats{Available: st.TotalAvailableJobs, Assigned: st.TotalAssignedJobs, Running: st.TotalRunningJobs}
}

func event(k EventKind, b *scaleset.JobMessageBase, runner, result string) Event {
	return Event{
		Kind: k, RequestID: b.RunnerRequestID, RunID: b.WorkflowRunID, Name: b.JobDisplayName, Runner: runner, Result: result,
		Repository: b.OwnerName + "/" + b.RepositoryName, WorkflowRef: b.JobWorkflowRef, Trigger: b.EventName,
		Queued: b.QueueTime, Assigned: b.ScaleSetAssignTime, RunnerAssigned: b.RunnerAssignTime, Finished: b.FinishTime,
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
	switch {
	case err == nil:
		return closed
	case errors.Is(err, scaleset.BadRequestError) && strings.Contains(err.Error(), "RunnerScaleSetSessionExpiredException"):
		l.log.Warn("The scale set's session had already lapsed on GitHub, so it counts as closed")
		return closed
	}
	l.s.rest.rec.GitHubError(OpSession)
	l.log.Warn("Closing the scale set's session failed, so a conflict counts as its own claim until that lapses", "error", failed("close", err))
	return unclosed
}
