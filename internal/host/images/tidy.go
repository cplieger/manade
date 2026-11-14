package images

import (
	"context"
	"errors"

	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/jobspec"
)

// tidyState is what the tidy pass must keep, read under m.mu.
type tidyState struct {
	inService, checking, named map[string]bool
	probeID                    string
}

func (m *Manager) tidyState() tidyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := tidyState{inService: map[string]bool{}, checking: map[string]bool{}, named: map[string]bool{}, probeID: m.probeID}
	for _, p := range m.profiles {
		if p.id != "" {
			s.inService[p.id] = true
		}
	}
	for id := range m.checking {
		s.checking[id] = true
	}
	for name := range m.refs {
		s.named[name] = true
	}
	return s
}

// tidy removes what nothing uses any more: an image check's container whose
// check ended, a leftover probe container (Keep.ProbeLeftover), a
// docker-image runner's volume whose runner is gone, and each image the
// host pulled that no profile names and no container uses, or that a newer
// one superseded. The current probe image always stays. A removal the
// daemon refuses with a conflict is tried again at the next pass.
func (m *Manager) tidy(ctx context.Context) {
	containers, err := m.daemon.ListContainers(ctx)
	if err != nil {
		m.log.Warn("The tidy pass could not list containers, so it removes nothing this time", "err", err)
		return
	}
	s := m.tidyState()
	runners := map[string]bool{}
	for i := range containers {
		c := &containers[i]
		if name := c.Labels[jobspec.LabelRunner]; name != "" {
			runners[name] = true
		}
		if m.spent(c, &s) {
			m.remove("container", c.ID, m.daemon.RemoveOwned(ctx, c.ID))
		}
	}
	m.tidyVolumes(ctx, runners)
	m.tidyImages(ctx, &s)
}

// spent reports a probe or image check container the pass removes.
func (m *Manager) spent(c *docker.Summary, s *tidyState) bool {
	if id := c.Labels[jobspec.LabelImageCheck]; id != "" {
		return !s.checking[id]
	}
	run := c.Labels[jobspec.LabelProbe]
	return run != "" && m.keep.ProbeLeftover(run)
}

func (m *Manager) tidyVolumes(ctx context.Context, runners map[string]bool) {
	vols, err := m.daemon.ListVolumes(ctx)
	if err != nil {
		m.log.Warn("The tidy pass could not list volumes, so it removes none this time", "err", err)
		return
	}
	for _, v := range vols {
		name := v.Labels[jobspec.LabelRunner]
		if name != "" && !runners[name] && !m.keep.Runner(name) {
			m.remove("volume", v.Name, m.daemon.RemoveOwnedVolume(ctx, v.Name))
		}
	}
}

func (m *Manager) tidyImages(ctx context.Context, s *tidyState) {
	imgs, err := m.daemon.ListImages(ctx)
	if err != nil {
		m.log.Warn("The tidy pass could not list images, so it removes none this time", "err", err)
		return
	}
	repos := map[string]bool{}
	for name := range s.named {
		repos[repository(name)] = true
	}
	tagged := map[string]bool{}
	for i := range imgs {
		img := &imgs[i]
		removed := map[string]bool{}
		for _, ref := range m.removable(img, s, repos) {
			err := m.daemon.RemoveImage(ctx, ref)
			m.remove("image", ref, err)
			removed[ref] = err == nil
		}
		for _, t := range img.RepoTags {
			tagged[t] = tagged[t] || !removed[t]
		}
	}
	m.rec.keep(ctx, m.log, func(ref string) bool { return s.named[ref] || tagged[ref] })
}

// removable answers what the pass removes of img: the ID of an untagged
// image the host pulled (its repository is one the host pulled, or it is a
// shipped image), or each tag the host pulled that no profile names. An
// image in service or in a check stays, as does any
// tag a profile names. The daemon refuses an image a container uses, which
// keeps it for a later pass.
func (m *Manager) removable(img *docker.ImageSummary, s *tidyState, repos map[string]bool) []string {
	switch {
	case img.Labels[docker.LabelProbeImage] != "":
		if s.probeID == "" || img.ID == s.probeID {
			return nil
		}
		return refsOrID(img.RepoTags, img.ID)
	case s.inService[img.ID] || s.checking[img.ID]:
		return nil
	case len(img.RepoTags) == 0:
		if img.Labels[docker.LabelRunnerImage] != "" || m.pulledRepo(img.RepoDigests, repos) {
			return []string{img.ID}
		}
		return nil
	}
	var out []string
	for _, t := range img.RepoTags {
		if s.named[t] {
			return nil
		}
		if m.rec.has(t) {
			out = append(out, t)
		}
	}
	return out
}

// pulledRepo reports a repository digest of a repository the host pulled
// from: one it names now or one in its record.
func (m *Manager) pulledRepo(digests []string, repos map[string]bool) bool {
	for _, d := range digests {
		repo := repository(d)
		if repos[repo] || m.rec.hasRepo(repo) {
			return true
		}
	}
	return false
}

func refsOrID(tags []string, id string) []string {
	if len(tags) == 0 {
		return []string{id}
	}
	return tags
}

// remove logs a removal's failure; a conflict, an object still in use, is
// only noted, since the next pass tries it again.
func (m *Manager) remove(kind, ref string, err error) {
	switch {
	case err == nil:
		m.log.Info("The tidy pass removed an unused "+kind, kind, ref)
	case errors.Is(err, docker.ErrConflict):
		m.log.Debug("The tidy pass left a "+kind+" still in use for its next pass", kind, ref)
	default:
		m.log.Warn("The tidy pass could not remove a "+kind+", so its next pass tries again", kind, ref, "err", err)
	}
}
