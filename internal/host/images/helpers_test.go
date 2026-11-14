package images

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

const testVersion = "v1.0.0-dev.12"

// The references the shipped profiles resolve to at testVersion.
const (
	baseRef  = jobspec.RunnerRepo + ":base-v1-dev"
	buildRef = jobspec.RunnerRepo + ":build-v1-dev"
)

func digest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// remote is an image a registry serves under a reference.
type remote struct {
	files jobspec.Files
	img   docker.Image
}

// shipped is a shipped image of name, its user runner (1001:1001).
func shipped(id, name, repoDigest string) remote {
	img := docker.Image{ID: id, RepoDigests: []string{jobspec.RunnerRepo + "@" + repoDigest}}
	img.Config.User = "runner"
	img.Config.Labels = map[string]string{docker.LabelRunnerImage: name}
	return remote{img: img, files: jobspec.Files{
		Passwd: []byte("root:x:0:0::/root:/bin/sh\nrunner:x:1001:1001::/home/runner:/bin/sh\n"),
		Group:  []byte("root:x:0:\nrunner:x:1001:\n"),
	}}
}

// fakeDaemon is a host daemon's image cache, a registry behind it, and the
// containers and volumes the tidy pass reads, held in memory. Calls block
// only on its hold channels, which a synctest bubble counts as durable.
type fakeDaemon struct {
	images     map[string]*storedImage
	tags       map[string]string
	registry   map[string]remote
	files      map[string]jobspec.Files
	pullErr    map[string]error
	digestErr  map[string]error
	containers map[string]docker.Summary
	// ctrImage is each container's image ID.
	ctrImage   map[string]string
	holdDigest chan struct{}
	pulls      []pullAt
	digests    []string
	removed    []string
	volumes    []docker.Volume
	mu         sync.Mutex
}

type pullAt struct {
	at  time.Time
	ref string
}

func newFake() *fakeDaemon {
	return &fakeDaemon{
		images: map[string]*storedImage{}, tags: map[string]string{}, registry: map[string]remote{},
		files: map[string]jobspec.Files{}, pullErr: map[string]error{}, digestErr: map[string]error{},
		containers: map[string]docker.Summary{}, ctrImage: map[string]string{},
	}
}

var _ Daemon = (*fakeDaemon)(nil)

// serve makes the registry answer ref with r.
func (d *fakeDaemon) serve(ref string, r remote) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.registry[ref] = r
}

// storedImage is an image the fake daemon holds, with the tags naming it.
type storedImage struct {
	RepoTags []string
	docker.Image
}

func (d *fakeDaemon) InspectImage(_ context.Context, ref string) (docker.Image, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := ref
	if t, ok := d.tags[ref]; ok {
		id = t
	}
	img, ok := d.images[id]
	if !ok {
		return docker.Image{}, false, nil
	}
	out := img.Image
	out.RepoDigests = slices.Clone(img.RepoDigests)
	return out, true, nil
}

func (d *fakeDaemon) PullImage(_ context.Context, ref, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pulls = append(d.pulls, pullAt{ref: ref, at: time.Now()})
	if err := d.pullErr[ref]; err != nil {
		return err
	}
	r, ok := d.registry[ref]
	if !ok {
		return errors.New("manifest unknown")
	}
	img := storedImage{Image: r.img}
	img.RepoDigests = slices.Clone(r.img.RepoDigests)
	if cur, ok := d.images[img.ID]; ok {
		img.RepoTags = cur.RepoTags
	}
	d.untag(ref)
	if !pinned(ref) {
		img.RepoTags = append(img.RepoTags, ref)
	}
	d.images[img.ID] = &img
	d.tags[ref] = img.ID
	d.files[img.ID] = r.files
	return nil
}

// untag drops ref from the image it names; d.mu is held.
func (d *fakeDaemon) untag(ref string) {
	if old, ok := d.images[d.tags[ref]]; ok {
		old.RepoTags = slices.DeleteFunc(old.RepoTags, func(t string) bool { return t == ref })
	}
	delete(d.tags, ref)
}

func (d *fakeDaemon) Digest(ctx context.Context, ref, _ string) (string, error) {
	d.mu.Lock()
	hold := d.holdDigest
	d.digests = append(d.digests, ref)
	err, r := d.digestErr[ref], d.registry[ref]
	d.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	_, dg, _ := strings.Cut(r.img.RepoDigests[0], "@")
	return dg, nil
}

// LoadImage loads a `docker save` tar as the daemon does: the image ID is
// the config's digest, and each layer must match its diff_id.
func (d *fakeDaemon) LoadImage(_ context.Context, r io.Reader) error {
	files := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		files[hdr.Name] = b
	}
	var manifest []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil || len(manifest) != 1 {
		return fmt.Errorf("manifest: %v", err)
	}
	cfgBytes := files[manifest[0].Config]
	var cfg struct {
		Config struct {
			Labels map[string]string
		} `json:"config"`
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return err
	}
	if len(cfg.RootFS.DiffIDs) != len(manifest[0].Layers) {
		return errors.New("layer count mismatch")
	}
	for i, l := range manifest[0].Layers {
		sum := sha256.Sum256(files[l])
		if "sha256:"+hex.EncodeToString(sum[:]) != cfg.RootFS.DiffIDs[i] {
			return errors.New("layer does not match its diff_id")
		}
	}
	sum := sha256.Sum256(cfgBytes)
	img := &storedImage{ID: "sha256:" + hex.EncodeToString(sum[:]), RepoTags: manifest[0].RepoTags}
	img.Config.Labels = cfg.Config.Labels
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range img.RepoTags {
		d.untag(t)
		d.tags[t] = img.ID
	}
	d.images[img.ID] = img
	return nil
}

