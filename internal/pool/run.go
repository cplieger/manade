package pool

import (
	"context"
	"time"
)

// listenerLease is the longest a served listener goes between beats: the
// spacing github.Handler.Beat owns, plus 10 seconds so a hanging GitHub, an
// upstream, never reads as an unhealthy listener.
const listenerLease = 75 * time.Second

// ReconcileLease is how long a host's reconcile loop may go between beats
// before it reads as wedged: six reconciler intervals.
const ReconcileLease = 6 * reconcileEvery

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
	g.wg.Go(func() { g.d.Sweep.Run(loops, g.cfg, g.hot, g.queued) })
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

// Health is the pool's state for its health verdict: Started once the
// first generation booted and every served scale set's session has
// opened, Listening while each such listener beat within listenerLease,
// Reconciling while each host's reconcile loop waits on a call or beat
// within ReconcileLease, and Stopping once the shutdown began. A scope
// proven public has no listener and counts in neither. No host's own state
// counts: hosts are upstreams, which their own series report.
type Health struct{ Started, Listening, Reconciling, Stopping bool }

// Healthy reports every field of h set and the pool not stopping.
func (h Health) Healthy() bool { return h.Started && h.Listening && h.Reconciling && !h.Stopping }

// health reads the generation's health now.
func (g *generation) health() Health {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	h := Health{Started: !g.booting, Listening: true, Reconciling: true, Stopping: g.stopping}
	for _, s := range g.sets {
		if s.scope.public {
			continue
		}
		h.Started = h.Started && s.opened
		h.Listening = h.Listening && now.Sub(s.beat) <= listenerLease
	}
	for _, ho := range g.hosts {
		h.Reconciling = h.Reconciling && (ho.waiting || now.Sub(ho.beat) <= ReconcileLease)
	}
	return h
}
