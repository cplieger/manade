package pool

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/jobspec"
)

const (
	baseRef = jobspec.RunnerRepo + ":base"
	ownRef  = "example.com/own:1"
)

// pinnedRef is a reference pinned by digest.
var pinnedRef = "example.com/tools@sha256:" + strings.Repeat("a", 64)

var (
	appPinned = scaleSet("acme/app", "pinned", 5)
	appOwn    = scaleSet("acme/app", "own", 6)
)

// The images' own files: GitHub's runner user, and an image whose only
// user is root.
var (
	runnerFiles = jobspec.Files{
		Passwd: []byte("root:x:0:0:root:/root:/bin/bash\nrunner:x:1001:1001::/home/runner:/bin/bash\n"),
		Group:  []byte("root:x:0:\nsudo:x:27:runner\nrunner:x:1001:\n"),
	}
	rootFiles = jobspec.Files{Passwd: []byte("root:x:0:0:root:/root:/bin/sh\n")}
)

// shippedImage is a shipped runner image with id, run as runner.
func shippedImage(id string) docker.Image {
	var img docker.Image
	img.ID, img.Config.User = id, "runner"
	img.Config.Labels = map[string]string{docker.LabelRunnerImage: "base"}
	return img
}

// imageFixture is a pool on one repo scope whose profiles use a shipped
// tag (default), a digest (pinned) and an operator's tag (own), with no
// image in service yet and nothing in the daemon's cache.
func imageFixture(t *testing.T) *fixture {
	t.Helper()
	return imageFixtureOf(t, fmt.Sprintf(`  default: {cpus: 2, memory: 4GiB}
  pinned: {cpus: 2, memory: 4GiB, image: %q}
  own: {cpus: 2, memory: 4GiB, image: %q}
`, pinnedRef, ownRef), appDefault, appPinned, appOwn)
}

// imageFixtureOf is imageFixture with the profiles and scale sets given.
func imageFixtureOf(t *testing.T, profiles string, sets ...github.ScaleSet) *fixture {
	t.Helper()
	cfg := load(t, "capacity: {cpus: 8, memory: 32GiB}\nimages: {refresh: 6h}\nscopes:\n  - repo: acme/app\nprofiles:\n"+profiles)
	fx := newFixtureFrom(t, cfg, sets...)
	fx.p.mu.Lock()
	clear(fx.p.images)
	fx.p.mu.Unlock()
	own := shippedImage("sha256:own1")
	own.Config.Labels = nil
	fx.dk.registry = map[string]docker.Image{baseRef: shippedImage("sha256:base1"), pinnedRef: shippedImage("sha256:pinned"), ownRef: own}
	fx.dk.files = map[string]jobspec.Files{"sha256:base1": runnerFiles, "sha256:base2": runnerFiles, "sha256:pinned": runnerFiles, "sha256:own1": runnerFiles, "sha256:own2": runnerFiles, "sha256:root": rootFiles}
	return fx
}

// boot loads every image as boot does: from the cache, pulled when the
// cache lacks it.
func (fx *fixture) boot(t *testing.T) {
	t.Helper()
	if err := fx.p.loadImages(t.Context()); err != nil {
		t.Fatalf("loadImages: %v", err)
	}
}

// stale caches ref's registry image before boot, so boot neither pulls
// nor checks it.
func (fx *fixture) stale(ref string) { fx.dk.cache(ref, fx.dk.registry[ref]) }

// refreshDue runs every image check due now, as runImages does.
func (fx *fixture) refreshDue(t *testing.T) []string {
	t.Helper()
	var refs []string
	for _, img := range fx.p.dueImages() {
		refs = append(refs, img.ref)
		_ = fx.p.refresh(t.Context(), img, true)
	}
	return refs
}

// serving is profile's image ID in service.
func (fx *fixture) serving(profile string) string {
	fx.p.mu.Lock()
	defer fx.p.mu.Unlock()
	return fx.p.images[fx.p.byName[profile]]
}

