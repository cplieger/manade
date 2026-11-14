package images

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"

	"github.com/cplieger/go/filesafe"
)

// recordLimit caps the record file: a list of image references.
const recordLimit = 1 << 20

func recordPath(dir string) string { return filepath.Join(dir, "pulled-images.json") }

// record is the references this host pulled, kept across restarts, so the
// tidy pass removes only what the host brought in: an image a job built or
// pulled through an open host's socket is never the host's to remove.
type record struct {
	refs map[string]bool
	path string
	mu   sync.Mutex
}

// load reads the saved record; a missing or unreadable one starts empty,
// which only keeps more images.
func (r *record) load(ctx context.Context, log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = map[string]bool{}
	b, err := filesafe.ReadBounded(ctx, r.path, recordLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var refs []string
	if err == nil {
		err = json.Unmarshal(b, &refs)
	}
	if err != nil {
		log.Warn("The record of pulled images was not read, so the tidy pass keeps the images it named", "path", r.path, "err", err)
		return
	}
	for _, ref := range refs {
		r.refs[ref] = true
	}
}

// has reports a reference the host pulled.
func (r *record) has(ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refs[ref]
}

// hasRepo reports a repository a reference the host pulled names.
func (r *record) hasRepo(repo string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ref := range r.refs {
		if repository(ref) == repo {
			return true
		}
	}
	return false
}

// add records ref and saves the record when it is new.
func (r *record) add(ctx context.Context, ref string, log *slog.Logger) {
	r.update(ctx, log, func(refs map[string]bool) { refs[ref] = true })
}

// keep drops every reference keep does not report.
func (r *record) keep(ctx context.Context, log *slog.Logger, keep func(ref string) bool) {
	r.update(ctx, log, func(refs map[string]bool) { maps.DeleteFunc(refs, func(ref string, _ bool) bool { return !keep(ref) }) })
}

func (r *record) update(ctx context.Context, log *slog.Logger, fn func(map[string]bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs == nil {
		r.refs = map[string]bool{}
	}
	had := maps.Clone(r.refs)
	fn(r.refs)
	if maps.Equal(had, r.refs) {
		return
	}
	b, _ := json.Marshal(slices.Sorted(maps.Keys(r.refs))) // a list of strings always encodes
	if _, err := filesafe.WriteFile(ctx, r.path, b, filesafe.WithMode(0o600), filesafe.WithMkdirAll(true)); err != nil {
		log.Warn("Saving the record of pulled images failed, so a restart may keep an image the host pulled", "path", r.path, "err", err)
	}
}
