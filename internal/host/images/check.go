package images

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/jobspec"
)

// cleanupTimeout bounds the removal of an image check's container, which
// runs even after the check's context ended.
const cleanupTimeout = docker.CallTimeout

// errNotCached is a pull that succeeded with the reference still missing
// from the daemon's cache.
var errNotCached = errors.New("the pulled image is not in the daemon's cache")

// check brings r's current image into service: resolve a registry an
// internet: false host must check, then inspect the cache, ask a tag's
// digest, pull what is missing or moved, and check what is new.
func (m *Manager) check(ctx context.Context, r *ref) {
	served, err := m.resolve(ctx, r)
	if err == nil && served {
		err = m.bring(ctx, r)
	}
	defer m.kick()
	m.mu.Lock()
	defer m.mu.Unlock()
	r.busy = false
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	if err != nil {
		m.log.Warn("Bringing a profile's image into service failed, so any image already checked stays in service",
			"image", r.name, "err", err)
		for _, p := range r.profiles {
			p.fail(now, err.Error())
		}
	}
	if slices.ContainsFunc(r.profiles, func(p *profile) bool { return p.id == "" }) && r.refused == nil {
		r.wait = min(max(2*r.wait, retryFirst), retryMax)
		r.retry = now.Add(r.wait)
	} else {
		r.wait = 0
	}
}

// bring is check's daemon work. A pinned reference is only inspected, its
// good check; a tag found in the cache has its digest asked, and only a
// moved digest pulls. A cached image whose ask fails stays the one served.
// Its error is a failure of every profile of r; admit records its own.
func (m *Manager) bring(ctx context.Context, r *ref) error {
	m.mu.Lock()
	r.checked = time.Now()
	m.mu.Unlock()
	img, found, err := m.daemon.InspectImage(ctx, r.name)
	if err != nil {
		return err
	}
	if !found {
		m.gone(r)
		if img, err = m.pull(ctx, r); err != nil {
			return err
		}
		m.admit(ctx, r, &img, true)
		return nil
	}
	if !r.pinned {
		var digest string
		auth, err := m.auth(ctx, r.name)
		if err == nil {
			digest, err = m.daemon.Digest(ctx, r.name, auth)
		}
		if err != nil {
			m.admit(ctx, r, &img, false)
			return fmt.Errorf("asking the registry for %s's digest: %w", r.name, err)
		}
		if digestOf(r.name, &img) != digest {
			if img, err = m.pull(ctx, r); err != nil {
				return err
			}
		}
	}
	m.admit(ctx, r, &img, true)
	return nil
}

// gone takes r's images out of service when the daemon no longer holds the
// reference, as Missing does for a create.
func (m *Manager) gone(r *ref) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, p := range r.profiles {
		if p.id != "" {
			p.id, p.digest, p.missing = "", "", true
			p.fail(now, "the daemon no longer holds "+r.name)
			m.log.Warn("A profile's image is gone from the host's daemon, so it is pulled again", "profile", p.cfg.Name, "image", r.name)
		}
	}
}

// pull pulls r, records it as the host's, and inspects what it pulled.
func (m *Manager) pull(ctx context.Context, r *ref) (docker.Image, error) {
	auth, err := m.auth(ctx, r.name)
	if err != nil {
		return docker.Image{}, err
	}
	if err = m.daemon.PullImage(ctx, r.name, auth); err != nil {
		return docker.Image{}, fmt.Errorf("pulling %s: %w", r.name, err)
	}
	m.rec.add(ctx, r.name, m.log)
	img, found, err := m.daemon.InspectImage(ctx, r.name)
	if err == nil && !found {
		err = errNotCached
	}
	if err != nil {
		return docker.Image{}, fmt.Errorf("pulling %s: %w", r.name, err)
	}
	return img, nil
}

// admit puts img in service for each profile of r it passes and, when good
// (the tag's digest was asked, or a pinned image found), records a good
// check for each profile it serves. A profile it fails keeps the image it
// had, and the failure is recorded on it.
func (m *Manager) admit(ctx context.Context, r *ref, img *docker.Image, good bool) {
	m.mu.Lock()
	m.checking[img.ID]++
	profiles := slices.Clone(r.profiles)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.checking[img.ID]--
		if m.checking[img.ID] == 0 {
			delete(m.checking, img.ID)
		}
		m.mu.Unlock()
	}()
	digest := digestOf(r.name, img)
	var userErr error
	userChecked := false
	for _, p := range profiles {
		m.mu.Lock()
		current := p.id
		m.mu.Unlock()
		err := shippedLabel(p.cfg.RunnerImage, img)
		if err == nil && current != img.ID && jobspec.NeedsUserCheck(p.cfg) {
			if !userChecked {
				userChecked, userErr = true, m.checkUser(ctx, img)
			}
			err = userErr
		}
		m.mu.Lock()
		switch {
		case err != nil:
			p.fail(time.Now(), err.Error())
			m.log.Warn("A profile's image failed its check, so the image already in service stays", "profile", p.cfg.Name, "image", r.name, "id", img.ID, "err", err)
		case current == img.ID:
			p.digest = digest
		default:
			p.id, p.digest, p.missing = img.ID, digest, false
			m.log.Info("A profile's image is in service", "profile", p.cfg.Name, "image", r.name, "id", img.ID)
		}
		if err == nil && good {
			p.checkedAt = time.Now()
		}
		m.mu.Unlock()
	}
}

// errNotShipped is a shipped image's reference serving an image without its
// label: not a job image this host's contract was built for.
var errNotShipped = errors.New("the image lacks its manade.runner-image label")

func shippedLabel(name string, img *docker.Image) error {
	if name == "" {
		return nil
	}
	if got := img.Config.Labels[docker.LabelRunnerImage]; got != name {
		return fmt.Errorf("%w: want %s=%s, image %s has %q", errNotShipped, docker.LabelRunnerImage, name, img.ID, got)
	}
	return nil
}

// checkUser resolves img's user through its own passwd and group files,
// read from a container created from it and never started.
func (m *Manager) checkUser(ctx context.Context, img *docker.Image) error {
	res := m.daemon.Create(ctx, "manade-image-check-"+randHex(), jobspec.ImageCheck(img.ID))
	if res.ID != "" {
		defer m.removeQuietly(ctx, res.ID)
	}
	if res.Outcome != docker.Done {
		if res.Err == nil {
			res.Err = errors.New("the create's outcome is unknown")
		}
		return fmt.Errorf("creating the user check's container: %w", res.Err)
	}
	var f jobspec.Files
	var errs [2]error
	f.Passwd, _, errs[0] = m.daemon.ReadFile(ctx, res.ID, jobspec.PasswdPath, jobspec.UserFileLimit)
	f.Group, _, errs[1] = m.daemon.ReadFile(ctx, res.ID, jobspec.GroupPath, jobspec.UserFileLimit)
	if err := errors.Join(errs[:]...); err != nil {
		return fmt.Errorf("reading the image's user files: %w", err)
	}
	return jobspec.CheckUser(img.Config.User, f)
}

// removeQuietly removes an image check's container; one it leaves is the
// tidy pass's.
func (m *Manager) removeQuietly(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := m.daemon.RemoveOwned(ctx, id); err != nil {
		m.log.Warn("Removing an image check's container failed, so the tidy pass removes it", "container", id, "err", err)
	}
}

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}