// assign gives ss n more assignments and runs the starts they owe.
func (fx *fixture) assign(t *testing.T, ss github.ScaleSet, n int) []*runner {
	t.Helper()
	fx.p.mu.Lock()
	s := fx.p.set(ss)
	st := github.Stats{Assigned: s.asg.stats.Assigned + n}
	fx.p.mu.Unlock()
	fx.p.Opened(ss, st)
	rs := fx.p.owed()
	for _, r := range rs {
		fx.p.start(t.Context(), r)
	}
	return rs
}

// checkedAt reads profile's manade_image_checked_timestamp_seconds.
func checkedAt(t *testing.T, fx *fixture, profile string) float64 {
	t.Helper()
	prefix := `manade_image_checked_timestamp_seconds{profile="` + profile + `"} `
	sc := bufio.NewScanner(strings.NewReader(scrape(t, fx.m)))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), prefix); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", sc.Text(), err)
			}
			return f
		}
	}
	t.Fatalf("no %s series", prefix)
	return 0
}

func dockerErrors(t *testing.T, fx *fixture, op string) string {
	t.Helper()
	for line := range strings.Lines(scrape(t, fx.m)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), `manade_docker_errors_total{op="`+op+`"} `); ok {
			return v
		}
	}
	return ""
}

func TestImageBootPullsOnlyWhatTheCacheLacks(t *testing.T) {
	fx := imageFixture(t)
	fx.dk.cache(baseRef, shippedImage("sha256:base1"))
	fx.boot(t)
	if got := slices.Sorted(slices.Values(fx.dk.pulls)); !slices.Equal(got, []string{ownRef, pinnedRef}) {
		t.Errorf("pulls = %v, want the two images the cache lacked", got)
	}
	for profile, want := range map[string]string{"default": "sha256:base1", "pinned": "sha256:pinned", "own": "sha256:own1"} {
		if got := fx.serving(profile); got != want {
			t.Errorf("%s serves %q, want %q", profile, got, want)
		}
	}
	if got := checkedAt(t, fx, "own"); got != float64(epoch.Unix()) {
		t.Errorf("the pulled tag's check time = %v, want %v", got, epoch.Unix())
	}
}

// A tag the cache already holds has no check time in this process, so a
// restart must not leave it reading as never pulled until its first job.
func TestImageBootChecksACachedTagAtOnce(t *testing.T) {
	fx := imageFixture(t)
	fx.stale(baseRef)
	fx.boot(t)
	if got := checkedAt(t, fx, "default"); got != 0 {
		t.Errorf("the cached tag's check time after boot = %v, want 0: boot pulled nothing", got)
	}
	fx.advance(time.Second)
	if got := fx.refreshDue(t); !slices.Equal(got, []string{baseRef}) {
		t.Fatalf("checks right after boot = %v, want the cached tag alone: boot's pulls checked the others", got)
	}
	if got := checkedAt(t, fx, "default"); got != float64(fx.at.Unix()) {
		t.Errorf("the cached tag's check time = %v, want the background check's %v", got, fx.at.Unix())
	}
	fx.assign(t, appOwn, 1)
	fx.assign(t, appDefault, 1)
	if got := fx.refreshDue(t); got != nil {
		t.Errorf("checks after the first starts = %v, want none inside images.refresh", got)
	}
}

func TestImageTagCheckedOncePerRefresh(t *testing.T) {
	fx := imageFixture(t)
	fx.stale(baseRef)
	fx.boot(t)
	booted := len(fx.dk.created)
	fx.assign(t, appDefault, 1)
	if got := fx.refreshDue(t); !slices.Equal(got, []string{baseRef}) {
		t.Fatalf("checks after the first start = %v, want the tag", got)
	}
	if got := len(fx.dk.created) - booted; got != 1 {
		t.Errorf("containers created by the start and its check = %d, want the runner alone: an unchanged ID is not probed again", got)
	}
	fx.advance(6*time.Hour - time.Second)
	fx.assign(t, appDefault, 1)
	if got := fx.refreshDue(t); got != nil {
		t.Errorf("checks a second before images.refresh = %v, want none", got)
	}
	fx.advance(time.Second)
	fx.assign(t, appDefault, 1)
	if got := fx.refreshDue(t); !slices.Equal(got, []string{baseRef}) {
		t.Errorf("checks at images.refresh = %v, want the tag", got)
	}
	if got := checkedAt(t, fx, "default"); got != float64(fx.at.Unix()) {
		t.Errorf("check time = %v, want the last check's %v", got, fx.at.Unix())
	}
}

