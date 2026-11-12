package pool

import (
	"context"
	"time"
)

const (
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
	// last one lapses. One clock per scope keeps it near one read a minute
	// however many of its scale sets poll.
	refreshMargin = 10 * time.Second
)

// runProofs reads visibility until ctx ends: a scope a poll found stale at
// once, one a poll reported above its assignments within proofAge before
// its proof lapses, a failed one proofRetry after its last read, and every
// scope each recheckEvery, then the installations behind them.
func (p *Pool) runProofs(ctx context.Context) {
	t := time.NewTicker(proofTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.verify:
		case <-t.C:
		}
		p.proofs(ctx)
	}
}

// proofs reads every scope due now. Reads run outside the lock, one at a
// time; a read shutdown cuts short records nothing.
func (p *Pool) proofs(ctx context.Context) {
	p.mu.Lock()
	now := p.now()
	all := now.Sub(p.rechecked) >= recheckEvery
	if all {
		p.rechecked = now
	}
	var due []*scope
	for i := range p.cfg.Scopes {
		sc := p.scopes[p.cfg.Scopes[i].Key]
		if !sc.public && (all || sc.wanted || sc.refresh(now) || !sc.failed.IsZero() && now.Sub(sc.read) >= proofRetry) {
			due = append(due, sc)
		}
	}
	p.mu.Unlock()
	for _, sc := range due {
		private, err := p.rest.Private(ctx, sc.cfg)
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		p.prove(sc, private, err, now)
		p.mu.Unlock()
	}
	if all {
		if err := p.gh.Recheck(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("Resolving the scopes' installations failed, so their clients stay as they are until the next pass", "error", err)
		}
	}
}

// refresh reports that a poll within proofAge reported capacity above sc's
// assignments and its proof lapses within refreshMargin; p.mu is held. A
// failed scope waits for proofRetry instead.
func (sc *scope) refresh(now time.Time) bool {
	return sc.failed.IsZero() && now.Sub(sc.above) < proofAge && now.Sub(sc.proof) >= proofAge-refreshMargin
}
