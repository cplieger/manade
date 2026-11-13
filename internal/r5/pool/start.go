package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/jobspec"
	"github.com/cplieger/manade/internal/r5/obs"
)

const (
	// startTimeout bounds one start: its JIT config, then a create and a
	// start the Docker client bounds at 3 minutes each.
	startTimeout = 7 * time.Minute
	// startSettle is how long a start whose create or start call went
	// unanswered waits for the daemon to finish it, read every
	// settlePoll. A container not started by then is removed as failed.
	startSettle = 2 * time.Minute
	settlePoll  = 5 * time.Second
	// cleanupTimeout bounds the removals after a failed start, which run
	// past shutdown so no registration or container is left behind.
	cleanupTimeout = 30 * time.Second
	startsAtOnce   = 8
	// startRetry holds a scale set's starts back after one fails.
	startRetry = 10 * time.Second
	// spentFor is about one poll: by then a JobCompleted for an ended
	// runner has arrived, so a runner that ended without taking its job is
	// started again after it.
	spentFor = time.Minute
)

// runStarts is the start worker. Each poke starts the runners every scale
// set owes; it returns once ctx ends and the starts in flight are done.
func (p *Pool) runStarts(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, startsAtOnce)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		}
		for _, r := range p.owed() {
			select {
			case sem <- struct{}{}:
				wg.Go(func() {
					defer func() { <-sem }()
					p.start(ctx, r)
				})
			case <-ctx.Done():
				p.drop(r)
			}
		}
	}
}

// owed records a starting runner for each assignment a scale set holds a
// reservation for and no runner serves: max(0, its funded assignments -
// its live runners - the ended runners GitHub may still count, set.end).
// Each record takes over its assignment's slot. Unbacked assignments the
// budget now covers are funded first: GitHub never cancels an assignment
// whose scale set keeps polling, so one left unfunded would wait forever.
func (p *Pool) owed() []*runner {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []*runner
	for _, s := range p.sets {
		img := p.images[s.profile]
		if p.stopping || s.scope.public || img == "" || now.Before(s.retryAt) {
			continue
		}
		if s.asg.unbacked > 0 {
			pending := max(0, s.asg.stats.Assigned-s.asg.unbacked-len(s.runners)-s.spending(now))
			s.asg.unbacked -= min(s.asg.unbacked, max(0, p.fits(s, pending+s.asg.unbacked)-pending))
		}
		n := s.asg.stats.Assigned - s.asg.unbacked - len(s.runners) - s.spending(now)
		for range n {
			r := &runner{set: s, name: p.runnerName(s), image: img, res: p.slot(s.profile), created: now}
			s.runners[r.name] = r
			out = append(out, r)
		}
		// A start finds its tag's check due; the check runs in the
		// background, and the start uses the checked ID it has.
		if im := p.imgOf[s.profile]; n > 0 && !im.digest && now.Sub(im.tried) >= p.cfg.ImageRefresh {
			im.due = true
			wake(p.pull)
		}
	}
	return out
}

// runnerName is a new runner's name, unique within its scale set, and its
// container's: the pool, the profile and 12 random hex digits, at most 60
// bytes, inside GitHub's 64.
func (p *Pool) runnerName(s *set) string {
	for {
		name := p.cfg.PoolName + "-" + s.ss.Profile + "-" + randHex()
		if _, taken := s.runners[name]; !taken {
			return name
		}
	}
}

// randHex is 12 random hex digits.
func randHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// start registers r, then creates and starts its container from the
// checked image ID. A failure after the registration removes it, and one
// after the create removes the container. A create or start call the
// daemon left unanswered is no failure yet: the daemon may still be doing
// it, so the container is read until it shows (settle).
func (p *Pool) start(parent context.Context, r *runner) {
	ctx, cancel := context.WithTimeout(parent, startTimeout)
	defer cancel()
	s := r.set
	jit, err := p.gh.JIT(ctx, s.ss, r.name)
	if err != nil {
		p.startFailed(parent, r, "Registering a runner failed", err)
		return
	}
	req := p.host.Runner(&p.cfg.Profiles[s.profile], &jobspec.Runner{
		Scope: s.ss.Scope, Name: r.name, ImageID: r.image, JITConfig: jit.Config, ID: jit.RunnerID,
	})
	id, err := p.dk.CreateContainer(ctx, r.name, req)
	if docker.Unanswered(err) {
		id = p.lateCreate(parent, r.name, jit.RunnerID)
	}
	if id == "" {
		p.dockerFailed(parent, obs.DockerCreate)
		p.unregister(ctx, s.ss.Scope, jit.RunnerID, r.name)
		if docker.ImageMissing(err) {
			p.imageGone(s.profile, r.image)
		}
		p.startFailed(parent, r, "Creating a runner's container failed", err)
		return
	}
	if err := p.dk.StartContainer(ctx, id); err != nil {
		p.dockerFailed(parent, obs.DockerStart)
		st := settledStopped
		if docker.Unanswered(err) {
			st = p.settleStart(parent, id)
		}
		switch st {
		case settledRunning:
			p.log.Info("A runner's container start went unanswered but the container started", "scope", s.ss.Scope, "profile", s.ss.Profile, "runner", r.name, "error", err)
		case settledGone:
			p.unregister(ctx, s.ss.Scope, jit.RunnerID, r.name)
			p.startFailed(parent, r, "Starting a runner's container failed and the container is gone", err)
			return
		case settledUnread:
			p.startFailed(parent, r, "Starting a runner's container went unanswered and its state could not be read, so the start is retried and the pass settles the container", err)
			return
		default:
			p.removeContainer(ctx, r, id)
			p.unregister(ctx, s.ss.Scope, jit.RunnerID, r.name)
			p.startFailed(parent, r, "Starting a runner's container failed", err)
			return
		}
	}
	p.mu.Lock()
	r.container, r.id = id, jit.RunnerID
	// A JobStarted may already have made it busy.
	if r.state == starting {
		r.state = idle
	}
	p.mu.Unlock()
	p.m.RunnerStarted(s.ss.ScaleSet, true)
	p.log.Info("Runner started", "scope", s.ss.Scope, "profile", s.ss.Profile, "runner", r.name, "image", r.image)
}

