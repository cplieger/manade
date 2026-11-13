package jobspec

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/cplieger/go/pathinside"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
)

// ErrMountRefused is a bind source that, resolved in the host's filesystem,
// reaches a protected path, lies outside the host container's mounts, or
// nests with another bind source on the host one of them writable.
var ErrMountRefused = errors.New("jobspec: a mount source is refused")

// mounts answers p's mounts. On a host that is not open each bind source is
// resolved with every symlink followed, checked, and passed to Docker
// resolved; the checks run again before every create, because the start
// binds the path following whatever links it then holds.
func (h *Host) mounts(p *config.Profile) ([]docker.Mount, error) {
	var out []docker.Mount
	for k, m := range p.Mounts {
		if !m.Bind {
			out = append(out, docker.Mount{Type: "volume", Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
			continue
		}
		src := m.Source
		if !h.host.Egress.Open {
			var err error
			if src, err = h.checkSource(p.Name, k, &m); err != nil {
				return nil, err
			}
		}
		out = append(out, docker.Mount{Type: "bind", Source: src, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return out, nil
}

// checkSource answers mount k of profile resolved, or refuses it when
// either form reaches a protected path, when the resolved form lies under
// none of the host container's mounts, or when it nests with another source
// (checkNesting).
func (h *Host) checkSource(profile string, k int, m *config.Mount) (string, error) {
	src := m.Source
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", fmt.Errorf("%w: profile %s: source %s: %w", ErrMountRefused, profile, src, err)
	}
	for _, s := range []string{src, resolved} {
		for _, prot := range h.protected {
			if pathinside.Root(prot).Contains(s) || pathinside.Root(s).Contains(prot) {
				return "", fmt.Errorf("%w: profile %s: source %s reaches %s, which host %s protects", ErrMountRefused, profile, describe(src, resolved), prot, h.host.Name)
			}
		}
	}
	for _, mnt := range h.plat.Mounts {
		if pathinside.Root(mnt).Contains(resolved) {
			return resolved, h.checkNesting(profile, k, m, resolved)
		}
	}
	return "", fmt.Errorf("%w: profile %s: source %s lies outside every mount of host %s's container; mount the path into the host service first",
		ErrMountRefused, profile, describe(src, resolved), h.host.Name)
}

// bindSource is bind mount index of profile, with its forms: as written and,
// where it differs, resolved.
type bindSource struct {
	profile  string
	forms    []string
	index    int
	writable bool
}

// bindSources answers the bind sources of every profile cfg gives host.
func bindSources(cfg *config.Config, host string) []bindSource {
	var out []bindSource
	for i := range cfg.Profiles {
		pr := &cfg.Profiles[i]
		if !slices.Contains(pr.Hosts, host) {
			continue
		}
		for k, m := range pr.Mounts {
			if m.Bind && m.Source != "" {
				out = append(out, bindSource{profile: pr.Name, forms: withResolved(m.Source), index: k, writable: !m.ReadOnly})
			}
		}
	}
	return out
}

// checkNesting refuses mount k of profile, resolved now to resolved, when
// any of its forms lies strictly under a form of another bind source jobs
// may write, or is writable itself and a strict ancestor of another's: a job
// could rename or replace a directory the other runner binds. Each other
// source's forms are its own at the host's start and now, so a link it
// held then through a writable source stays refused after a swap.
func (h *Host) checkNesting(profile string, k int, m *config.Mount, resolved string) error {
	mine := []string{resolved}
	if i := slices.IndexFunc(h.binds, func(b bindSource) bool { return b.profile == profile && b.index == k }); i >= 0 {
		mine = append(mine, h.binds[i].forms...)
	}
	for i := range h.binds {
		b := &h.binds[i]
		if b.profile == profile && b.index == k {
			continue
		}
		theirs := append(slices.Clone(b.forms), withResolved(b.forms[0])...)
		for _, a := range mine {
			for _, o := range theirs {
				if err := h.nested(profile, m, a, b, o); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// nested refuses form a of profile's mount m against form o of b.
func (h *Host) nested(profile string, m *config.Mount, a string, b *bindSource, o string) error {
	switch {
	case a == o:
		return nil
	case b.writable && pathinside.Root(o).Contains(a):
		return fmt.Errorf("%w: profile %s: source %s lies under %s, a source profile %s's jobs may write on host %s; mount the two apart",
			ErrMountRefused, profile, describe(m.Source, a), o, b.profile, h.host.Name)
	case !m.ReadOnly && pathinside.Root(a).Contains(o):
		return fmt.Errorf("%w: profile %s: source %s is writable to jobs and an ancestor of %s, a source of profile %s, on host %s; mount it read-only, or mount the two apart",
			ErrMountRefused, profile, describe(m.Source, a), o, b.profile, h.host.Name)
	}
	return nil
}

func describe(src, resolved string) string {
	if src == resolved {
		return src
	}
	return src + " (" + resolved + ")"
}

// withResolved answers p and, when it exists and differs, p with its
// symlinks resolved.
func withResolved(p string) []string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil || r == p {
		return []string{p}
	}
	return []string{p, r}
}