func TestImageDueCheckNeverHoldsAStart(t *testing.T) {
	fx := imageFixture(t)
	fx.stale(baseRef)
	fx.boot(t)
	fx.dk.registry[baseRef] = shippedImage("sha256:base2")
	gate := make(chan struct{})
	fx.dk.mu.Lock()
	fx.dk.pullGate = gate
	fx.dk.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { fx.p.runImages(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })

	first := fx.assign(t, appDefault, 1)
	waitFor(t, "the check's pull to begin", func() bool {
		fx.dk.mu.Lock()
		defer fx.dk.mu.Unlock()
		return len(fx.dk.pulls) == 3
	})
	second := fx.assign(t, appDefault, 1)
	for _, r := range append(first, second...) {
		if r.image != "sha256:base1" || r.container == "" {
			t.Errorf("runner %s started from %q with container %q while the pull hung, want the checked ID and a container", r.name, r.image, r.container)
		}
	}
	close(gate)
	waitFor(t, "the check to end", func() bool {
		fx.p.mu.Lock()
		defer fx.p.mu.Unlock()
		return !fx.p.imgOf[0].pulling
	})
	if got := fx.serving("default"); got != "sha256:base2" {
		t.Errorf("default serves %q, want the checked sha256:base2", got)
	}
	if got := fx.refreshDue(t); got != nil {
		t.Errorf("checks due after the one in flight = %v, want none: the second start found it in flight", got)
	}
	fx.dk.mu.Lock()
	pulls := slices.Clone(fx.dk.pulls)
	fx.dk.mu.Unlock()
	if !slices.Equal(pulls[2:], []string{baseRef}) || len(pulls) != 3 {
		t.Errorf("pulls = %v, want boot's two then one check", pulls)
	}
}

// waitFor polls cond until it holds or 5 seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestImageNewIDServesTheNextStart(t *testing.T) {
	fx := imageFixture(t)
	fx.stale(baseRef)
	fx.boot(t)
	booted := len(fx.dk.created)
	old := fx.assign(t, appDefault, 1)[0]
	fx.dk.registry[baseRef] = shippedImage("sha256:base2")
	fx.refreshDue(t)

	if old.image != "sha256:base1" {
		t.Errorf("the running runner's image = %q, want it untouched", old.image)
	}
	if r := fx.assign(t, appDefault, 1)[0]; r.image != "sha256:base2" {
		t.Errorf("the next start's image = %q, want the new ID", r.image)
	}
	probes := 0
	for _, req := range fx.dk.created[booted:] {
		if req.Labels[docker.LabelProbe] == string(jobspec.ProbeUser) {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("user probes = %d, want one for the new ID", probes)
	}
	for id, b := range fx.dk.boxes {
		if b.ctr.Config.Labels[docker.LabelProbe] != "" {
			t.Errorf("probe %s still in the daemon", id)
		}
	}
	if len(fx.dk.started) != 2 {
		t.Errorf("started = %v, want only the two runners: a probe is never started", fx.dk.started)
	}
}

