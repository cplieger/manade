package pool

import (
	"context"
	"time"
)

// ReconcileLease is how long a host's reconcile loop may go between beats
// before it reads as wedged, and passLease how long the pass loop may go
// between passes: six reconciler intervals each.
const (
	ReconcileLease = 6 * reconcileEvery
	passLease      = 6 * reconcileEvery
)

// applyLease is the longest one settings revision may take to apply: its
// ensures, each bounded by ensureTimeout, and the boot pass.
const applyLease = 10 * time.Minute

// shutdownTimeout bounds the shutdown inside the shipped compose file's
// 60-second stop_grace_period; a test seam.
var shutdownTimeout = 45 * time.Second

// boot is the boot path's pass: every connected host's runner list is
// judged against the records the generation holds or took over, which
// adopts what no record holds and retires idle runners whose profile
// changed. A host that does not answer is judged once it does.
func (g *generation) boot(ctx context.Context) {
	g.pass(ctx)
	g.mu.Lock()
	g.booting = false
	g.mu.Unlock()
}

// run starts the generation's loops, which last until stop. Their calls
// run under ctx, which a stop does not cut: each loop ends at its next pass
// boundary, and the start worker finishes the starts in flight.
func (g *generation) run(ctx context.Context) {
	loops, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.stopLoops = cancel
	g.mu.Unlock()
	stop := loops.Done()
	for i := range g.hosts {
		g.wg.Go(func() { g.runProver(loops, i) })
		g.wg.Go(func() { g.watchEvents(loops, i) })
		g.wg.Go(func() { g.runReconciler(ctx, stop, i) })
	}
	g.wg.Go(func() { g.runStarts(ctx, stop) })
	g.wg.Go(func() { g.runVisibility(loops) })
	g.wg.Go(func() { g.runStep(loops) })
	g.wg.Go(func() { g.runEnsure(loops) })
	g.wg.Go(func() { g.gh.Sweep.Run(loops, g.cfg, g.hot, g.queued) })
}

// stop ends the loops at their pass boundaries and waits for them; it
// removes nothing.
func (g *generation) stop() {
	g.mu.Lock()
	cancel := g.stopLoops
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	g.wg.Wait()
}

// shutdown stops the pool within shutdownTimeout. From stopping on, a poll
// reports 0 and no start begins; the loops end, then endSessions closes
// the sessions, then a last pass removes the idle runners and leaves busy
// ones.
func (g *generation) shutdown(endSessions func()) {
	g.mu.Lock()
	g.stopping = true
	g.gate()
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.stop()
		endSessions()
		g.pass(ctx)
	}()
	select {
	case <-done:
		g.log.Info("The pool stopped; busy runners keep running and the next start adopts them")
	case <-ctx.Done():
		g.log.Warn("The shutdown gave up after its deadline, so the next start adopts or removes what is left",
			"deadline", shutdownTimeout.String())
	}
}

// Health is the pool's part of the control plane's health.
// Booted holds once the first generation's boot pass has run; Passing
// while the current generation's pass loop ended a pass within passLease;
// Reconciling while each connected host's reconcile loop waits on a call
// or beat within ReconcileLease; Applied while no settings revision has
// taken longer than applyLease to apply. GitHub and the hosts are
// upstreams, so no session, poll or host state counts.
type Health struct{ Booted, Passing, Reconciling, Applied bool }

// health reads the generation's health at now; Applied is the
// registry's.
func (g *generation) health(now time.Time) Health {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := Health{Booted: !g.booting, Passing: now.Sub(g.stepped) <= passLease, Reconciling: true}
	for _, ho := range g.hosts {
		if _, connected := g.d.Links.Host(ho.cfg.Name); connected {
			h.Reconciling = h.Reconciling && (ho.waiting || now.Sub(ho.beat) <= ReconcileLease)
		}
	}
	return h
}