// settled is what a container read after an unanswered call shows.
type settled int

const (
	// settledStopped is a container that never started, or no container
	// yet after a create.
	settledStopped settled = iota
	// settledRunning is a container that started; it may have exited since.
	settledRunning
	// settledGone is a container the daemon no longer has.
	settledGone
	// settledUnread is a container whose state no read answered, or a
	// wait shutdown cut short. The container and its registration are left
	// to the reconcile pass, which adopts or removes it.
	settledUnread
)

// settleStart reads container id until it shows it started or is gone, for
// at most p.settle. One still not started then is stopped: its start is
// taken as hung.
func (p *Pool) settleStart(parent context.Context, id string) settled {
	return p.poll(parent, func(ctx context.Context) (settled, bool) {
		ctr, found, err := p.dk.InspectContainer(ctx, id)
		switch {
		case err != nil:
			return settledUnread, false
		case !found:
			return settledGone, true
		case !ctr.State.StartedAt.IsZero():
			return settledRunning, true
		}
		return settledStopped, false
	})
}

// lateCreate reads the container a create left unanswered by its name, name,
// for at most p.settle, and answers its ID once it shows carrying runner
// registration runnerID, "" when it never does. A container the daemon
// creates after that is an orphan whose registration is removed, which the
// reconcile pass removes too.
func (p *Pool) lateCreate(parent context.Context, name string, runnerID int64) string {
	var id string
	p.poll(parent, func(ctx context.Context) (settled, bool) {
		ctr, found, err := p.dk.InspectContainer(ctx, name)
		switch {
		case err != nil:
			return settledUnread, false
		case !found:
			return settledStopped, false
		case ctr.Config.Labels[docker.LabelRunner] == name &&
			ctr.Config.Labels[docker.LabelManaged] == "true" &&
			ctr.Config.Labels[docker.LabelRunnerID] == strconv.FormatInt(runnerID, 10):
			id = ctr.ID
		}
		return settledStopped, true
	})
	return id
}

// poll runs read every p.settleEvery until it reports done or p.settle
// passes, and answers its last result; settledUnread when parent ends.
func (p *Pool) poll(parent context.Context, read func(context.Context) (settled, bool)) settled {
	ctx, cancel := context.WithTimeout(parent, p.settle)
	defer cancel()
	t := time.NewTicker(p.settleEvery)
	defer t.Stop()
	for {
		st, done := read(ctx)
		if done {
			return st
		}
		if parent.Err() != nil {
			return settledUnread
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return settledUnread
			}
			return st
		case <-t.C:
		}
	}
}

// startFailed drops r after a failed start. A start shutdown cut short
// is no failure: nothing will retry it.
func (p *Pool) startFailed(parent context.Context, r *runner, msg string, err error) {
	p.drop(r)
	if parent.Err() != nil {
		return
	}
	p.mu.Lock()
	r.set.retryAt = p.now().Add(startRetry)
	p.mu.Unlock()
	p.m.RunnerStarted(r.set.ss.ScaleSet, false)
	p.log.Warn(msg+", so the start is retried", "scope", r.set.ss.Scope, "profile", r.set.ss.Profile, "runner", r.name, "error", err)
}

// drop deletes r's record, which frees its reservation.
func (p *Pool) drop(r *runner) {
	p.mu.Lock()
	delete(r.set.runners, r.name)
	p.mu.Unlock()
}

// unregister removes runner id's registration in scope, which no
// container will use; name is the runner's, for the log.
func (p *Pool) unregister(ctx context.Context, scope string, id int64, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := p.gh.RemoveRunner(ctx, scope, id); err != nil {
		p.log.Warn("Removing a runner's registration failed, so it stays offline on GitHub",
			"scope", scope, "runner", name, "error", err)
	}
}

func (p *Pool) removeContainer(ctx context.Context, r *runner, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := p.dk.RemoveOwned(ctx, id); err != nil {
		p.m.DockerError(obs.DockerRemove)
		p.log.Warn("Removing a failed runner's container failed", "runner", r.name, "error", err)
	}
}