func TestImageRefusedUserKeepsTheCheckedID(t *testing.T) {
	fx := imageFixture(t)
	fx.stale(baseRef)
	fx.boot(t)
	root := shippedImage("sha256:root")
	root.Config.User = ""
	fx.dk.registry[baseRef] = root
	fx.assign(t, appDefault, 1)
	fx.advance(time.Minute)
	fx.refreshDue(t)
	if got := fx.serving("default"); got != "sha256:base1" {
		t.Errorf("default serves %q after a root image, want the checked sha256:base1", got)
	}
	if got := dockerErrors(t, fx, "pull"); got != "1" {
		t.Errorf("pull errors = %q, want 1", got)
	}
	if got := checkedAt(t, fx, "default"); got != 0 {
		t.Errorf("check time = %v, want none: boot found the tag cached, and a refused image is no good check", got)
	}
	if r := fx.assign(t, appDefault, 1)[0]; r.image != "sha256:base1" {
		t.Errorf("the next start's image = %q, want the checked ID", r.image)
	}
	fx.p.mu.Lock()
	old := slices.Clone(fx.p.old)
	fx.p.mu.Unlock()
	if len(old) != 0 {
		t.Errorf("superseded = %v, want none", old)
	}
}

func TestImageFailedCheckKeepsTheCache(t *testing.T) {
	fx := imageFixture(t)
	fx.stale(baseRef)
	fx.boot(t)
	fx.dk.pullErr = errFake
	fx.assign(t, appDefault, 1)
	fx.refreshDue(t)
	if got := dockerErrors(t, fx, "pull"); got != "1" {
		t.Errorf("pull errors = %q, want 1", got)
	}
	if r := fx.assign(t, appDefault, 1)[0]; r.image != "sha256:base1" {
		t.Errorf("start after a failed check = %q, want the cached sha256:base1", r.image)
	}
	if got := fx.refreshDue(t); got != nil {
		t.Errorf("checks right after a failed one = %v, want none until images.refresh", got)
	}
}

func TestImageMissingRetriesWithBackoff(t *testing.T) {
	fx := imageFixture(t)
	fx.dk.cache(pinnedRef, fx.dk.registry[pinnedRef])
	fx.dk.cache(ownRef, fx.dk.registry[ownRef])
	fx.dk.pullErr = errFake
	for _, img := range fx.p.imgs {
		_ = fx.p.refresh(t.Context(), img, false)
	}
	fx.sweep(map[string][]github.QueuedJob{"acme/app": {job("default", 1, "build", 0)}})
	if c := counts(fx.p.Snapshot(), appDefault); c.ImageMissing != 1 || c.Queued != 0 {
		t.Errorf("counts = %+v, want the job unfundable as image_missing", c)
	}
	if got := fx.capacity(appDefault); got != 0 {
		t.Errorf("capacity = %d, want 0 for a profile without an image", got)
	}
	for _, wait := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		fx.advance(wait - time.Second)
		if got := fx.refreshDue(t); slices.Contains(got, baseRef) {
			t.Fatalf("retried %v into a %v backoff", wait-time.Second, wait)
		}
		fx.advance(time.Second)
		if got := fx.refreshDue(t); !slices.Contains(got, baseRef) {
			t.Fatalf("no retry after the %v backoff", wait)
		}
	}
	fx.advance(4 * time.Minute)
	fx.dk.pullErr = nil
	fx.refreshDue(t)
	if got := fx.serving("default"); got != "sha256:base1" {
		t.Errorf("default serves %q after a good pull, want sha256:base1", got)
	}
	// The missing tag's boot pull and three retries, and the cached own
	// tag's check right after boot.
	if got := dockerErrors(t, fx, "pull"); got != "5" {
		t.Errorf("pull errors = %q, want one per failed pull", got)
	}
}

func TestImageMissingPullInFlightNotRepeated(t *testing.T) {
	fx := imageFixture(t)
	fx.dk.pullErr = errFake
	_ = fx.p.refresh(t.Context(), fx.p.imgOf[0], false)
	isBase := func(img *image) bool { return img.ref == baseRef }
	fx.advance(pullFirst)
	if got := fx.p.dueImages(); !slices.ContainsFunc(got, isBase) {
		t.Fatal("the missing image is not due after its backoff")
	}
	fx.advance(time.Hour)
	if got := fx.p.dueImages(); slices.ContainsFunc(got, isBase) {
		t.Error("the missing image is due again while its pull is in flight")
	}
}

