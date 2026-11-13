package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/jobspec"
	"github.com/cplieger/manade/internal/r5/obs"
)

const (
	// pullFirst and pullMax bound the backoff of a missing image's pull.
	pullFirst = 30 * time.Second
	pullMax   = 10 * time.Minute
	// probeAge is how old an unrecorded probe container is before a pass
	// removes it.
	probeAge = 10 * time.Minute
)

// probeRetry spaces the removals of a probe, tried probeTries times on the
// path that created it.
var probeRetry = 2 * time.Second

const probeTries = 3

// image is one image reference the profiles use and its refresh state.
type image struct {
	ref string
	// tried is when its last check began; retry is when a missing image's
	// next pull is due, after wait.
	tried, retry time.Time
	profiles     []int
	wait         time.Duration
	// digest is a reference pinned by digest, never checked again; shipped
	// is a shipped image, whose superseded IDs the pool removes.
	digest, shipped bool
	// pulling holds one check in flight; due asks for the next.
	pulling, due bool
}

// missing reports a profile of img with no checked image ID; p.mu is held.
func (p *Pool) missing(img *image) bool {
	return slices.ContainsFunc(img.profiles, func(pr int) bool { return p.images[pr] == "" })
}

// runImages runs the image checks due until ctx ends: a tag a start found
// past images.refresh, and a missing image whose backoff has passed. It
// returns once ctx ends and the checks in flight are done.
func (p *Pool) runImages(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	t := time.NewTicker(proofTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.pull:
		case <-t.C:
		}
		for _, img := range p.dueImages() {
			wg.Go(func() { _ = p.refresh(ctx, img, true) })
		}
	}
}

// dueImages marks each image due now as in flight and returns them.
func (p *Pool) dueImages() []*image {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []*image
	for _, img := range p.imgs {
		if img.pulling || !img.due && (!p.missing(img) || now.Before(img.retry)) {
			continue
		}
		img.pulling, img.due, img.tried = true, false, now
		out = append(out, img)
	}
	return out
}

