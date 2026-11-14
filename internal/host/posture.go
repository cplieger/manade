package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/host/platform"
)

// posture is the egress state a section's open and internet keys decide:
// the rules the guard holds, an internet: false host's domain list and
// learned addresses, and an ipv6 host's learned /64. Bring-up builds one;
// a change of either key builds the next one the same way, never by
// converting this one.
type posture struct {
	guard *guard
	// gh is what the host's resolver learns into, nil unless internet is
	// false.
	gh *gitHub
	// host is the section applied, which a reload of allow entries alone
	// replaces.
	host     atomic.Pointer[config.Host]
	self64   atomic.Pointer[learned64]
	learns64 bool
}

// rebuild is a change of posture sent to hold.
type rebuild struct {
	h    *config.Host
	done chan rebuilt
}

type rebuilt struct{ failed, fatal error }

// newPosture answers section h's posture, nothing of it loaded yet.
func (n *hostNet) newPosture(h *config.Host) *posture {
	p := &posture{learns64: h.Egress.IPv6 && h.Egress.Internet}
	p.host.Store(h)
	p.self64.Store(unasked64())
	if !h.Egress.Internet {
		p.gh = n.newGitHub()
	}
	p.guard = newGuard(n.rules, n.want(p, h), n.log, n.hooks)
	return p
}

// want is what posture p's guard holds for section h.
func (n *hostNet) want(p *posture, h *config.Host) wanted {
	_, bridge6 := platform.BridgeNF(n.rules.Root)
	w := wanted{
		Generate: func() (*egress.Ruleset, error) {
			st := egress.State{Own64: n.Own64, Self64: p.self64.Load().prefix}
			if p.gh != nil {
				st.GitHub = p.gh.learner.Held()
			}
			return egress.Generate(&egress.Input{Host: h, Job6: n.Job6, State: st}, n.Kernel.Backend)
		},
		IPv6Off:   !h.Egress.Open && !h.Egress.IPv6,
		BridgeNF6: h.Egress.IPv6 && bridge6,
	}
	if p.gh != nil {
		w.Loading = p.gh.learner.Loading()
	}
	return w
}

// ready reads an internet: false posture's domain list through the host's
// resolver, learning as it does, then resolves GitHub's API name through it.
func (n *hostNet) ready(ctx context.Context, p *posture) error {
	if p.gh != nil {
		if err := p.gh.source.Start(ctx); err != nil {
			return err
		}
	}
	return netsetup.CheckName(ctx, p.host.Load(), n.seams.Lookup)
}

// section answers the section the running posture applies.
func (n *hostNet) section() *config.Host { return n.posture.Load().host.Load() }

// rulesGuard answers the running posture's guard.
func (n *hostNet) rulesGuard() *guard { return n.posture.Load().guard }

// postureChanged reports whether section h's open or internet key differs
// from the running posture's: a change that only change applies.
func (n *hostNet) postureChanged(h *config.Host) bool {
	e := n.section().Egress
	return e.Open != h.Egress.Open || e.Internet != h.Egress.Internet
}

// applicable answers why the running host cannot take section h: a key
// only a restart applies changed, or a resolver or allow entry overlaps
// the job subnet in use.
func (n *hostNet) applicable(h *config.Host) error {
	err := restartOnly(n.section(), h)
	if c := netsetup.Conflicts(n.Subnet, h); len(c) > 0 {
		err = errors.Join(err, fmt.Errorf("the job subnet in use %s", strings.Join(c, "; ")))
	}
	return err
}

// swap loads section h's rules into the running posture, whose open and
// internet keys h shares, keeping its learned state. A failed swap keeps
// the old rules and answers why as kept; fatal is a failure the guard would
// answer, and the caller stops the host.
func (n *hostNet) swap(ctx context.Context, h *config.Host) (kept, fatal error) {
	p := n.posture.Load()
	kept, fatal = p.guard.swap(ctx, n.want(p, h))
	if kept == nil && fatal == nil {
		p.host.Store(h)
	}
	return kept, fatal
}

// change replaces the running posture with section h's through hold, which
// must be running: hold stops the posture's work and builds h's as
// bring-up builds one. failed is h's failure, after which the old section's
// posture is up again, built the same way; fatal is a failure of that,
// which ends hold too. ctx ending first answers its error as failed.
func (n *hostNet) change(ctx context.Context, h *config.Host) (failed, fatal error) {
	r := &rebuild{h: h, done: make(chan rebuilt, 1)}
	select {
	case n.rebuilds <- r:
	case <-ctx.Done():
		return ctx.Err(), nil
	}
	select {
	case res := <-r.done:
		return res.failed, res.fatal
	case <-ctx.Done():
		return ctx.Err(), nil
	}
}

// holdPostures holds each posture in turn, applying each change between
// two, until ctx ends, answering nil, or a posture's part or a change's
// return to the old posture fails, answering why.
func (n *hostNet) holdPostures(ctx context.Context) error {
	for {
		r, err := n.holdPosture(ctx, n.posture.Load())
		if r == nil {
			return err
		}
		failed, fatal := n.rebuildTo(ctx, r.h)
		r.done <- rebuilt{failed: failed, fatal: fatal}
		switch {
		case ctx.Err() != nil:
			return nil
		case fatal != nil:
			return fatal
		}
	}
}

// holdPosture runs p's guard, domain list refresh and /64 learner until
// ctx ends, answering nil, until one fails, answering why, or until a
// change arrives, answering it once p's work has stopped.
func (n *hostNet) holdPosture(ctx context.Context, p *posture) (*rebuild, error) {
	pctx, cancel := context.WithCancel(ctx)
	failed := make(chan error, 1)
	fail := func(err error) {
		select {
		case failed <- err:
		default:
		}
		cancel()
	}
	var wg sync.WaitGroup
	if p.gh != nil {
		wg.Go(func() { p.gh.source.Run(pctx) })
	}
	if p.learns64 {
		wg.Go(func() {
			if err := n.learn64(pctx, p); err != nil {
				fail(err)
			}
		})
	}
	wg.Go(func() {
		if err := p.guard.run(pctx); err != nil {
			fail(err)
		}
	})
	var r *rebuild
	select {
	case <-pctx.Done():
	case r = <-n.rebuilds:
	}
	cancel()
	wg.Wait()
	select {
	case err := <-failed:
		if r != nil {
			r.done <- rebuilt{fatal: err}
		}
		return nil, err
	default:
	}
	return r, nil
}

// rebuildTo replaces the posture with section h's, as bring-up builds one:
// the old posture's learner is retired and h's rules replace its own, none
// of its learned state carried over. When h's fails, the old section's
// posture is built again the same way; failed is h's failure and fatal a
// failure of that.
func (n *hostNet) rebuildTo(ctx context.Context, h *config.Host) (failed, fatal error) {
	old := n.posture.Load().host.Load()
	if failed = n.replace(ctx, h); failed == nil {
		return nil, nil
	}
	if err := n.replace(ctx, old); err != nil {
		return failed, fmt.Errorf("bringing the old egress posture back after a failed change: %w", err)
	}
	return failed, nil
}

// replace builds section h's posture in place of the running one.
func (n *hostNet) replace(ctx context.Context, h *config.Host) error {
	cur, p := n.posture.Load(), n.newPosture(h)
	n.posture.Store(p)
	if cur.gh != nil {
		cur.gh.learner.Retire()
	}
	if err := p.guard.bring(ctx); err != nil {
		return err
	}
	return n.ready(ctx, p)
}