func TestImageBackoffRestartsAfterAGoodPull(t *testing.T) {
	fx := imageFixture(t)
	fx.dk.cache(baseRef, fx.dk.registry[baseRef])
	fx.dk.cache(ownRef, fx.dk.registry[ownRef])
	for _, profile := range []string{"default", "own"} {
		_ = fx.p.refresh(t.Context(), fx.p.imgOf[fx.p.byName[profile]], false)
	}
	fx.dk.pullErr = errFake
	pinned := fx.p.imgOf[fx.p.byName["pinned"]]
	for range 3 {
		_ = fx.p.refresh(t.Context(), pinned, true)
	}
	fx.dk.pullErr = nil
	_ = fx.p.refresh(t.Context(), pinned, true)
	fx.dk.mu.Lock()
	delete(fx.dk.imgs, "sha256:pinned")
	fx.dk.pullErr = errFake
	fx.dk.mu.Unlock()
	fx.p.tidyImages(t.Context())
	fx.refreshDue(t)
	fx.advance(pullFirst - time.Second)
	if got := fx.refreshDue(t); got != nil {
		t.Errorf("retried %v after a fresh failure = %v, want the backoff back at %v", pullFirst-time.Second, got, pullFirst)
	}
	fx.advance(time.Second)
	if got := fx.refreshDue(t); !slices.Equal(got, []string{pinnedRef}) {
		t.Errorf("checks at %v = %v, want the retry", pullFirst, got)
	}
}

func TestImageBackoffCapsAtTenMinutes(t *testing.T) {
	fx := imageFixture(t)
	fx.dk.pullErr = errFake
	img := fx.p.imgOf[0]
	for range 8 {
		_ = fx.p.refresh(t.Context(), img, true)
	}
	if img.wait != 10*time.Minute {
		t.Errorf("backoff = %v, want 10m", img.wait)
	}
}

func TestImageDigestNeverChecked(t *testing.T) {
	fx := imageFixture(t)
	fx.boot(t)
	fx.advance(7 * time.Hour)
	fx.assign(t, appPinned, 1)
	if got := fx.refreshDue(t); got != nil {
		t.Errorf("checks after a digest's start = %v, want none", got)
	}
	if got := checkedAt(t, fx, "pinned"); got != float64(epoch.Unix()) {
		t.Errorf("check time = %v, want boot's pull", got)
	}
	fx.advance(time.Minute)
	fx.p.tidyImages(t.Context())
	if got := checkedAt(t, fx, "pinned"); got != float64(fx.at.Unix()) {
		t.Errorf("check time = %v, want the pass that found it cached", got)
	}
	fx.dk.mu.Lock()
	delete(fx.dk.imgs, "sha256:pinned")
	fx.dk.mu.Unlock()
	fx.p.tidyImages(t.Context())
	if got := fx.serving("pinned"); got != "" {
		t.Errorf("pinned serves %q after its image left the cache, want none", got)
	}
	if got := fx.refreshDue(t); !slices.Equal(got, []string{pinnedRef}) {
		t.Errorf("checks after the image left = %v, want its pull", got)
	}
	if got := fx.serving("pinned"); got != "sha256:pinned" {
		t.Errorf("pinned serves %q after the pull, want sha256:pinned", got)
	}
}