// refresh brings img's current image into service, pulling it when check
// is set or the cache lacks it; a tag the cache served is due for a
// check. A new ID serves each profile from its next start once it passes
// the user check. A failure keeps the checked image in
// service and puts a missing one on backoff. It returns the failure.
func (p *Pool) refresh(ctx context.Context, img *image, check bool) error {
	began := p.now()
	got, pulled, err := p.fetch(ctx, img.ref, check)
	if err == nil {
		err = p.admit(ctx, img, &got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	img.pulling = false
	if ctx.Err() != nil {
		return err
	}
	now := p.now()
	switch {
	case err != nil:
		p.log.Warn("Refreshing a profile's image failed, so the image already checked stays in service and a missing one is retried",
			"image", img.ref, "error", err)
	case pulled:
		// Boot's pull of a missing image is also the tag's check, so the
		// first start finds none due.
		if !check {
			img.tried = began
		}
		for _, pr := range img.profiles {
			p.m.ImageChecked(p.cfg.Profiles[pr].Name, now)
		}
	case !check && !img.digest:
		// The cache served a tag boot did not pull. Its last check is
		// unknown to this process, so it is due at once; waiting for a
		// start leaves its check time at 0, which ManadeImageStale reads
		// as never pulled.
		img.due = true
		wake(p.pull)
	}
	if !p.missing(img) {
		img.wait = 0
	} else if err != nil {
		img.wait = min(max(2*img.wait, pullFirst), pullMax)
		img.retry = now.Add(img.wait)
	}
	return err
}

var errNotCached = errors.New("docker: the pulled image is not in the cache")

// fetch reads ref from the cache, pulling it first when check is set or
// the cache lacks it, and reports whether it pulled.
func (p *Pool) fetch(ctx context.Context, ref string, check bool) (docker.Image, bool, error) {
	got, found, err := p.dk.InspectImage(ctx, ref)
	if err != nil {
		p.dockerFailed(ctx, obs.DockerInspect)
		return got, false, err
	}
	if found && !check {
		return got, false, nil
	}
	if err = p.dk.PullImage(ctx, ref); err != nil {
		p.dockerFailed(ctx, obs.DockerPull)
		return got, false, err
	}
	got, found, err = p.dk.InspectImage(ctx, ref)
	if err == nil && !found {
		err = errNotCached
	}
	if err != nil {
		p.dockerFailed(ctx, obs.DockerInspect)
	}
	return got, true, err
}

// admit puts got in service for each profile of img not already on it.
// A profile that runs as the image's user takes it only once the user
// check passes; a refusal counts a pull error.
func (p *Pool) admit(ctx context.Context, img *image, got *docker.Image) error {
	var userErr error
	checked := false
	for _, pr := range img.profiles {
		p.mu.Lock()
		current := p.images[pr]
		p.mu.Unlock()
		if current == got.ID {
			continue
		}
		if jobspec.NeedsUserCheck(&p.cfg.Profiles[pr]) {
			if !checked {
				checked, userErr = true, p.checkUser(ctx, pr, got)
			}
			if userErr != nil {
				continue
			}
		}
		p.mu.Lock()
		if img.shipped && current != "" && !slices.Contains(p.old, current) {
			p.old = append(p.old, current)
		}
		p.mu.Unlock()
		p.setImage(pr, got.ID)
		p.log.Info("A profile's image is in service", "profile", p.cfg.Profiles[pr].Name, "image", img.ref, "id", got.ID)
	}
	return userErr
}

// checkUser resolves got's user through its own files, read from a user
// probe created on profile pr.
func (p *Pool) checkUser(ctx context.Context, pr int, got *docker.Image) error {
	var f jobspec.Files
	err := p.probe(ctx, pr, jobspec.ProbeUser, got.ID, func(id string) error {
		var errs [2]error
		f.Passwd, _, errs[0] = p.dk.ReadFile(ctx, id, jobspec.PasswdPath, jobspec.UserFileLimit)
		f.Group, _, errs[1] = p.dk.ReadFile(ctx, id, jobspec.GroupPath, jobspec.UserFileLimit)
		return errors.Join(errs[:]...)
	})
	if err != nil {
		return err
	}
	if err := jobspec.CheckUser(got.Config.User, f); err != nil {
		p.m.DockerError(obs.DockerPull)
		return fmt.Errorf("profile %s: %w", p.cfg.Profiles[pr].Name, err)
	}
	return nil
}

// probe creates a probe container of kind from image id on profile pr,
// hands it to fn when fn is set, and removes it. The record keeps the
// reconciler off it while this call runs; a removal that still fails after
// probeTries is left to the reconciler's probeAge rule.
func (p *Pool) probe(ctx context.Context, pr int, kind jobspec.ProbeKind, id string, fn func(id string) error) error {
	name := p.cfg.PoolName + "-probe-" + randHex()
	cid, err := p.dk.CreateContainer(ctx, name, p.host.Probe(&p.cfg.Profiles[pr], kind, id))
	if err != nil {
		p.dockerFailed(ctx, obs.DockerCreate)
		return err
	}
	p.mu.Lock()
	p.probes[cid] = true
	p.mu.Unlock()
	defer p.dropProbe(ctx, cid)
	if fn == nil {
		return nil
	}
	if err := fn(cid); err != nil {
		p.dockerFailed(ctx, obs.DockerArchive)
		return err
	}
	return nil
}

func (p *Pool) dropProbe(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	defer func() {
		p.mu.Lock()
		delete(p.probes, id)
		p.mu.Unlock()
	}()
	for try := 1; ; try++ {
		err := p.dk.RemoveOwned(ctx, id)
		if err == nil {
			return
		}
		p.m.DockerError(obs.DockerRemove)
		if try == probeTries {
			p.log.Warn("Removing a probe container failed, so a reconciler pass removes it later", "container", id, "error", err)
			return
		}
		t := time.NewTimer(probeRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// tidyImages is the reconciler's image work: a digest's image found in the
// cache moves its check time, one gone from it is missing again, and a
// superseded shipped image no runner and no profile uses is removed. An
// image a container outside the pool uses is refused by the daemon and
// kept for the next pass.
func (p *Pool) tidyImages(ctx context.Context) {
	p.mu.Lock()
	var digests []*image
	for _, img := range p.imgs {
		if img.digest && !p.missing(img) {
			digests = append(digests, img)
		}
	}
	used := map[string]bool{}
	for _, id := range p.images {
		used[id] = true
	}
	for _, s := range p.sets {
		for _, r := range s.runners {
			used[r.image] = true
		}
	}
	old := slices.DeleteFunc(slices.Clone(p.old), func(id string) bool { return used[id] })
	p.mu.Unlock()
	for _, img := range digests {
		p.cached(ctx, img)
	}
	for _, id := range old {
		if p.removeOld(ctx, id) {
			p.mu.Lock()
			p.old = slices.DeleteFunc(p.old, func(o string) bool { return o == id })
			p.mu.Unlock()
		}
	}
}

// removeOld removes a superseded shipped image, and reports whether the
// pool is done with it: removed, or not the pool's to remove.
func (p *Pool) removeOld(ctx context.Context, id string) bool {
	switch err := p.dk.RemoveOwnedImage(ctx, id); {
	case err == nil:
		p.log.Info("Removed a superseded runner image", "id", id)
	case errors.Is(err, docker.ErrConflict):
		return false
	case !errors.Is(err, docker.ErrNotOwned):
		if p.dockerFailed(ctx, obs.DockerRemove) {
			p.log.Warn("Removing a superseded runner image failed, so the pass retries it", "id", id, "error", err)
		}
		return false
	}
	return true
}

// cached checks that a digest's image is still in the cache.
func (p *Pool) cached(ctx context.Context, img *image) {
	_, found, err := p.dk.InspectImage(ctx, img.ref)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	switch {
	case err != nil:
		p.dockerFailed(ctx, obs.DockerInspect)
	case !found:
		for _, pr := range img.profiles {
			p.images[pr] = ""
		}
		img.retry = now
		wake(p.pull)
		p.log.Warn("A profile's image left the cache, so the pool pulls it again", "image", img.ref)
	default:
		for _, pr := range img.profiles {
			p.m.ImageChecked(p.cfg.Profiles[pr].Name, now)
		}
	}
}

// imageGone takes id out of service for every profile of pr's image once
// the daemon reports it gone, so the image counts as missing: nothing is
// funded on it and it is pulled again at once, instead of every start
// failing on an ID that no longer exists.
func (p *Pool) imageGone(pr int, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	img := p.imgOf[pr]
	gone := false
	for _, q := range img.profiles {
		if p.images[q] == id {
			p.images[q] = ""
			gone = true
		}
	}
	if !gone {
		return
	}
	img.due, img.retry, img.wait = true, time.Time{}, 0
	wake(p.pull)
	p.log.Warn("A profile's image is gone from Docker's cache, so it is pulled again", "image", img.ref, "id", id)
}