func (d *fakeDaemon) ListImages(context.Context) ([]docker.ImageSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []docker.ImageSummary
	for _, id := range slices.Sorted(maps.Keys(d.images)) {
		img := d.images[id]
		out = append(out, docker.ImageSummary{ID: id, Labels: img.Config.Labels, RepoTags: slices.Clone(img.RepoTags), RepoDigests: slices.Clone(img.RepoDigests)})
	}
	return out, nil
}

// RemoveImage acts as the daemon's: a tag is untagged and the image goes
// with its last tag; an image a container uses is a conflict.
func (d *fakeDaemon) RemoveImage(_ context.Context, ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, isTag := d.tags[ref]
	if !isTag {
		id = ref
	}
	img, ok := d.images[id]
	if !ok {
		return nil
	}
	for c, img := range d.ctrImage {
		if img == id {
			return fmt.Errorf("%w: image %s is used by container %s", docker.ErrConflict, id, c)
		}
	}
	if isTag {
		d.untag(ref)
	} else if len(img.RepoTags) > 0 {
		return fmt.Errorf("%w: image %s is tagged", docker.ErrConflict, id)
	}
	d.removed = append(d.removed, ref)
	if len(img.RepoTags) == 0 {
		delete(d.images, id)
	}
	return nil
}

func (d *fakeDaemon) Create(_ context.Context, name string, req *docker.CreateRequest) docker.Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.images[req.Image]; !ok {
		return docker.Result{Outcome: docker.Failed, Err: fmt.Errorf("%w: %s", docker.ErrImageMissing, req.Image)}
	}
	labels := map[string]string{docker.LabelManaged: "true"}
	maps.Copy(labels, req.Labels)
	d.containers[name] = docker.Summary{ID: name, State: docker.StateCreated, Labels: labels}
	d.ctrImage[name] = req.Image
	return docker.Result{Outcome: docker.Done, ID: name}
}

func (d *fakeDaemon) ReadFile(_ context.Context, id, p string, _ int64) ([]byte, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.files[d.ctrImage[id]]
	switch p {
	case jobspec.PasswdPath:
		return f.Passwd, f.Passwd != nil, nil
	case jobspec.GroupPath:
		return f.Group, f.Group != nil, nil
	}
	return nil, false, nil
}

func (d *fakeDaemon) RemoveOwned(_ context.Context, ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.containers, ref)
	delete(d.ctrImage, ref)
	d.removed = append(d.removed, "container:"+ref)
	return nil
}

func (d *fakeDaemon) ListContainers(context.Context) ([]docker.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Collect(maps.Values(d.containers)), nil
}

func (d *fakeDaemon) ListVolumes(context.Context) ([]docker.Volume, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.volumes), nil
}

func (d *fakeDaemon) RemoveOwnedVolume(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.volumes = slices.DeleteFunc(d.volumes, func(v docker.Volume) bool { return v.Name == name })
	d.removed = append(d.removed, "volume:"+name)
	return nil
}

// addContainer adds c, created from image.
func (d *fakeDaemon) addContainer(c docker.Summary, image string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers[c.ID] = c
	d.ctrImage[c.ID] = image
}

func (d *fakeDaemon) pullsOf(ref string) []pullAt {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(d.pulls), func(p pullAt) bool { return p.ref != ref })
}

func (d *fakeDaemon) digestCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.digests)
}

func (d *fakeDaemon) removedRefs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.removed)
}

func (d *fakeDaemon) has(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.images[id]
	return ok
}

// forget drops an image from the cache, as a prune inside the host would.
func (d *fakeDaemon) forget(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.images, id)
	for t, i := range d.tags {
		if i == id {
			delete(d.tags, t)
		}
	}
}

func profileOf(name, runnerImage string) *config.Profile {
	return &config.Profile{Name: name, RunnerImage: runnerImage}
}

func internetHost() *config.Host {
	return &config.Host{Name: "default", ImageRefresh: time.Hour, Egress: config.Egress{Internet: true}}
}

// keep is a Keep with its records held by the test.
type keep struct {
	runners map[string]bool
	runs    map[string]time.Time
	mu      sync.Mutex
	running map[string]bool
}

func newKeep() *keep {
	return &keep{runners: map[string]bool{}, runs: map[string]time.Time{}, running: map[string]bool{}}
}

func (k *keep) Keep() Keep {
	return Keep{
		Runner: func(name string) bool { k.mu.Lock(); defer k.mu.Unlock(); return k.runners[name] },
		ProbeRun: func(run string) (time.Time, bool) {
			k.mu.Lock()
			defer k.mu.Unlock()
			return k.runs[run], k.running[run]
		},
	}
}

// harness is a Manager over a fakeDaemon, run inside a synctest bubble.
type harness struct {
	m    *Manager
	d    *fakeDaemon
	k    *keep
	cfg  *Config
	stop func()
}

func newHarness(t *testing.T, h *config.Host, profiles ...*config.Profile) *harness {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "manade")
	if err := os.WriteFile(bin, []byte("#!probe binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hs := &harness{d: newFake(), k: newKeep()}
	hs.cfg = &Config{
		Daemon: hs.d, Host: h, Profiles: profiles, Version: testVersion, Binary: bin,
		StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler), Keep: hs.k.Keep(),
		Lookup: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no lookup") },
	}
	return hs
}

// start runs the Manager until stop, which waits for Run to return.
func (hs *harness) start() {
	hs.m = New(hs.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hs.m.Run(ctx)
	}()
	hs.stop = func() { cancel(); <-done }
	synctest.Wait()
}

// status answers profile name's status.
func (hs *harness) status(t *testing.T, name string) hostapi.ProfileStatus {
	t.Helper()
	for _, p := range hs.m.Status() {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no status for profile %s", name)
	return hostapi.ProfileStatus{}
}
