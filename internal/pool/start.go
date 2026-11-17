package pool

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

const (
	// startTimeout bounds one start's JIT config and POST; the host builds
	// the container after its answer, so no deadline here cuts a create.
	startTimeout = 3 * time.Minute
	startsAtOnce = 8
	// startRetry holds a scale set's starts back after a JIT config failed.
	startRetry = 10 * time.Second
	// cleanupTimeout bounds a registration removal, which runs past
	// shutdown so no registration is left behind.
	cleanupTimeout = 30 * time.Second
)

// runStarts is the start worker. Each poke starts a runner for every
// assignment that holds a slot and no runner, under ctx; once stop closes
// it takes no new start and returns when the starts in flight are done,
// so the next generation takes over answered records only.
func (g *generation) runStarts(ctx context.Context, stop <-chan struct{}) {
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, startsAtOnce)
	for {
		select {
		case <-stop:
			return
		case <-g.kick:
		}
		for _, j := range g.owed() {
			select {
			case sem <- struct{}{}:
				wg.Go(func() {
					defer func() { <-sem }()
					g.start(ctx, j)
				})
			case <-stop:
				g.mu.Lock()
				g.l.end(j.b, j.r.name, g.now())
				g.mu.Unlock()
			}
		}
	}
}

// launch is a runner record a start takes over: its scale set and host.
type launch struct {
	b *book
	r *runner
	h int
}

// owed takes a slot for each assignment that waits for a runner, on the
// host the slot is reserved on, and records a starting runner there.
func (g *generation) owed() []launch {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate()
	now := g.now()
	var out []launch
	for _, b := range g.l.books {
		if g.stopping || now.Before(g.sets[b].retryAt) {
			continue
		}
		for {
			name := g.runnerName(b)
			h, ok := g.l.take(b, name)
			if !ok {
				break
			}
			r := b.runners[name]
			r.created, r.posting = now, true
			out = append(out, launch{b: b, r: r, h: h})
		}
	}
	return out
}

// start registers j's runner with GitHub, then asks its host to start it.
func (g *generation) start(parent context.Context, j launch) {
	ctx, cancel := context.WithTimeout(parent, startTimeout)
	defer cancel()
	b, r := j.b, j.r
	g.mu.Lock()
	scope := g.sets[b].scope.cfg
	g.mu.Unlock()
	jit, err := g.gh.ScaleSets.JIT(ctx, scope, b.ss, r.name)
	if err != nil {
		g.mu.Lock()
		g.l.end(b, r.name, g.now())
		if parent.Err() == nil {
			g.sets[b].retryAt = g.now().Add(startRetry)
			g.d.Metrics.RunnerStarted(g.hosts[j.h].cfg.Name, b.ss, false)
		}
		g.mu.Unlock()
		if parent.Err() == nil {
			g.log.Warn("Registering a runner failed, so the start is retried", "scope", b.ss.Scope, "profile", b.ss.Profile, "runner", r.name, "error", err)
		}
		g.poke()
		return
	}
	g.mu.Lock()
	r.id, r.instance = jit.RunnerID, g.hosts[j.h].instance
	r.req = &hostapi.RunnerRequest{Profile: b.ss.Profile, RunnerName: r.name, Scope: b.ss.Scope, JITConfig: jit.Config, RunnerID: jit.RunnerID}
	g.mu.Unlock()
	g.post(ctx, b, r)
}

// post sends r's POST. An answer makes the host the runner's keeper; a
// refusal means the host created nothing, so the registration goes and
// the slot frees at once; any other failure decides nothing, and the
// reconciler repeats the POST by name until a list judges it.
func (g *generation) post(ctx context.Context, b *book, r *runner) {
	g.mu.Lock()
	hi := r.host
	h := g.hosts[hi]
	req := r.req
	g.mu.Unlock()
	ans, err := g.api(hi).CreateRunner(ctx, req)
	var notReady *hostapi.NotReadyError
	refused := errors.As(err, &notReady) || errors.Is(err, hostapi.ErrUnknownProfile) || errors.Is(err, hostapi.ErrProfileRefused)
	g.mu.Lock()
	r.posting = false
	held := b.runners[r.name] == r
	switch {
	case !held:
	case err == nil:
		h.seq++
		r.answered, r.instance, r.req = h.seq, ans.InstanceID, nil
	case refused:
		if h.ready != nil {
			h.ready[b.profile] = false
		}
		r.counted = true
		g.l.end(b, r.name, g.now())
		g.d.Metrics.RunnerStarted(h.cfg.Name, b.ss, false)
		g.gate()
	}
	g.mu.Unlock()
	switch {
	case !held || err == nil:
	case refused:
		g.log.Warn("A host refused a runner, so its registration is removed and its slot freed", "host", h.cfg.Name,
			"scope", b.ss.Scope, "profile", b.ss.Profile, "runner", r.name, "error", err)
		g.unregister(ctx, b.ss.Scope, r.name, r.id)
		wake(h.wake)
	case !errors.Is(ctx.Err(), context.Canceled):
		g.log.Warn("A host did not answer a runner's start, so the start is sent again by name", "host", h.cfg.Name,
			"scope", b.ss.Scope, "profile", b.ss.Profile, "runner", r.name, "error", err)
	}
	g.poke()
}

// unregister removes registration id of runner name in scope. One GitHub
// refuses as running a job, or that fails, is retried at each pool-wide
// step until GitHub takes it or a JobCompleted names the runner.
func (g *generation) unregister(ctx context.Context, scope, name string, id int64) {
	if id <= 0 {
		return
	}
	cfg := g.scopeConfig(scope)
	if cfg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	err := g.gh.ScaleSets.RemoveRunner(ctx, cfg, id)
	g.mu.Lock()
	defer g.mu.Unlock()
	owed := g.removals(scope)
	if err == nil {
		delete(owed, name)
		return
	}
	if _, again := owed[name]; !again && !errors.Is(err, github.ErrRunnerBusy) {
		g.log.Warn("Removing a runner's registration failed, so each pass retries it", "scope", scope, "runner", name, "error", err)
	}
	owed[name] = id
}

// removals is the registration removals owed in the scope with key, by
// runner name: the scope's own, or for a scope the settings no longer
// hold, the generation's departed record of it; g.mu is held.
func (g *generation) removals(key string) map[string]int64 {
	if sc := g.scopes[key]; sc != nil {
		return sc.unregister
	}
	if g.departed[key] == nil {
		g.departed[key] = map[string]int64{}
	}
	return g.departed[key]
}

// scopeConfig is the settings' scope with key, or a dropped one an earlier
// generation held, which its runners deregister under; nil for one no
// generation of this process held. It reads only what takeOver set before
// the generation runs, so it takes no lock.
func (g *generation) scopeConfig(key string) *config.Scope {
	if sc := g.scopes[key]; sc != nil {
		return sc.cfg
	}
	return g.gone[key]
}

// unregisterPending retries every registration removal still owed.
func (g *generation) unregisterPending(ctx context.Context) {
	type owed struct {
		scope, name string
		id          int64
	}
	var due []owed
	g.mu.Lock()
	for key, sc := range g.scopes {
		for name, id := range sc.unregister {
			due = append(due, owed{key, name, id})
		}
	}
	for key, names := range g.departed {
		for name, id := range names {
			due = append(due, owed{key, name, id})
		}
	}
	g.mu.Unlock()
	for _, o := range due {
		if ctx.Err() != nil {
			return
		}
		g.unregister(ctx, o.scope, o.name, o.id)
	}
}
