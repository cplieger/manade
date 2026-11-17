package pool

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

const (
	reconcileEvery = 30 * time.Second
	// tailLines is how much of an unclean exit's log its run keeps.
	tailLines = 200
	// eventsRetry spaces reconnects to a failed events stream; the pass
	// every reconcileEvery catches the exits between.
	eventsRetry = 5 * time.Second
)

// errDisconnected is a call to a host with no open connection.
var errDisconnected = errors.New("pool: the host is not connected")

// disconnected is the API of a host with no open connection: every call
// fails at once.
type disconnected struct{}

func (disconnected) Status(context.Context) (hostapi.Status, error) {
	return hostapi.Status{}, errDisconnected
}

func (disconnected) CreateRunner(context.Context, *hostapi.RunnerRequest) (hostapi.RunnerAnswer, error) {
	return hostapi.RunnerAnswer{}, errDisconnected
}

func (disconnected) Runners(context.Context) (hostapi.RunnerList, error) {
	return hostapi.RunnerList{}, errDisconnected
}

func (disconnected) DeleteRunner(context.Context, string, bool) ([]byte, error) {
	return nil, errDisconnected
}

func (disconnected) Logs(context.Context, string, int) ([]byte, error) { return nil, errDisconnected }

func (disconnected) Probe(context.Context, time.Duration) (hostapi.ProbeReport, error) {
	return hostapi.ProbeReport{}, errDisconnected
}

func (disconnected) Events(context.Context, func(*hostapi.Event)) error { return errDisconnected }

// api answers host i's API over its open connection, read at each call so
// a reconnection is used at once.
func (g *generation) api(i int) HostAPI {
	if a, ok := g.d.Links.Host(g.hosts[i].cfg.Name); ok {
		return a
	}
	return disconnected{}
}

// runReconciler is host i's reconcile loop: a pass every reconcileEvery
// and after each of its runner events, one at a time, until stop closes.
// Each host has its own loop, so a slow host holds back no other host's
// exits.
func (g *generation) runReconciler(ctx context.Context, stop <-chan struct{}, i int) {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-g.hosts[i].wake:
		}
		// A wake and the stop can both be ready, and select picks either.
		select {
		case <-stop:
			return
		default:
		}
		g.passHost(ctx, i)
	}
}

// runStep runs the pool-wide step every reconcileEvery until ctx ends.
func (g *generation) runStep(ctx context.Context) {
	scheduler.RunLoop(ctx, g.step, scheduler.LoopOptions{Interval: reconcileEvery})
}

// wakeAll asks every host's loop for a pass.
func (g *generation) wakeAll() {
	for _, h := range g.hosts {
		wake(h.wake)
	}
}

