package pool

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/jobspec"
	"github.com/cplieger/manade/internal/r5/obs"
)

const (
	// listenerLease is the longest a served listener goes between beats.
	// github.Handler's Beat promises one 5-second step and one create or
	// poll of at most 60 seconds; that 60 is a context deadline the call
	// returns after, so 10 seconds more keeps a hanging GitHub, which is an
	// upstream, from reading as an unhealthy listener.
	listenerLease = 75 * time.Second
	// passLease is six reconciler intervals without a good pass, which
	// reads as a wedged reconciler or an unreachable daemon.
	passLease = 6 * reconcileEvery
)

// shutdownTimeout bounds the shutdown inside compose.yaml's 60-second
// stop_grace_period; a test seam.
var shutdownTimeout = 45 * time.Second

var errUnsettled = errors.New("pool: the previous process's runners are not all adopted or removed, so no session opens")

// Run boots the pool and serves every scale set until ctx ends, then shuts
// down: no poll reports capacity, every session closes, idle runners are
// removed, and busy ones keep running for the next start to adopt. It
// waits for the shutdown, or gives up on it after 45 seconds, and returns
// an error only when boot fails.
func (p *Pool) Run(ctx context.Context) error {
	if err := p.bootRecorded(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	// The loops outlive ctx, so shutdown sets stopping before they end.
	loops, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	var wg sync.WaitGroup
	// Listeners first: a public verdict from the proofs loop needs each
	// scope's stop in place.
	p.listen(loops, &wg)
	wg.Go(func() { p.runStarts(loops) })
	wg.Go(func() { p.runProofs(loops) })
	wg.Go(func() { p.runImages(loops) })
	wg.Go(func() { p.runReconciler(loops) })
	wg.Go(func() { p.sweep.Run(loops, p.hot, p.queued) })
	<-ctx.Done()
	p.shutdown(stop, &wg)
	return nil
}

// bootRecorded is boot under obs's boot record, ended once boot has
// finished. A failed boot records no end: the process exits on its error.
func (p *Pool) bootRecorded(ctx context.Context) error {
	rec, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { p.m.Boot(rec) })
	err := p.boot(ctx)
	stop()
	wg.Wait()
	if err == nil {
		p.m.Booted()
	}
	return err
}

// boot readies the pool, up to the sessions. It fails on
// what only the operator can fix or a restart retries: a public scope, an
// image whose user the check refuses, a disk limit the daemon rejects, an
// installation or scale set GitHub refuses, or a runner left unadopted. A
// scale set exists before adoption because adoption files a runner under it.
func (p *Pool) boot(ctx context.Context) error {
	if err := p.clearProbes(ctx); err != nil {
		return err
	}
	for _, n := range p.host.Networks() {
		if err := p.dk.EnsureNetwork(ctx, n); err != nil {
			return fmt.Errorf("pool: job network %s: %w", n.Name, err)
		}
	}
	if err := p.loadImages(ctx); err != nil {
		return err
	}
	if err := p.probeDisk(ctx); err != nil {
		return err
	}
	if err := p.rest.CheckInstallations(ctx); err != nil {
		return err
	}
	if err := p.proveScopes(ctx); err != nil {
		return err
	}
	for i := range p.cfg.Scopes {
		sets, err := p.gh.Ensure(ctx, &p.cfg.Scopes[i])
		if err != nil {
			return err
		}
		for _, ss := range sets {
			p.addSet(ss)
		}
	}
	if !p.pass(ctx) {
		return errUnsettled
	}
	p.mu.Lock()
	p.booting = false
	p.mu.Unlock()
	return nil
}

// loadImages brings each image into service, pulling only what the cache
// lacks. A user check that refuses an image fails boot naming its
// profile; any other failure leaves the image missing and on backoff.
func (p *Pool) loadImages(ctx context.Context) error {
	for _, img := range p.imgs {
		if err := p.refresh(ctx, img, false); errors.Is(err, jobspec.ErrRootUser) {
			return fmt.Errorf("pool: %w", err)
		}
	}
	return nil
}

