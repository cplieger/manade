package images

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/jobspec"
)

func TestTidy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		browserRef := jobspec.RunnerRepo + ":browser-v1-dev"
		hs := newHarness(t, internetHost(), profileOf("small", "base"), profileOf("browser", "browser"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.d.serve(browserRef, shipped("sha256:w1", "browser", digest(9)))
		hs.start()
		defer hs.stop()
		if _, err := hs.m.LoadProbe(t.Context()); err != nil {
			t.Fatal(err)
		}
		probe := hs.m.ProbeImage()

		// The base tag moves twice; a running container keeps the first.
		time.Sleep(startCheck)
		hs.d.serve(baseRef, shipped("sha256:a2", "base", digest(2)))
		hs.m.ForStart("small")
		synctest.Wait()
		time.Sleep(startCheck)
		hs.d.serve(baseRef, shipped("sha256:a3", "base", digest(3)))
		hs.m.ForStart("small")
		synctest.Wait()
		hs.d.addContainer(docker.Summary{ID: "running", Labels: map[string]string{jobspec.LabelRunner: "runner-a"}}, "sha256:a1")
		// The browser profile leaves the config.
		hs.m.Reload(internetHost(), []*config.Profile{profileOf("small", "base")})
		synctest.Wait()

		hs.d.mu.Lock()
		hs.d.images["sha256:old-probe"] = &storedImage{ID: "sha256:old-probe", RepoTags: []string{probeRepo + ":v0.9.0"}}
		hs.d.images["sha256:old-probe"].Config.Labels = map[string]string{docker.LabelProbeImage: "v0.9.0"}
		hs.d.tags[probeRepo+":v0.9.0"] = "sha256:old-probe"
		hs.d.images["sha256:cache"] = &storedImage{ID: "sha256:cache", RepoTags: []string{"build-cache:1"}}
		hs.d.tags["build-cache:1"] = "sha256:cache"
		hs.d.images["sha256:dangling"] = &storedImage{ID: "sha256:dangling", RepoDigests: []string{"alpine@" + digest(8)}}
		hs.d.volumes = []docker.Volume{
			{Name: "va", Labels: map[string]string{jobspec.LabelRunner: "runner-a"}},
			{Name: "vb", Labels: map[string]string{jobspec.LabelRunner: "runner-b"}},
			{Name: "vc", Labels: map[string]string{jobspec.LabelRunner: "runner-c"}},
			{Name: "other"},
		}
		hs.d.mu.Unlock()
		hs.k.runners["runner-c"] = true
		hs.k.leftover["old"] = true
		for _, c := range []docker.Summary{
			{ID: "probe-old", Labels: map[string]string{jobspec.LabelProbe: "old"}},
			{ID: "probe-recent", Labels: map[string]string{jobspec.LabelProbe: "recent"}},
			{ID: "check-left", Labels: map[string]string{jobspec.LabelImageCheck: "sha256:a3"}},
		} {
			hs.d.addContainer(c, probe)
		}

		hs.m.Tidy()
		synctest.Wait()
		removed := hs.d.removedRefs()
		for _, want := range []string{"sha256:a2", browserRef, probeRepo + ":v0.9.0", "container:probe-old", "container:check-left", "volume:vb"} {
			if !slices.Contains(removed, want) {
				t.Errorf("removed %v, want %s among them", removed, want)
			}
		}
		for _, kept := range []string{"sha256:a1", "sha256:a3", probe, "sha256:cache", "sha256:dangling"} {
			if !hs.d.has(kept) {
				t.Errorf("image %s removed, want it kept", kept)
			}
		}
		for _, kept := range []string{"container:probe-recent", "container:running", "volume:va", "volume:vc", "volume:other"} {
			if slices.Contains(removed, kept) {
				t.Errorf("%s removed, want it kept", kept)
			}
		}
		if hs.m.rec.has(browserRef) || !hs.m.rec.has(baseRef) {
			t.Errorf("record = %v, want the untagged reference dropped and the named one kept", hs.m.rec.refs)
		}

		// Once nothing uses the first image, the next pass removes it.
		_ = hs.d.RemoveOwned(t.Context(), "running")
		time.Sleep(time.Hour)
		synctest.Wait()
		if hs.d.has("sha256:a1") {
			t.Error("a superseded image no container uses was kept past the next pass")
		}
	})
}

