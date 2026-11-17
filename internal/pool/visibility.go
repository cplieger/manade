package pool

import (
	"context"
	"time"
)

const (
	// proofAge is how long a visibility proof lets a scope be funded past
	// its assignments.
	proofAge = time.Minute
	// proofRetry spaces the reads of a scope whose last read failed.
	proofRetry = time.Minute
	// recheckEvery is the pass that reads every scope's visibility, idle
	// ones included, and its installation.
	recheckEvery = 10 * time.Minute
	// proofTick is how often the loop looks for a read due.
	proofTick = 5 * time.Second
	// refreshMargin is how long before proofAge a scope polls keep above
	// its assignments is read again: one proofTick until the loop sees it
	// due, and the read's own latency, so the next proof lands before the
	// last one lapses.
	refreshMargin = 10 * time.Second
)

// runVisibility reads visibility until ctx ends: a scope a poll found
// stale or the generation added at once, one a poll reported above its
// assignments before its proof lapses, a failed one proofRetry after its
// last read, and every scope each recheckEvery, then the installations
// behind them.
func (g *generation) runVisibility(ctx context.Context) {
	t := time.NewTicker(proofTick)
	defer t.Stop()
	g.visibility(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.verify:
		case <-t.C:
		}
		g.visibility(ctx)
	}
}

// visibility reads every scope due now, outside the lock, one at a time,
// then routes the scopes due; a read the stop cuts short records nothing.
// With no credential nothing can be read.
func (g *generation) visibility(ctx context.Context) {
	if !g.credentialed {
		return
	}
	g.mu.Lock()
	now := g.now()
	all := now.Sub(g.rechecked) >= recheckEvery
	if all {
		g.rechecked = now
	}
	var due []*scope
	for i := range g.cfg.Scopes {
		sc := g.scopes[g.cfg.Scopes[i].Key]
		if !sc.public && (all || sc.wanted || sc.refresh(now) || !sc.failed.IsZero() && now.Sub(sc.read) >= proofRetry) {
			due = append(due, sc)
		}
	}
	g.mu.Unlock()
	for _, sc := range due {
		private, err := g.gh.REST.Private(ctx, sc.cfg)
		if ctx.Err() != nil {
			return
		}
		g.mu.Lock()
		g.verdict(sc, private, err, now)
		g.mu.Unlock()
	}
	g.routing(ctx, all, now)
	if all {
		g.reg.prune(ctx, g)
		if err := g.gh.ScaleSets.Recheck(ctx); err != nil && ctx.Err() == nil {
			g.log.Warn("Resolving the scopes' installations failed, so their clients stay as they are until the next pass", "error", err)
		}
	}
}

// verdict records a visibility read of sc begun at at; g.mu is held. A
// scope proven public stays public for as long as the settings serve it:
// its sessions close and its idle runners are removed.
func (g *generation) verdict(sc *scope, private bool, err error, at time.Time) {
	sc.wanted, sc.read = false, at
	switch {
	case err != nil:
		if sc.failed.IsZero() {
			sc.failed = at
			g.log.Warn("Reading a scope's visibility failed, so the pool funds nothing for it until a read succeeds",
				"scope", sc.cfg.Key, "error", err)
		}
	case !private:
		sc.public = true
		g.log.Error("A scope's repository is public or its runner group admits public repositories, so the pool stops serving it",
			"scope", sc.cfg.Key)
		g.reg.closeScope(sc.cfg.Key)
		// Its variable is deleted at once, so no new job is routed to it.
		sc.routed, sc.routeTry = time.Time{}, time.Time{}
		g.wakeAll()
	default:
		if !sc.failed.IsZero() {
			g.log.Info("A scope's visibility read succeeded again, so the pool serves it", "scope", sc.cfg.Key, "after", at.Sub(sc.failed).String())
		}
		sc.failed, sc.proof = time.Time{}, at
	}
	g.gate()
}

// wantProof asks for sc's visibility to be read again; g.mu is held.
func (g *generation) wantProof(sc *scope) {
	sc.wanted = true
	wake(g.verify)
}

// refresh reports that a poll within proofAge reported capacity above sc's
// assignments and its proof lapses within refreshMargin. A failed scope
// waits for proofRetry instead.
func (sc *scope) refresh(now time.Time) bool {
	return sc.failed.IsZero() && now.Sub(sc.above) < proofAge && now.Sub(sc.proof) >= proofAge-refreshMargin
}