func TestImageSupersededRemoval(t *testing.T) {
	t.Run("removedOnceNoRunnerUsesIt", func(t *testing.T) {
		fx := imageFixture(t)
		fx.stale(baseRef)
		fx.boot(t)
		r := fx.assign(t, appDefault, 1)[0]
		fx.dk.registry[baseRef] = shippedImage("sha256:base2")
		fx.refreshDue(t)
		fx.reconcile(t)
		if len(fx.dk.removedImages) != 0 {
			t.Fatalf("removed %v while a runner uses it", fx.dk.removedImages)
		}
		fx.dk.exit(r.name, 0, false)
		fx.reconcile(t)
		if !slices.Equal(fx.dk.removedImages, []string{"sha256:base1"}) {
			t.Errorf("removed images = %v, want the superseded sha256:base1", fx.dk.removedImages)
		}
	})
	t.Run("keptWhileAContainerOutsideThePoolUsesIt", func(t *testing.T) {
		fx := imageFixture(t)
		fx.stale(baseRef)
		fx.boot(t)
		r := fx.assign(t, appDefault, 1)[0]
		fx.dk.registry[baseRef] = shippedImage("sha256:base2")
		fx.refreshDue(t)
		other := &fakeBox{state: "running"}
		other.ctr.ID, other.ctr.Image = "c-other", "sha256:base1"
		fx.dk.mu.Lock()
		fx.dk.add(other)
		fx.dk.mu.Unlock()
		fx.dk.exit(r.name, 0, false)
		fx.reconcile(t)
		if len(fx.dk.removedImages) != 0 {
			t.Fatalf("removed %v while another container uses it", fx.dk.removedImages)
		}
		if got := dockerErrors(t, fx, "remove"); got != "0" {
			t.Errorf("remove errors = %q, want 0: the daemon's refusal is expected", got)
		}
		fx.dk.mu.Lock()
		delete(fx.dk.boxes, "c-other")
		fx.dk.mu.Unlock()
		fx.reconcile(t)
		if !slices.Equal(fx.dk.removedImages, []string{"sha256:base1"}) {
			t.Errorf("removed images = %v, want sha256:base1 once free", fx.dk.removedImages)
		}
	})
	t.Run("keptForAStartInFlight", func(t *testing.T) {
		fx := imageFixture(t)
		fx.stale(baseRef)
		fx.boot(t)
		fx.p.Opened(appDefault, github.Stats{Assigned: 1})
		rs := fx.p.owed()
		fx.dk.registry[baseRef] = shippedImage("sha256:base2")
		fx.refreshDue(t)
		fx.p.tidyImages(t.Context())
		if len(fx.dk.removedImages) != 0 {
			t.Fatalf("removed %v before the start that took it created its container", fx.dk.removedImages)
		}
		fx.p.start(t.Context(), rs[0])
		if len(fx.dk.started) != 1 {
			t.Errorf("started = %v, want the runner started from its image", fx.dk.started)
		}
	})
	t.Run("keptWhileAnotherProfileServesIt", func(t *testing.T) {
		appNumeric := scaleSet("acme/app", "numeric", 7)
		fx := imageFixtureOf(t, "  default: {cpus: 2, memory: 4GiB}\n  numeric: {cpus: 2, memory: 4GiB, user: \"1001:1001\"}\n",
			appDefault, appNumeric)
		fx.stale(baseRef)
		fx.boot(t)
		root := shippedImage("sha256:root")
		root.Config.User = ""
		fx.dk.registry[baseRef] = root
		r := fx.assign(t, appNumeric, 1)[0]
		fx.refreshDue(t)
		if fx.serving("numeric") != "sha256:root" || fx.serving("default") != "sha256:base1" {
			t.Fatalf("numeric serves %q and default %q, want the new ID only where no user check applies",
				fx.serving("numeric"), fx.serving("default"))
		}
		fx.dk.exit(r.name, 0, false)
		fx.reconcile(t)
		if len(fx.dk.removedImages) != 0 {
			t.Errorf("removed %v while default still serves it", fx.dk.removedImages)
		}
	})
	t.Run("keptWhileATagNamesIt", func(t *testing.T) {
		fx := imageFixture(t)
		fx.stale(baseRef)
		fx.boot(t)
		fx.dk.registry[baseRef] = shippedImage("sha256:base2")
		r := fx.assign(t, appDefault, 1)[0]
		fx.refreshDue(t)
		fx.dk.mu.Lock()
		kept := fx.dk.imgs["sha256:base1"]
		kept.RepoTags = []string{"local/keep:1"}
		fx.dk.imgs["sha256:base1"] = kept
		fx.dk.mu.Unlock()
		fx.dk.exit(r.name, 0, false)
		fx.reconcile(t)
		fx.p.mu.Lock()
		old := slices.Clone(fx.p.old)
		fx.p.mu.Unlock()
		if len(fx.dk.removedImages) != 0 || len(old) != 0 {
			t.Errorf("removed %v, still tracked %v; want a tagged image kept and forgotten", fx.dk.removedImages, old)
		}
	})
	t.Run("keptWithoutTheLabel", func(t *testing.T) {
		fx := imageFixture(t)
		unlabelled := shippedImage("sha256:base1")
		unlabelled.Config.Labels = nil
		fx.dk.registry[baseRef] = unlabelled
		fx.stale(baseRef)
		fx.boot(t)
		fx.dk.registry[baseRef] = shippedImage("sha256:base2")
		r := fx.assign(t, appDefault, 1)[0]
		fx.refreshDue(t)
		fx.dk.exit(r.name, 0, false)
		fx.reconcile(t)
		fx.p.mu.Lock()
		old := slices.Clone(fx.p.old)
		fx.p.mu.Unlock()
		if len(fx.dk.removedImages) != 0 || len(old) != 0 {
			t.Errorf("removed %v, still tracked %v; want an image without the label kept and forgotten", fx.dk.removedImages, old)
		}
	})
	t.Run("neverAnOperatorsImage", func(t *testing.T) {
		fx := imageFixture(t)
		// An image built on a shipped one inherits its label.
		fx.dk.registry[ownRef] = shippedImage("sha256:own1")
		fx.stale(ownRef)
		fx.boot(t)
		fx.dk.registry[ownRef] = shippedImage("sha256:own2")
		r := fx.assign(t, appOwn, 1)[0]
		fx.refreshDue(t)
		if got := fx.serving("own"); got != "sha256:own2" {
			t.Fatalf("own serves %q, want sha256:own2", got)
		}
		fx.dk.exit(r.name, 0, false)
		fx.reconcile(t)
		if len(fx.dk.removedImages) != 0 {
			t.Errorf("removed %v, want an image: reference never removed", fx.dk.removedImages)
		}
	})
}

