// Package images keeps a runner host's job images in service. It pulls what
// the host's profiles name, checks each image before any runner is created
// from it, asks the registry for each tag's digest beside the starts and at
// every images.refresh, takes a vanished image out of service and pulls it
// again, and tidies what nothing uses any more. It also builds the probe
// image from the host's own binary and loads it.
package images

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// Daemon is the host daemon's image, container and volume calls.
type Daemon interface {
	InspectImage(ctx context.Context, ref string) (docker.Image, bool, error)
	PullImage(ctx context.Context, ref, auth string) error
	Digest(ctx context.Context, ref, auth string) (string, error)
	LoadImage(ctx context.Context, tar io.Reader) error
	ListImages(ctx context.Context) ([]docker.ImageSummary, error)
	RemoveImage(ctx context.Context, ref string) error
	Create(ctx context.Context, name string, req *docker.CreateRequest) docker.Result
	ReadFile(ctx context.Context, id, p string, limit int64) ([]byte, bool, error)
	RemoveOwned(ctx context.Context, ref string) error
	ListContainers(ctx context.Context) ([]docker.Summary, error)
	ListVolumes(ctx context.Context) ([]docker.Volume, error)
	RemoveOwnedVolume(ctx context.Context, name string) error
}

var _ Daemon = (*docker.Client)(nil)

// The backoff of a profile with no image in service, and the least time
// between two digest asks a tag's starts make.
const (
	retryFirst = 15 * time.Second
	retryMax   = 5 * time.Minute
	startCheck = time.Minute
)

// Keep is what only the rest of the host knows the tidy pass must leave.
// Both funcs are required.
type Keep struct {
	// Runner reports a runner the host still records, whose docker volume
	// stays even while it has no container.
	Runner func(name string) bool
	// ProbeLeftover reports a container of probe run that the pass removes:
	// probe.Runner.Leftover.
	ProbeLeftover func(run string) bool
}

// Config is what a Manager is built from.
type Config struct {
	Daemon Daemon
	Host   *config.Host
	// Lookup resolves a registry's name through the host's resolver.
	Lookup func(ctx context.Context, name string) ([]netip.Addr, error)
	// ByName reports a name an internet: false host reaches by name:
	// GitHub's domain list.
	ByName func(name string) bool
	// Tidied, if set, is called after each tidy pass has ended.
	Tidied func()
	Log    *slog.Logger
	Keep   Keep
	// Version is the host's build version, which picks the shipped
	// images' tags and names the probe image.
	Version string
	// StateDir holds the record of the references the host pulled.
	StateDir string
	// Binary is the host's own executable, the probe image's one file.
	Binary string
	// Profiles are the profiles the host serves, each passing its checks.
	Profiles []*config.Profile
}

// Manager holds each profile's image in service. Its methods are safe for
// concurrent use.
type Manager struct {
	daemon   Daemon
	lookup   func(ctx context.Context, name string) ([]netip.Addr, error)
	byName   func(name string) bool
	tidied   func()
	log      *slog.Logger
	keep     Keep
	wake     chan struct{}
	host     *config.Host
	refs     map[string]*ref
	version  string
	binary   string
	rec      record
	probeID  string
	checking map[string]int
	profiles []*profile
	// tidyAsked is a tidy pass Tidy asked for, not yet begun.
	tidyAsked bool
	mu        sync.Mutex
}

// ref is one image reference the profiles name, and its check state.
type ref struct {
	refused *refusal
	// checked is when the registry was last asked for the tag's digest, or
	// a pinned image last inspected; resolved when a refused registry was
	// last resolved.
	checked, resolved, retry time.Time
	name                     string
	profiles                 []*profile
	wait                     time.Duration
	// pinned is a digest reference, never asked of the registry; due asks
	// for a check now; busy holds one check in flight.
	pinned, due, busy bool
}

// profile is one profile's image in service and its last failure.
type profile struct {
	cfg                 *config.Profile
	ref                 *ref
	checkedAt, failedAt time.Time
	id, digest          string
	failure             string
	failures            uint64
	// missing is an image the daemon lost, until another comes into
	// service.
	missing bool
}

func (p *profile) ready() bool { return p.id != "" && p.ref.refused == nil }

// New answers a Manager for cfg. No image is in service until Run.
func New(cfg *Config) *Manager {
	m := &Manager{
		daemon: cfg.Daemon, lookup: cfg.Lookup, byName: cfg.ByName, log: cfg.Log, keep: cfg.Keep, tidied: cfg.Tidied,
		version: cfg.Version, binary: cfg.Binary, wake: make(chan struct{}, 1),
		rec: record{path: recordPath(cfg.StateDir)}, checking: map[string]int{},
	}
	m.set(cfg.Host, cfg.Profiles)
	return m
}

// set replaces the host section and the profiles, keeping the state of each
// profile whose image and check stay the same; m.mu is held or m is new.
func (m *Manager) set(h *config.Host, profiles []*config.Profile) {
	old := map[string]*profile{}
	for _, p := range m.profiles {
		old[p.cfg.Name] = p
	}
	refs := map[string]*ref{}
	m.profiles = m.profiles[:0:0]
	for _, c := range profiles {
		name := jobspec.ImageRef(c, m.version)
		r, ok := refs[name]
		if !ok {
			r = m.refs[name]
			if r == nil {
				r = &ref{name: name, pinned: pinned(name)}
			}
			r.profiles, r.due = nil, true
			refs[name] = r
		}
		p := old[c.Name]
		if p == nil || p.ref != r || !sameCheck(p.cfg, c) {
			p = &profile{ref: r}
		}
		p.cfg = c
		r.profiles = append(r.profiles, p)
		m.profiles = append(m.profiles, p)
	}
	m.host, m.refs = h, refs
}

