package jobspec

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cplieger/go/pathinside"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
)

// ErrMountRefused is a bind source that, resolved in the host's filesystem,
// reaches a protected path or lies outside the host container's mounts.
var ErrMountRefused = errors.New("jobspec: a mount source is refused")

// mounts answers p's mounts. On a host that is not open each bind source is
// resolved with every symlink followed and passed to Docker resolved, so a
// link swapped in after this check is not what the create binds; Docker
// follows a source's links at every create, so the check holds only for the
// create that follows it.
func (h *Host) mounts(p *config.Profile) ([]docker.Mount, error) {
	var out []docker.Mount
	for _, m := range p.Mounts {
		if !m.Bind {
			out = append(out, docker.Mount{Type: "volume", Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
			continue
		}
		src := m.Source
		if !h.host.Egress.Open {
			var err error
			if src, err = h.checkSource(p.Name, m.Source); err != nil {
				return nil, err
			}
		}
		out = append(out, docker.Mount{Type: "bind", Source: src, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return out, nil
}

// checkSource answers src resolved, or refuses it when either form reaches
// a protected path, or when the resolved form lies under none of the host
// container's mounts.
func (h *Host) checkSource(profile, src string) (string, error) {
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
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%w: profile %s: source %s lies outside every mount of host %s's container; mount the path into the host service first",
		ErrMountRefused, profile, describe(src, resolved), h.host.Name)
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