func TestProbes(t *testing.T) {
	probeBox := func(id string, created time.Time) *fakeBox {
		b := &fakeBox{state: "created"}
		b.ctr.ID = id
		b.ctr.Config.Labels = map[string]string{
			docker.LabelManaged: "true", docker.LabelProbe: "user", docker.LabelCreated: created.Format(time.RFC3339),
		}
		return b
	}
	t.Run("removalRetriedOnItsPath", func(t *testing.T) {
		probeRetry = 0
		t.Cleanup(func() { probeRetry = 2 * time.Second })
		fx := imageFixture(t)
		fx.dk.probeRemoveFails = 2
		fx.boot(t)
		if got := len(fx.dk.boxes); got != 0 {
			t.Errorf("containers left = %d, want the probes removed on their third try", got)
		}
		if got := dockerErrors(t, fx, "remove"); got != "2" {
			t.Errorf("remove errors = %q, want 2", got)
		}
	})
	t.Run("leftToThePassAfterItGivesUp", func(t *testing.T) {
		probeRetry = 0
		t.Cleanup(func() { probeRetry = 2 * time.Second })
		fx := imageFixture(t)
		fx.dk.probeRemoveFails = probeTries
		fx.dk.registry = map[string]docker.Image{baseRef: shippedImage("sha256:base1")}
		_ = fx.p.refresh(t.Context(), fx.p.imgOf[0], false)
		if got := len(fx.dk.boxes); got != 1 {
			t.Fatalf("containers = %d, want the one probe left", got)
		}
		fx.p.mu.Lock()
		recorded := len(fx.p.probes)
		fx.p.mu.Unlock()
		if recorded != 0 {
			t.Errorf("probes recorded = %d, want none once the call gave up", recorded)
		}
		fx.advance(probeAge)
		fx.reconcile(t)
		if got := len(fx.dk.boxes); got != 1 {
			t.Errorf("containers = %d at %v, want the probe kept", got, probeAge)
		}
		fx.advance(time.Second)
		fx.reconcile(t)
		if got := len(fx.dk.boxes); got != 0 {
			t.Errorf("containers = %d past %v, want the probe removed", got, probeAge)
		}
	})
	t.Run("leftAloneWhileItsCallRuns", func(t *testing.T) {
		fx := imageFixture(t)
		fx.dk.registry = map[string]docker.Image{baseRef: shippedImage("sha256:base1")}
		kept := false
		fx.dk.readHook = func() {
			fx.advance(time.Hour)
			fx.reconcile(t)
			fx.dk.mu.Lock()
			kept = len(fx.dk.boxes) == 1
			fx.dk.mu.Unlock()
		}
		if err := fx.p.refresh(t.Context(), fx.p.imgOf[0], false); err != nil {
			t.Fatal(err)
		}
		if !kept {
			t.Error("a pass during the probe's call removed it")
		}
	})
	t.Run("recordedLeftAloneAtAnyAge", func(t *testing.T) {
		fx := imageFixture(t)
		fx.dk.mu.Lock()
		fx.dk.add(probeBox("c-probe", epoch))
		fx.dk.mu.Unlock()
		fx.p.mu.Lock()
		fx.p.probes["c-probe"] = true
		fx.p.mu.Unlock()
		fx.advance(24 * time.Hour)
		fx.reconcile(t)
		if fx.dk.boxes["c-probe"] == nil {
			t.Fatal("the pass removed a probe whose call is running")
		}
		fx.p.mu.Lock()
		delete(fx.p.probes, "c-probe")
		fx.p.mu.Unlock()
		fx.reconcile(t)
		if fx.dk.boxes["c-probe"] != nil {
			t.Error("the pass kept an old probe no call records")
		}
		if got := fx.gh.removed; len(got) != 0 {
			t.Errorf("registrations removed = %v, want no GitHub call for a probe", got)
		}
	})
	t.Run("aYoungUnrecordedProbeStays", func(t *testing.T) {
		fx := imageFixture(t)
		fx.dk.mu.Lock()
		fx.dk.add(probeBox("c-probe", epoch.Add(-probeAge+time.Second)))
		fx.dk.mu.Unlock()
		fx.reconcile(t)
		if fx.dk.boxes["c-probe"] == nil {
			t.Error("the pass removed a probe younger than probeAge")
		}
	})
}