// watchEvents wakes host i's loop at each of its runner events until ctx
// ends, following the host's connection as it is replaced.
func (g *generation) watchEvents(ctx context.Context, i int) {
	failing := false
	for {
		err := g.api(i).Events(ctx, func(ev *hostapi.Event) {
			failing = false
			if ev.Type == hostapi.EventRunner {
				wake(g.hosts[i].wake)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, errDisconnected) {
			g.d.Metrics.HostLinkError(g.hosts[i].cfg.Name)
			if !failing {
				g.log.Warn("A host's events stream failed, so its exits wait for the next pass", "host", g.hosts[i].cfg.Name, "error", err)
				failing = true
			}
		}
		t := time.NewTimer(eventsRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// pass is a pass of every host at once, then the pool-wide step.
func (g *generation) pass(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range g.hosts {
		wg.Go(func() { g.passHost(ctx, i) })
	}
	wg.Wait()
	// Every host's pass has just returned, so each loop beats now; a host
	// that returned early would otherwise read as wedged behind a slow one.
	g.mu.Lock()
	now := g.now()
	for _, h := range g.hosts {
		h.beat = now
	}
	g.mu.Unlock()
	g.step(ctx)
}

// passHost is one pass of host i. A host that does not answer does not
// fail it.
func (g *generation) passHost(ctx context.Context, i int) {
	h := g.hosts[i]
	g.mu.Lock()
	h.beat = g.now()
	g.mu.Unlock()
	g.reconcileHost(ctx, i)
	g.mu.Lock()
	h.beat = g.now()
	g.gate()
	g.mu.Unlock()
	g.poke()
}

// step is the pool-wide part of reconciling, which calls no host: the pass
// record from each host loop's lease, then the registration removals still
// owed. Those stop at reconcileEvery, so a slow GitHub never holds the
// next record back.
func (g *generation) step(ctx context.Context) {
	g.mu.Lock()
	g.gate()
	g.mu.Unlock()
	g.d.Metrics.Pass(g.health().Reconciling)
	g.settle()
	uctx, cancel := context.WithTimeout(ctx, reconcileEvery)
	defer cancel()
	g.unregisterPending(uctx)
}

// await runs fn, a call to host i or to GitHub, with host i's loop marked
// as waiting on it. Every such call carries its own bound, so only the time
// between calls counts against the loop's lease.
func (g *generation) await(i int, fn func()) {
	h := g.hosts[i]
	g.mu.Lock()
	h.waiting = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		h.waiting, h.beat = false, g.now()
		g.mu.Unlock()
	}()
	fn()
}

// reconcileHost reads host i's status and runners and acts on each runner.
// The host is settled once a pass adopted or removed every runner its list
// carries that no record held.
func (g *generation) reconcileHost(ctx context.Context, i int) {
	h, api := g.hosts[i], g.api(i)
	var st hostapi.Status
	var err error
	g.await(i, func() { st, err = api.Status(ctx) })
	if err != nil {
		g.down(ctx, i, err)
		return
	}
	g.mu.Lock()
	h.seq++
	begin := h.seq
	g.mu.Unlock()
	var list hostapi.RunnerList
	g.await(i, func() { list, err = api.Runners(ctx) })
	if err != nil {
		g.down(ctx, i, err)
		return
	}
	if g.reg.firstList(h.cfg.Name) {
		names := make([]string, len(list.Runners))
		for j := range list.Runners {
			names[j] = list.Runners[j].Name
		}
		g.await(i, func() { g.d.Runs.Listed(ctx, h.cfg.Name, names) })
	}
	g.mu.Lock()
	g.applyStatus(i, &st)
	acts := g.plan(i, begin, &list)
	g.mu.Unlock()
	settled := true
	for _, a := range acts {
		if ctx.Err() != nil {
			return
		}
		ok := true
		g.await(i, func() { ok = g.act(ctx, i, a) })
		settled = ok && settled
	}
	g.mu.Lock()
	was := h.serving(g.now())
	h.settled = settled
	g.served(i, was)
	g.mu.Unlock()
}

func (g *generation) down(ctx context.Context, i int, err error) {
	if ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	g.unreachable(i, err)
	g.mu.Unlock()
}

// actKind is what a pass does to one runner.
type actKind int

const (
	// actClose removes an ended runner's container and closes its record.
	actClose actKind = iota
	// actFail removes a failed start's registration, entry and orphans.
	actFail
	// actGone closes a record its host no longer holds.
	actGone
	// actDropped closes an unlisted record whose end the pool decided, or
	// one its own supervisor's list misses.
	actDropped
	// actRepost sends an unanswered POST again.
	actRepost
	// actRetire removes an idle runner: its registration, then its container.
	actRetire
	// actExpire removes a runner past its profile's max_lifetime.
	actExpire
	// actAdopt takes a runner no record holds.
	actAdopt
	// actStray removes an ended runner of no scale set the settings hold.
	actStray
)

type action struct {
	b    *book
	r    *runner
	inst string
	// reason is how the record closes.
	reason ExitReason
	lr     hostapi.Runner
	begin  uint64
	kind   actKind
}

// plan sorts host i's runner list, begun at begin, into the pass's
// actions; g.mu is held. A runner is judged only from a list begun after
// its POST was answered, and an absent one is gone only when the list
// comes from another supervisor than the one that held its record.
func (g *generation) plan(i int, begin uint64, list *hostapi.RunnerList) []*action {
	spare := g.spares()
	var acts []*action
	listed := map[string]bool{}
	for k := range list.Runners {
		lr := &list.Runners[k]
		listed[lr.Name] = true
		if a := g.planListed(i, lr, list.InstanceID, begin, spare); a != nil {
			acts = append(acts, a)
		}
	}
	acts = append(acts, g.planUnlisted(i, listed, list.InstanceID, begin)...)
	for name, st := range g.l.strays {
		if st.host == i && !listed[name] {
			g.l.release(name, g.now())
		}
	}
	return acts
}

// spares answers the runners the head reserve wants retired; g.mu is held.
func (g *generation) spares() map[*runner]bool {
	out := map[*runner]bool{}
	for b, names := range g.l.retire() {
		for _, name := range names {
			out[b.runners[name]] = true
		}
	}
	return out
}

// planUnlisted plans each record on host i whose runner the list does not
// carry; g.mu is held.
func (g *generation) planUnlisted(i int, listed map[string]bool, inst string, begin uint64) []*action {
	var acts []*action
	for _, b := range g.l.books {
		for _, r := range b.runners {
			if r.host != i || listed[r.name] || r.posting || r.id == 0 {
				continue
			}
			if a := unlisted(b, r, inst, begin); a != nil {
				acts = append(acts, a)
			}
		}
	}
	return acts
}

// unlisted decides what a pass does to a record whose runner a list from
// supervisor inst, begun at begin, does not carry. An unanswered POST is
// sent again to the supervisor it was sent to. Missed by a list begun
// after the answer, a record whose end the pool decided closes for that
// reason from any supervisor's list. Any other is gone from another
// supervisor's list, and closes as removed when its own supervisor's list
// misses it.
func unlisted(b *book, r *runner, inst string, begin uint64) *action {
	switch {
	case r.answered == 0 && inst == r.instance:
		r.posting = true
		return &action{kind: actRepost, b: b, r: r}
	case r.answered != 0 && begin <= r.answered:
		return nil
	case r.answered == 0 || r.closing == "" && !r.failed && inst != r.instance:
		return &action{kind: actGone, b: b, r: r, reason: gone(r)}
	}
	return &action{kind: actDropped, b: b, r: r, reason: cmp.Or(r.closing, ExitRemoved)}
}

// planListed decides what a pass does to runner lr host i lists; g.mu is
// held.
func (g *generation) planListed(i int, lr *hostapi.Runner, inst string, begin uint64, spare map[*runner]bool) *action {
	b := g.bookOf(lr.Scope, lr.Profile)
	var r *runner
	if b != nil {
		r = b.runners[lr.Name]
	}
	switch {
	case r == nil:
		return g.planUnheld(i, b, lr, inst, begin)
	case r.host != i || r.posting:
		return nil
	}
	if r.answered == 0 {
		r.answered, r.req = begin, nil
	}
	if lr.State == hostapi.StateExited {
		// Judged against the supervisor that held the record, before inst
		// takes it over, and kept for every later pass.
		r.closing = exitReason(r, lr, inst)
	}
	r.instance = inst
	switch lr.State {
	case hostapi.StateExited:
		return &action{kind: actClose, b: b, r: r, lr: *lr, reason: r.closing}
	case hostapi.StateFailed:
		return &action{kind: actFail, b: b, r: r, lr: *lr}
	case hostapi.StateRunning:
		return g.judgeRunning(b, r, spare[r])
	}
	// The host's DELETE waits out a create in flight.
	if g.sets[b].scope.public {
		return &action{kind: actRetire, b: b, r: r}
	}
	return nil
}

// planUnheld decides what a pass does to a listed runner no record holds:
// a new one is adopted, a stray a generation change left is judged as an
// adoption once it runs, and a stray is removed once it ended or passed
// its lifetime; g.mu is held.
func (g *generation) planUnheld(i int, b *book, lr *hostapi.Runner, inst string, begin uint64) *action {
	st, held := g.l.strays[lr.Name]
	switch {
	case !held:
		return &action{kind: actAdopt, b: b, lr: *lr, inst: inst, begin: begin}
	case lr.State == hostapi.StateExited || lr.State == hostapi.StateFailed:
		return &action{kind: actStray, lr: *lr}
	case st.recheck && lr.State == hostapi.StateRunning:
		return &action{kind: actAdopt, b: b, lr: *lr, inst: inst, begin: begin}
	case g.now().Sub(st.created) > g.lifetime(st.profile):
		g.log.Warn("A runner of no scale set the settings hold passed its max_lifetime, so it is removed", "host", g.hosts[i].cfg.Name,
			"scope", lr.Scope, "profile", lr.Profile, "runner", lr.Name)
		return &action{kind: actStray, lr: *lr}
	}
	return nil
}

// judgeRunning decides what a pass does to a running runner r holds;
// g.mu is held. A stale runner was built for a profile spec a later
// generation changed, and one on a drained host may take no new job, so
// either is retired once idle.
func (g *generation) judgeRunning(b *book, r *runner, spare bool) *action {
	if r.state == starting {
		r.state = idle
	}
	if !r.counted {
		r.counted = true
		g.d.Metrics.RunnerStarted(g.hosts[r.host].cfg.Name, b.ss, true)
	}
	pr, now := &g.cfg.Profiles[b.profile], g.now()
	switch {
	case r.closing == ExitLifetime || now.Sub(r.created) > pr.MaxLifetime:
		return &action{kind: actExpire, b: b, r: r}
	case r.state == busy:
		return nil
	case r.closing == ExitIdle || spare || r.stale || g.stopping || g.sets[b].scope.public || !g.hosts[r.host].cfg.Accepting ||
		now.Sub(r.created) > pr.IdleTimeout:
		return &action{kind: actRetire, b: b, r: r}
	}
	return nil
}

// lifetime is profile's max_lifetime, the default for -1, a profile the
// settings no longer hold.
func (g *generation) lifetime(profile int) time.Duration {
	if profile < 0 {
		return config.DefaultMaxLifetime
	}
	return g.cfg.Profiles[profile].MaxLifetime
}

// stopExitCode is a runner's exit code after its host's SIGTERM stop
// (128 + 15); a crash leaves crashExitCode.
const (
	stopExitCode  = 143
	crashExitCode = 255
)

// exitReason is how an exited runner's record closes.
func exitReason(r *runner, lr *hostapi.Runner, inst string) ExitReason {
	switch {
	case r.closing != "":
		return r.closing
	case lr.OOMKilled:
		return ExitOOM
	case inst != r.instance && r.state == busy && !r.completed && (lr.ExitCode == nil || *lr.ExitCode != stopExitCode):
		return ExitLost
	case lr.RemovedBy != "" || lr.ExitCode == nil:
		return ExitRemoved
	case *lr.ExitCode != 0:
		return ExitFailed
	}
	return ExitCompleted
}

// recordlessExit is how an exited runner no record holds counts, judged
// by its exit code alone.
func recordlessExit(lr *hostapi.Runner) ExitReason {
	switch {
	case lr.OOMKilled:
		return ExitOOM
	case lr.RemovedBy != "" || lr.ExitCode == nil:
		return ExitRemoved
	case *lr.ExitCode == crashExitCode:
		return ExitLost
	case *lr.ExitCode != 0:
		return ExitFailed
	}
	return ExitCompleted
}

// gone is how a record whose end the pool did not decide closes when its
// host's new supervisor does not list it: lost once it started, a failed
// start before.
func gone(r *runner) ExitReason {
	if r.counted {
		return ExitLost
	}
	return ExitFailed
}

// act runs one action; it reports false for an adoption whose reads did
// not answer.
func (g *generation) act(ctx context.Context, i int, a *action) bool {
	switch a.kind {
	case actClose:
		g.closeExited(ctx, a)
	case actFail:
		g.failStart(ctx, a)
	case actGone:
		g.closeGone(ctx, a)
	case actDropped:
		g.closeDropped(ctx, a)
	case actRepost:
		g.post(ctx, a.b, a.r)
	case actRetire:
		g.retire(ctx, a)
	case actExpire:
		g.expire(ctx, a)
	case actAdopt:
		return g.adopt(ctx, i, a)
	case actStray:
		g.endStray(ctx, i, &a.lr)
	}
	return true
}

// closeExited removes the container, with an unclean exit's evidence, then
// closes the record, removing the registration when no JobCompleted named
// the runner.
func (g *generation) closeExited(ctx context.Context, a *action) {
	h := a.r.host
	ev, ok := g.endContainer(ctx, h, a.r.name, a.reason, a.lr.ExitCode)
	if !ok {
		return
	}
	g.closeRecord(ctx, a.b, a.r, a.reason, a.reason != ExitIdle, &ev)
	code := -1
	if a.lr.ExitCode != nil {
		code = *a.lr.ExitCode
	}
	level := slog.LevelInfo
	if unclean(a.reason) {
		level = slog.LevelWarn
	}
	g.log.Log(ctx, level, "A runner ended", "host", g.hosts[h].cfg.Name, "scope", a.b.ss.Scope, "profile", a.b.ss.Profile, "runner", a.r.name,
		"reason", string(a.reason), "exit_code", code, "oom", a.lr.OOMKilled, "duration", a.lr.ExitedAt.Sub(a.lr.StartedAt).Round(time.Second).String())
}

// failStart removes a failed start's registration, then its entry with
// any orphan; its slot frees at the DELETE's answer.
func (g *generation) failStart(ctx context.Context, a *action) {
	h := a.r.host
	g.mu.Lock()
	first := !a.r.counted
	a.r.counted, a.r.failed = true, true
	g.mu.Unlock()
	if first {
		g.d.Metrics.RunnerStarted(g.hosts[h].cfg.Name, a.b.ss, false)
		g.log.Warn("A host failed a runner's start, so its registration and entry are removed", "host", g.hosts[h].cfg.Name,
			"scope", a.b.ss.Scope, "profile", a.b.ss.Profile, "runner", a.r.name, "reason", textsafe.SanitizeSingleLineBounded(a.lr.Reason, 512))
	}
	g.unregister(ctx, a.b.ss.Scope, a.r.name, a.r.id)
	if !g.remove(ctx, h, a.r.name) {
		return
	}
	g.mu.Lock()
	if a.b.runners[a.r.name] == a.r {
		g.l.end(a.b, a.r.name, g.now())
	}
	g.mu.Unlock()
}

// closeGone closes a record the host's new supervisor does not hold.
func (g *generation) closeGone(ctx context.Context, a *action) {
	name := g.hosts[a.r.host].cfg.Name
	if a.reason == ExitFailed {
		g.mu.Lock()
		if a.b.runners[a.r.name] == a.r {
			a.r.counted = true
			g.l.end(a.b, a.r.name, g.now())
			g.d.Metrics.RunnerStarted(name, a.b.ss, false)
		}
		g.mu.Unlock()
		g.log.Warn("A host restarted before it held a runner, so its registration is removed", "host", name,
			"scope", a.b.ss.Scope, "profile", a.b.ss.Profile, "runner", a.r.name)
		g.unregister(ctx, a.b.ss.Scope, a.r.name, a.r.id)
		return
	}
	g.closeRecord(ctx, a.b, a.r, a.reason, true, nil)
	g.log.Warn("A host restarted and no longer lists a runner, so its record is closed", "host", name,
		"scope", a.b.ss.Scope, "profile", a.b.ss.Profile, "runner", a.r.name, "reason", string(a.reason))
}

// closeDropped closes a record unlisted sends to actDropped: a failed
// start's frees its slot, and any other closes for a.reason.
func (g *generation) closeDropped(ctx context.Context, a *action) {
	g.mu.Lock()
	failed := a.r.failed
	if failed && a.b.runners[a.r.name] == a.r {
		g.l.end(a.b, a.r.name, g.now())
	}
	g.mu.Unlock()
	if !failed {
		g.closeRecord(ctx, a.b, a.r, a.reason, a.reason != ExitIdle, nil)
	}
	g.log.Info("A host no longer lists a runner, so its record is closed", "host", g.hosts[a.r.host].cfg.Name,
		"scope", a.b.ss.Scope, "profile", a.b.ss.Profile, "runner", a.r.name, "reason", string(a.reason))
}

// closeRecord ends r's record, which frees its reservation, counts its
// exit, hands it to the run history with ev, nil when none was read, and,
// with unreg and no JobCompleted naming it, removes its registration.
func (g *generation) closeRecord(ctx context.Context, b *book, r *runner, reason ExitReason, unreg bool, ev *evidence) {
	g.mu.Lock()
	if b.runners[r.name] != r {
		g.mu.Unlock()
		return
	}
	g.l.end(b, r.name, g.now())
	completed := r.completed
	host := g.hosts[r.host].cfg.Name
	g.d.Metrics.RunnerExited(host, b.ss, reason)
	g.mu.Unlock()
	g.exited(ctx, host, r.name, reason, ev)
	if unreg && !completed {
		g.unregister(ctx, b.ss.Scope, r.name, r.id)
	}
}

// retire removes an idle runner: its registration first, so GitHub hands
// no job to a runner about to vanish, then its container. A runner GitHub
// says holds a job is busy, and any other failure keeps both for the next
// pass.
func (g *generation) retire(ctx context.Context, a *action) {
	switch err := g.gh.ScaleSets.RemoveRunner(ctx, g.sets[a.b].scope.cfg, a.r.id); {
	case errors.Is(err, github.ErrRunnerBusy):
		g.mu.Lock()
		a.r.state, a.r.closing = busy, ""
		g.mu.Unlock()
		return
	case err != nil:
		if ctx.Err() == nil {
			g.log.Warn("Removing an idle runner's registration failed, so the pass retries it", "scope", a.b.ss.Scope, "runner", a.r.name, "error", err)
		}
		return
	}
	g.mu.Lock()
	a.r.closing = ExitIdle
	g.mu.Unlock()
	if !g.remove(ctx, a.r.host, a.r.name) {
		return
	}
	g.closeRecord(ctx, a.b, a.r, ExitIdle, false, nil)
	g.log.Info("Removed an idle runner", "scope", a.b.ss.Scope, "profile", a.b.ss.Profile, "runner", a.r.name)
}

// expire removes a runner past its profile's max_lifetime.
func (g *generation) expire(ctx context.Context, a *action) {
	g.mu.Lock()
	a.r.closing = ExitLifetime
	g.mu.Unlock()
	if !g.remove(ctx, a.r.host, a.r.name) {
		return
	}
	g.closeRecord(ctx, a.b, a.r, ExitLifetime, true, nil)
	g.log.Warn("A runner passed its profile's max_lifetime, so it was removed", "scope", a.b.ss.Scope, "profile", a.b.ss.Profile, "runner", a.r.name)
}

// exited hands a runner's end to the run history; an idle runner held no
// job.
func (g *generation) exited(ctx context.Context, host, name string, reason ExitReason, ev *evidence) {
	if reason == ExitIdle {
		return
	}
	x := &RunnerExit{At: g.now(), Host: host, Runner: name, Reason: reason}
	if ev != nil {
		x.Code, x.Log, x.Diag = ev.code, ev.log, ev.diag
	}
	g.d.Runs.Exited(ctx, x)
}

// endContainer removes an ended runner's container on host h, reading an
// unclean exit's log tail first and its _diag tar with the removal, and
// reports whether the container is gone.
func (g *generation) endContainer(ctx context.Context, h int, name string, reason ExitReason, code *int) (evidence, bool) {
	ev := evidence{code: code}
	keep := unclean(reason)
	if keep {
		var err error
		if ev.log, err = g.api(h).Logs(ctx, name, tailLines); err != nil && ctx.Err() == nil {
			g.log.Warn("Reading an ended runner's log failed, so its run keeps no log tail", "host", g.hosts[h].cfg.Name, "runner", name, "error", err)
		}
	}
	diag, ok := g.removeKeeping(ctx, h, name, keep)
	ev.diag = diag
	return ev, ok
}

// remove deletes runner name's container and entry on host h, and reports
// whether it is gone.
func (g *generation) remove(ctx context.Context, h int, name string) bool {
	_, ok := g.removeKeeping(ctx, h, name, false)
	return ok
}

// removeKeeping is remove, answering the runner's _diag tar with diag.
func (g *generation) removeKeeping(ctx context.Context, h int, name string, diag bool) ([]byte, bool) {
	tar, err := g.api(h).DeleteRunner(ctx, name, diag)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, errDisconnected) {
			g.d.Metrics.HostLinkError(g.hosts[h].cfg.Name)
			g.log.Warn("Removing a runner failed, so the pass retries it", "host", g.hosts[h].cfg.Name, "runner", name, "error", err)
		}
		return nil, false
	}
	return tar, true
}
