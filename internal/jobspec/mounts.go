package jobspec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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

// checkSource answers mount k of profile resolved, or refuses it when any
// form of it reaches a protected path, when it resolves under none of the
// host container's mounts, or when it nests with another source
// (checkNesting).
func (h *Host) checkSource(profile string, k int, m *config.Mount) (string, error) {
	src := m.Source
	w, err := resolve(src)
	if err != nil {
		return "", fmt.Errorf("%w: profile %s: source %s: %w", ErrMountRefused, profile, src, err)
	}
	for _, s := range w.forms {
		for _, prot := range h.protected {
			if pathinside.Root(prot).Contains(s) || pathinside.Root(s).Contains(prot) {
				return "", fmt.Errorf("%w: profile %s: source %s reaches %s, which host %s protects", ErrMountRefused, profile, describe(src, s), prot, h.host.Name)
			}
		}
	}
	for _, mnt := range h.plat.Mounts {
		if pathinside.Root(mnt).Contains(w.final) {
			return w.final, h.checkNesting(profile, k, m, &w)
		}
	}
	return "", fmt.Errorf("%w: profile %s: source %s lies outside every mount of host %s's container; mount the path into the host service first",
		ErrMountRefused, profile, describe(src, w.final), h.host.Name)
}

// checkNesting refuses mount k of profile, walked as mine, when a path its
// walk read lies strictly under a form of another source jobs may write, or
// when it is writable and a form of it is a strict ancestor of a path
// another source's walk reads: a job could rename or replace an entry the
// other runner's bind goes through. Every source is walked again at each
// create, and every entry a walk reads before its first one under a
// writable source is out of any job's reach, so the answer is the same
// whenever the Host was built.
func (h *Host) checkNesting(profile string, k int, m *config.Mount, mine *walk) error {
	for i := range h.binds {
		b := &h.binds[i]
		if b.Profile == profile && b.Index == k {
			continue
		}
		// A walk that fails still answers what it read, which is what
		// decides the nesting; that source's own create refuses it.
		theirs, _ := resolve(b.Mount.Source)
		if !b.Mount.ReadOnly {
			if a, o, ok := strictlyUnder(mine.read(), theirs.forms); ok {
				return fmt.Errorf("%w: profile %s: source %s lies under %s, a source profile %s's jobs may write on host %s; mount the two apart",
					ErrMountRefused, profile, describe(m.Source, a), o, b.Profile, h.host.Name)
			}
		}
		if !m.ReadOnly {
			if o, a, ok := strictlyUnder(theirs.read(), mine.forms); ok {
				return fmt.Errorf("%w: profile %s: source %s is writable to jobs and an ancestor of %s, a source of profile %s, on host %s; mount it read-only, or mount the two apart",
					ErrMountRefused, profile, describe(m.Source, a), o, b.Profile, h.host.Name)
			}
		}
	}
	return nil
}

// strictlyUnder answers the first path of paths strictly under one of
// roots, and that root.
func strictlyUnder(paths, roots []string) (path, root string, ok bool) {
	for _, p := range paths {
		for _, r := range roots {
			if p != r && pathinside.Root(r).Contains(p) {
				return p, r, true
			}
		}
	}
	return "", "", false
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

// maxLinks is filepath.EvalSymlinks's bound on the links one walk follows.
const maxLinks = 255

var errTooManyLinks = errors.New("too many links")

// walk is an absolute path resolved one component at a time, as
// filepath.EvalSymlinks does: final is the path it resolves to; forms are
// every spelling of it the walk went through, from as written to final;
// lookups are every entry the walk read, each link included, so a writer of
// any of their directories decides where the walk goes.
type walk struct {
	final   string
	forms   []string
	lookups []string
}

// read answers every path w read or was spelled as.
func (w *walk) read() []string { return append(slices.Clone(w.lookups), w.forms...) }

// resolve walks src. On an error it answers what the walk reached.
func resolve(src string) (walk, error) {
	w := walk{forms: []string{src}}
	dest, rest, links := "/", src, 0
	for rest != "" {
		var name string
		var more bool
		name, rest, more = strings.Cut(strings.TrimLeft(rest, "/"), "/")
		switch name {
		case "", ".":
			continue
		case "..":
			dest = filepath.Dir(dest)
			continue
		}
		dest = filepath.Join(dest, name)
		link, isLink, err := w.step(dest, more)
		switch {
		case err != nil:
			return w, err
		case !isLink:
			continue
		}
		links++
		if links > maxLinks {
			return w, errTooManyLinks
		}
		w.addForm(filepath.Join(dest, rest))
		if filepath.IsAbs(link) {
			dest = "/"
		} else {
			dest = filepath.Dir(dest)
		}
		if more {
			link += "/" + rest
		}
		rest = link
		w.addForm(filepath.Join(dest, rest))
	}
	w.final = dest
	w.addForm(dest)
	return w, nil
}

// step reads entry dest, a directory when more of the path follows, and
// answers its target when it is a link.
func (w *walk) step(dest string, more bool) (link string, isLink bool, err error) {
	w.lookups = append(w.lookups, dest)
	fi, err := os.Lstat(dest)
	switch {
	case err != nil:
		return "", false, err
	case fi.Mode()&fs.ModeSymlink == 0 && more && !fi.IsDir():
		return "", false, syscall.ENOTDIR
	case fi.Mode()&fs.ModeSymlink == 0:
		return "", false, nil
	}
	link, err = os.Readlink(dest)
	return link, err == nil, err
}

func (w *walk) addForm(p string) {
	if !slices.Contains(w.forms, p) {
		w.forms = append(w.forms, p)
	}
}