func TestAnImageGoneFromTheCacheIsPulledAgain(t *testing.T) {
	fx := imageFixture(t)
	fx.boot(t)
	if got := fx.serving("default"); got != "sha256:base1" {
		t.Fatalf("serving = %q, want sha256:base1", got)
	}
	// Something outside the pool removed the image; the tag now names base2.
	fx.dk.createErr = &docker.APIError{Op: "create", Code: http.StatusNotFound, Message: "No such image: sha256:base1"}
	fx.dk.registry[baseRef] = shippedImage("sha256:base2")
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.startAll(t)
	if got := fx.serving("default"); got != "" {
		t.Errorf("serving after the create's 404 = %q, want none until the pull", got)
	}
	// The pull's user check creates a probe, which the new image allows.
	fx.dk.createErr = nil
	if got := fx.refreshDue(t); !slices.Contains(got, baseRef) {
		t.Fatalf("checks due = %v, want %s pulled again at once", got, baseRef)
	}
	if got := fx.serving("default"); got != "sha256:base2" {
		t.Errorf("serving after the pull = %q, want sha256:base2", got)
	}
	fx.advance(startRetry)
	if n := fx.startAll(t); n != 1 {
		t.Errorf("starts after the pull = %d, want the retry", n)
	}
}

func TestAnotherCreate404LeavesTheImageInService(t *testing.T) {
	fx := imageFixture(t)
	fx.boot(t)
	fx.dk.createErr = &docker.APIError{Op: "create", Code: http.StatusNotFound, Message: "network manade-isolated not found"}
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.startAll(t)
	if got := fx.serving("default"); got != "sha256:base1" {
		t.Errorf("serving = %q, want sha256:base1 kept", got)
	}
}