// A host with only a build profile proves itself after a tidy pass with
// the registry unreachable: the probe image is its own and stays.
func TestProbeImageSurvivesTidyWithTheRegistryDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("build", "build"))
		hs.d.pullErr[buildRef] = errors.New("registry unreachable")
		hs.start()
		defer hs.stop()
		id, err := hs.m.LoadProbe(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		hs.m.Tidy()
		synctest.Wait()
		if !hs.d.has(id) || hs.m.ProbeImage() != id {
			t.Errorf("probe image %s gone after the tidy pass", id)
		}
		res := hs.d.Create(t.Context(), "probe", &docker.CreateRequest{Image: hs.m.ProbeImage()})
		if res.Outcome != docker.Done {
			t.Errorf("probe create = %+v, want it created from the probe image", res)
		}
	})
}

// A named tag whose new image failed its check stays: removing it would
// pull the same image again at every check.
func TestTidyKeepsANamedTag(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		root := shipped("sha256:r1", "base", digest(3))
		root.img.Config.User = "root"
		hs.d.serve(baseRef, root)
		time.Sleep(time.Hour)
		synctest.Wait()
		if s := hs.status(t, "small"); s.ImageID != "sha256:a1" {
			t.Fatalf("status = %+v, want the old image kept in service", s)
		}
		hs.m.Tidy()
		synctest.Wait()
		if !hs.d.has("sha256:r1") || !hs.d.has("sha256:a1") {
			t.Errorf("removed %v, want the named tag's image and the image in service kept", hs.d.removedRefs())
		}
	})
}

func TestRecordSurvivesARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		browserRef := jobspec.RunnerRepo + ":browser-v1-dev"
		hs := newHarness(t, internetHost(), profileOf("browser", "browser"))
		hs.d.serve(browserRef, shipped("sha256:w1", "browser", digest(9)))
		hs.start()
		hs.stop()
		hs.cfg.Profiles = []*config.Profile{profileOf("small", "base")}
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		hs.m.Tidy()
		synctest.Wait()
		if hs.d.has("sha256:w1") {
			t.Error("an image the host pulled before its restart, no longer named, was kept")
		}
	})
}

// Tidied is told after each pass, the one Tidy asked for once its removals
// are done and the periodic one alike.
func TestTidiedFollowsEachPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"))
		var mu sync.Mutex
		var removedFirst []bool
		calls := func() []bool {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(removedFirst)
		}
		hs.cfg.Tidied = func() {
			removed := slices.Contains(hs.d.removedRefs(), "container:probe-old")
			mu.Lock()
			defer mu.Unlock()
			removedFirst = append(removedFirst, removed)
		}
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		if got := calls(); len(got) != 0 {
			t.Fatalf("Tidied calls %v before any pass", got)
		}
		hs.k.leftover["old"] = true
		hs.d.addContainer(docker.Summary{ID: "probe-old", Labels: map[string]string{jobspec.LabelProbe: "old"}}, "sha256:a1")
		hs.m.Tidy()
		synctest.Wait()
		if got := calls(); !slices.Equal(got, []bool{true}) {
			t.Errorf("Tidied calls %v, want one, after the asked pass removed the leftover", got)
		}
		time.Sleep(internetHost().ImageRefresh + time.Second)
		synctest.Wait()
		if got := calls(); len(got) != 2 {
			t.Errorf("Tidied calls %v, want a second after the periodic pass", got)
		}
	})
}

func TestTidyKeepsAnImageAPinnedProfileNames(t *testing.T) {
	for name, refs := range map[string]struct{ profile, repoDigest string }{
		"as the daemon lists it":    {"registry.example/team/runner@" + digest(7), "registry.example/team/runner@" + digest(7)},
		"in Docker Hub's full form": {"docker.io/library/alpine@" + digest(7), "alpine@" + digest(7)},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hs := newHarness(t, internetHost(), &config.Profile{Name: "pinned", Image: refs.profile})
				r := shipped("sha256:p1", "", digest(7))
				r.img.RepoDigests = []string{refs.repoDigest}
				r.img.Config.User = "root"
				hs.d.serve(refs.profile, r)
				hs.start()
				defer hs.stop()
				if s := hs.status(t, "pinned"); s.Ready {
					t.Fatalf("setup: status = %+v, want the root image refused", s)
				}
				hs.m.Tidy()
				synctest.Wait()
				if !hs.d.has("sha256:p1") {
					t.Errorf("removed %v, want the image the pinned reference names kept", hs.d.removedRefs())
				}
			})
		})
	}
}