// clearProbes removes every probe a previous process left, with no GitHub
// read: no call of this process records one yet. A failed removal is left
// to the reconciler's probeAge rule.
func (p *Pool) clearProbes(ctx context.Context) error {
	list, err := p.dk.ListContainers(ctx)
	if err != nil {
		p.dockerFailed(ctx, obs.DockerList)
		return fmt.Errorf("pool: listing the pool's containers: %w", err)
	}
	for _, c := range list {
		if c.Labels[docker.LabelProbe] == "" {
			continue
		}
		if err := p.dk.RemoveOwned(ctx, c.ID); err != nil && p.dockerFailed(ctx, obs.DockerRemove) {
			p.log.Warn("Removing a leftover probe container failed, so a reconciler pass removes it later", "container", c.ID, "error", err)
		}
	}
	return nil
}

// probeDisk creates and removes one probe with the first disk profile's
// size limit, which a driver that cannot enforce it refuses. With no
// image in service it is skipped: every runner create carries the same
// limit, so such a driver refuses those instead.
func (p *Pool) probeDisk(ctx context.Context) error {
	for pr := range p.cfg.Profiles {
		if p.cfg.Profiles[pr].Disk == 0 {
			continue
		}
		id := cmp.Or(p.images[pr], cmp.Or(p.images...))
		if id == "" {
			p.log.Warn("No image is in service, so the disk probe is skipped", "profile", p.cfg.Profiles[pr].Name)
			return nil
		}
		if err := p.probe(ctx, pr, jobspec.ProbeDisk, id, nil); err != nil {
			return fmt.Errorf("pool: the daemon refused profile %s's disk limit: %w", p.cfg.Profiles[pr].Name, err)
		}
		return nil
	}
	return nil
}

// proveScopes reads every scope's visibility once. A scope proven public
// fails boot; a failed read leaves it unfunded and retried a
// minute later. The 10-minute pass counts from here.
func (p *Pool) proveScopes(ctx context.Context) error {
	now := p.now()
	for i := range p.cfg.Scopes {
		sc := p.scopes[p.cfg.Scopes[i].Key]
		private, err := p.rest.Private(ctx, sc.cfg)
		if err == nil && !private {
			return fmt.Errorf("pool: scope %s is public or admits public repositories, which the pool refuses", sc.cfg.Key)
		}
		p.mu.Lock()
		p.prove(sc, private, err, now)
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.rechecked = now
	p.mu.Unlock()
	return nil
}

// listen starts each scale set's listener under its scope's context, which
// a public verdict cancels so that scope's sessions close.
func (p *Pool) listen(ctx context.Context, wg *sync.WaitGroup) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sc := range p.scopes {
		sctx, cancel := context.WithCancel(ctx)
		sc.stop = cancel
		for _, s := range p.sets {
			if s.scope == sc {
				wg.Go(func() { p.gh.Listen(sctx, s.ss, p) })
			}
		}
	}
}

// shutdown stops the pool within shutdownTimeout. From stopping on, a
// poll reports 0 and no start begins; the loops end and their sessions
// close; then a last pass removes the idle runners and leaves busy ones.
func (p *Pool) shutdown(stop context.CancelFunc, wg *sync.WaitGroup) {
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
		p.pass(ctx)
	}()
	select {
	case <-done:
		p.log.Info("The pool stopped; busy runners keep running and the next start adopts them")
	case <-ctx.Done():
		p.log.Warn("The shutdown gave up after its deadline, so the next start adopts or removes what is left",
			"deadline", shutdownTimeout.String())
	}
}

// Health is the pool's state for its health verdict: Started once
// boot is done and every served scale set's session has opened, Listening
// while each such listener beat within listenerLease, Reconciling while the
// last good reconciler pass is within passLease. A scope proven public has
// no listener and counts in neither.
type Health struct{ Started, Listening, Reconciling bool }

// Health reads the pool's health now.
func (p *Pool) Health() Health {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.health()
}

// health is Health with p.mu held.
func (p *Pool) health() Health {
	now := p.now()
	h := Health{Started: !p.booting, Listening: true, Reconciling: now.Sub(p.passed) <= passLease}
	for _, s := range p.sets {
		if s.scope.public {
			continue
		}
		h.Started = h.Started && s.opened
		h.Listening = h.Listening && now.Sub(s.beat) <= listenerLease
	}
	return h
}

// Healthy reports every field of Health set and the pool not stopping. It
// satisfies go/health's Signal.
func (p *Pool) Healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.health()
	return h.Started && h.Listening && h.Reconciling && !p.stopping
}