// sameCheck reports whether an image a passed for profile a also passes
// for b.
func sameCheck(a, b *config.Profile) bool {
	return a.RunnerImage == b.RunnerImage && jobspec.NeedsUserCheck(a) == jobspec.NeedsUserCheck(b)
}

// Reload takes the host section and profiles a SIGHUP read. A profile whose
// image and check are unchanged keeps its image in service; every image is
// checked again at once, and a refused registry resolved again.
func (m *Manager) Reload(h *config.Host, profiles []*config.Profile) {
	m.mu.Lock()
	m.set(h, profiles)
	m.mu.Unlock()
	m.kick()
}

func (m *Manager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// ForStart answers the checked image ID a runner of profile name is created
// from, and false while the profile is not ready. It asks for the tag's
// digest beside the start, at most once a minute, never in front of it.
func (m *Manager) ForStart(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(name)
	if p == nil || !p.ready() {
		return "", false
	}
	if r := p.ref; !r.pinned && time.Since(r.checked) >= startCheck {
		r.due = true
		m.kick()
	}
	return p.id, true
}

// Missing takes id out of service for profile name after a create the
// daemon refused because it no longer holds id: every profile on id reports
// not ready, and the image is pulled and checked again at once.
func (m *Manager) Missing(name, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.find(name)
	if p == nil || p.id != id {
		return
	}
	r := p.ref
	now := time.Now()
	for _, q := range r.profiles {
		if q.id == id {
			q.id, q.digest, q.missing = "", "", true
			q.fail(now, "the daemon no longer holds image "+id+", which a create needed")
		}
	}
	r.due, r.wait, r.retry = true, 0, time.Time{}
	m.kick()
	m.log.Warn("A profile's image is gone from the host's daemon, so it is pulled again", "profile", name, "image", r.name, "id", id)
}

func (p *profile) fail(now time.Time, why string) {
	p.failure, p.failedAt = why, now
	p.failures++
}

// find answers the profile named name; m.mu is held.
func (m *Manager) find(name string) *profile {
	i := slices.IndexFunc(m.profiles, func(p *profile) bool { return p.cfg.Name == name })
	if i < 0 {
		return nil
	}
	return m.profiles[i]
}

// Status answers each profile's readiness and image, in config order.
func (m *Manager) Status() []hostapi.ProfileStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]hostapi.ProfileStatus, len(m.profiles))
	for i, p := range m.profiles {
		s := hostapi.ProfileStatus{
			Name: p.cfg.Name, Image: p.ref.name, Digest: p.digest, ImageID: p.id, Ready: p.ready(),
			CheckedAt: p.checkedAt, Failure: p.failure, FailedAt: p.failedAt, Failures: p.failures,
		}
		switch {
		case p.ref.refused != nil:
			s.Reason, s.Detail = hostapi.ReasonRefused, p.ref.refused.detail
		case s.Ready:
		case p.missing:
			s.Reason, s.Detail = hostapi.ReasonImageMissing, "pulling "+p.ref.name+" again: "+p.failure
		default:
			s.Reason, s.Detail = hostapi.ReasonPulling, "no image of "+p.ref.name+" has passed its pull and check yet"
		}
		out[i] = s
	}
	return out
}

// Tidy asks for a tidy pass now, as after a start the daemon refused for
// want of space.
func (m *Manager) Tidy() {
	m.mu.Lock()
	m.tidyAsked = true
	m.mu.Unlock()
	m.kick()
}

// Run brings every profile's image into service and keeps it current until
// ctx ends: a check of each image due, a backoff of 15 s doubling to
// 5 minutes for one with no image in service, and an idle check and a tidy
// pass every images.refresh. It returns once ctx ends and its checks are
// done.
func (m *Manager) Run(ctx context.Context) {
	m.rec.load(ctx, m.log)
	var wg sync.WaitGroup
	defer wg.Wait()
	t := time.NewTimer(0)
	defer t.Stop()
	lastTidy := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-t.C:
		}
		due, tidy, next := m.due(lastTidy)
		for _, r := range due {
			wg.Go(func() { m.check(ctx, r) })
		}
		if tidy {
			lastTidy = time.Now()
			wg.Go(func() { m.tidyThenTell(ctx) })
		}
		t.Reset(time.Until(next))
	}
}

func (m *Manager) tidyThenTell(ctx context.Context) {
	m.tidy(ctx)
	if m.tidied != nil {
		m.tidied()
	}
}

// due marks each ref due now busy and answers them, the tidy pass due now,
// and when the next work falls due.
func (m *Manager) due(lastTidy time.Time) (due []*ref, tidy bool, next time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	refresh := m.host.ImageRefresh
	next = lastTidy.Add(refresh)
	if m.tidyAsked || !now.Before(next) {
		tidy, m.tidyAsked, next = true, false, now.Add(refresh)
	}
	for _, r := range m.refs {
		if r.busy {
			continue
		}
		at := r.dueAt(refresh)
		if !now.Before(at) {
			r.busy, r.due = true, false
			due = append(due, r)
			continue
		}
		next = minTime(next, at)
	}
	return due, tidy, next
}

// dueAt is when r's next check falls due.
func (r *ref) dueAt(refresh time.Duration) time.Time {
	switch {
	case r.due:
		return time.Time{}
	case r.refused != nil:
		return r.resolved.Add(refresh)
	case slices.ContainsFunc(r.profiles, func(p *profile) bool { return p.id == "" }):
		return r.retry
	}
	return r.checked.Add(refresh)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// ProbeImage answers the probe image's ID, "" before LoadProbe.
func (m *Manager) ProbeImage() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.probeID
}
