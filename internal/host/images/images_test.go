package images

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

func TestStartPullsAndChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"), profileOf("medium", "base"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		if got := hs.d.pullsOf(baseRef); len(got) != 1 {
			t.Errorf("pulls = %v, want one for both profiles", got)
		}
		for _, name := range []string{"small", "medium"} {
			s := hs.status(t, name)
			if !s.Ready || s.ImageID != "sha256:a1" || s.Image != baseRef || s.Digest != digest(1) || s.CheckedAt.IsZero() {
				t.Errorf("status %s = %+v, want ready on the pulled image with its digest and check time", name, s)
			}
			if id, ok := hs.m.ForStart(name); !ok || id != "sha256:a1" {
				t.Errorf("ForStart(%s) = %q, %v; want the checked ID", name, id, ok)
			}
		}
	})
}

func TestStartServesACachedImageWithTheRegistryDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		_ = hs.d.PullImage(t.Context(), baseRef, "")
		hs.d.digestErr[baseRef] = errors.New("registry unreachable")
		hs.start()
		defer hs.stop()
		s := hs.status(t, "small")
		if !s.Ready || s.ImageID != "sha256:a1" {
			t.Errorf("status = %+v, want the cached image in service", s)
		}
		if !s.CheckedAt.IsZero() || !strings.Contains(s.Failure, "registry unreachable") || s.Failures != 1 {
			t.Errorf("status = %+v, want no good check and the failed ask counted", s)
		}
	})
}

func TestFailedPullBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"))
		hs.d.pullErr[baseRef] = errors.New("denied")
		began := time.Now()
		hs.start()
		defer hs.stop()
		time.Sleep(20 * time.Minute)
		synctest.Wait()
		var gaps []time.Duration
		last := began
		for _, p := range hs.d.pullsOf(baseRef) {
			gaps = append(gaps, p.at.Sub(last))
			last = p.at
		}
		want := []time.Duration{0, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
		if len(gaps) < len(want) || !slices.Equal(gaps[:len(want)], want) {
			t.Errorf("gaps between pulls = %v, want %v then 5m", gaps, want)
		}
		s := hs.status(t, "small")
		if s.Ready || s.Reason != hostapi.ReasonPulling || !strings.Contains(s.Failure, "denied") || s.FailedAt.IsZero() {
			t.Errorf("status = %+v, want not ready, pulling, naming the failure and its time", s)
		}
		if _, ok := hs.m.ForStart("small"); ok {
			t.Error("ForStart served a profile with no image in service")
		}
		delete(hs.d.pullErr, baseRef)
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if s := hs.status(t, "small"); !s.Ready {
			t.Errorf("status = %+v, want ready once a pull succeeds", s)
		}
	})
}

// A create refused because the daemon lost the image takes it out of
// service, and the tag is pulled and checked again at once.
func TestMissingImagePulledAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"), profileOf("medium", "base"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		hs.d.serve(baseRef, shipped("sha256:a2", "base", digest(2)))
		hs.d.mu.Lock()
		hs.d.pullErr[baseRef] = errors.New("registry down")
		hs.d.mu.Unlock()
		hs.d.forget("sha256:a1")
		hs.m.Missing("small", "sha256:a1")
		for _, name := range []string{"small", "medium"} {
			s := hs.status(t, name)
			if s.Ready || s.Reason != hostapi.ReasonImageMissing || !strings.Contains(s.Failure, "sha256:a1") || s.FailedAt.IsZero() {
				t.Errorf("status %s = %+v, want not ready, image_missing, naming the image and the time", name, s)
			}
			if _, ok := hs.m.ForStart(name); ok {
				t.Errorf("ForStart(%s) served a missing image", name)
			}
		}
		synctest.Wait()
		if n := len(hs.d.pullsOf(baseRef)); n != 2 {
			t.Fatalf("pulls = %d, want the missing image pulled again at once", n)
		}
		hs.d.mu.Lock()
		delete(hs.d.pullErr, baseRef)
		hs.d.mu.Unlock()
		time.Sleep(retryFirst)
		synctest.Wait()
		if id, ok := hs.m.ForStart("small"); !ok || id != "sha256:a2" {
			t.Errorf("ForStart = %q, %v; want the image pulled again", id, ok)
		}
	})
}

func TestMissingIgnoresAnotherID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		hs.m.Missing("small", "sha256:old")
		hs.m.Missing("absent", "sha256:a1")
		if s := hs.status(t, "small"); !s.Ready {
			t.Errorf("status = %+v, want a stale or foreign report ignored", s)
		}
	})
}

func TestStartChecksTheDigestBesideTheStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		before := hs.d.digestCalls()
		hs.m.ForStart("small")
		synctest.Wait()
		if n := hs.d.digestCalls() - before; n != 0 {
			t.Errorf("digest asks within a minute of the last check = %d, want 0", n)
		}
		time.Sleep(startCheck)
		hs.d.serve(baseRef, shipped("sha256:a2", "base", digest(2)))
		hs.d.mu.Lock()
		hs.d.holdDigest = make(chan struct{})
		hs.d.mu.Unlock()
		if id, ok := hs.m.ForStart("small"); !ok || id != "sha256:a1" {
			t.Errorf("ForStart = %q, %v; want the image in service at once", id, ok)
		}
		hs.m.ForStart("small")
		synctest.Wait()
		if id, _ := hs.m.ForStart("small"); id != "sha256:a1" {
			t.Errorf("ForStart during a held digest ask = %q, want the image in service", id)
		}
		close(hs.d.holdDigest)
		synctest.Wait()
		if n := hs.d.digestCalls() - before; n != 1 {
			t.Errorf("digest asks = %d, want one for two starts in a minute", n)
		}
		if id, _ := hs.m.ForStart("small"); id != "sha256:a2" {
			t.Errorf("ForStart after the digest moved = %q, want the new image", id)
		}
		if s := hs.status(t, "small"); s.Digest != digest(2) {
			t.Errorf("digest = %q, want the new one", s.Digest)
		}
	})
}

func TestIdleCheckEveryRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, internetHost(), profileOf("small", "base"))
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		first := hs.status(t, "small").CheckedAt
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		if n := hs.d.digestCalls(); n != 0 {
			t.Errorf("digest asks before images.refresh = %d, want 0", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if n := hs.d.digestCalls(); n != 1 {
			t.Errorf("digest asks at images.refresh = %d, want 1", n)
		}
		if got := hs.status(t, "small").CheckedAt; !got.After(first) {
			t.Errorf("checked at %v, want later than %v", got, first)
		}
	})
}

func TestPinnedImageNeverAskedOfTheRegistry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ref := "registry.example/team/runner@" + digest(7)
		p := &config.Profile{Name: "pinned", Image: ref}
		hs := newHarness(t, internetHost(), p)
		r := shipped("sha256:p1", "", digest(7))
		r.img.RepoDigests = []string{"registry.example/team/runner@" + digest(7)}
		r.img.Config.Labels = nil
		hs.d.serve(ref, r)
		hs.start()
		defer hs.stop()
		first := hs.status(t, "pinned").CheckedAt
		time.Sleep(time.Hour)
		synctest.Wait()
		s := hs.status(t, "pinned")
		if !s.CheckedAt.After(first) || hs.d.digestCalls() != 0 {
			t.Errorf("checked at %v after %v with %d digest asks; want the inspect as the good check, no ask", s.CheckedAt, first, hs.d.digestCalls())
		}
		hs.d.forget("sha256:p1")
		time.Sleep(time.Hour)
		synctest.Wait()
		if n := len(hs.d.pullsOf(ref)); n != 2 {
			t.Errorf("pulls = %d, want the removed pinned image pulled again", n)
		}
		if s := hs.status(t, "pinned"); !s.Ready || s.ImageID != "sha256:p1" {
			t.Errorf("status = %+v, want ready again", s)
		}
	})
}

func TestCheckRefusesAnImage(t *testing.T) {
	root := shipped("sha256:r1", "base", digest(3))
	root.img.Config.User = "root"
	unlabelled := shipped("sha256:u1", "base", digest(4))
	unlabelled.img.Config.Labels = nil
	for name, r := range map[string]remote{"rootUser": root, "noLabel": unlabelled} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hs := newHarness(t, internetHost(), profileOf("small", "base"))
				hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
				hs.start()
				defer hs.stop()
				hs.d.serve(baseRef, r)
				time.Sleep(time.Hour)
				synctest.Wait()
				s := hs.status(t, "small")
				if !s.Ready || s.ImageID != "sha256:a1" || s.Failures != 1 || s.Failure == "" {
					t.Errorf("status = %+v, want the old image kept in service and the failure counted", s)
				}
				for _, c := range hs.d.containers {
					t.Errorf("image check container %s left behind", c.ID)
				}
			})
		})
	}
}

func TestAllowRootSkipsTheUserCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := profileOf("root", "base")
		p.AllowRoot = true
		hs := newHarness(t, internetHost(), p)
		r := shipped("sha256:r1", "base", digest(3))
		r.img.Config.User = "root"
		hs.d.serve(baseRef, r)
		hs.start()
		defer hs.stop()
		if s := hs.status(t, "root"); !s.Ready {
			t.Errorf("status = %+v, want a root image served with allow_root", s)
		}
	})
}

// offlineHost is an internet: false host whose one allow entry covers
// 10.1.0.0/16 on TCP 5000.
func offlineHost() *config.Host {
	return &config.Host{Name: "lab", ImageRefresh: time.Hour, Egress: config.Egress{Allow: []config.Allow{{
		To: netip.MustParsePrefix("10.1.0.0/16"), Protocol: config.ProtocolTCP, Ports: []config.PortRange{{First: 5000, Last: 5000}},
	}}}}
}

// A registry that resolved outside allow is served again at the next
// images.refresh once its answer falls inside, with no SIGHUP.
func TestRefusedRegistryServedAgainAtRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ref = "registry.lan:5000/ci/runner:1"
		hs := newHarness(t, offlineHost(), &config.Profile{Name: "lab", Image: ref})
		var answer atomic.Pointer[[]netip.Addr]
		answer.Store(&[]netip.Addr{netip.MustParseAddr("192.168.5.5"), netip.MustParseAddr("10.1.2.3")})
		var lookups atomic.Int32
		hs.cfg.Lookup = func(_ context.Context, name string) ([]netip.Addr, error) {
			lookups.Add(1)
			if name != "registry.lan" {
				t.Errorf("looked up %q", name)
			}
			return *answer.Load(), nil
		}
		r := shipped("sha256:l1", "", digest(5))
		r.img.RepoDigests = []string{"registry.lan:5000/ci/runner@" + digest(5)}
		hs.d.serve(ref, r)
		hs.start()
		defer hs.stop()
		s := hs.status(t, "lab")
		if s.Ready || s.Reason != hostapi.ReasonRefused || len(hs.d.pullsOf(ref)) != 0 {
			t.Fatalf("status = %+v with %d pulls; want refused and nothing pulled", s, len(hs.d.pullsOf(ref)))
		}
		for _, want := range []string{"192.168.5.5, 10.1.2.3", "192.168.5.5 lie outside", "10.1.0.0/16 tcp 5000", "add an allow entry", "answer must move inside"} {
			if !strings.Contains(s.Detail, want) {
				t.Errorf("detail %q lacks %q", s.Detail, want)
			}
		}
		answer.Store(&[]netip.Addr{netip.MustParseAddr("10.1.2.3")})
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		if s := hs.status(t, "lab"); s.Ready || lookups.Load() != 1 {
			t.Errorf("status = %+v after %d lookups; want still refused before images.refresh", s, lookups.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if s := hs.status(t, "lab"); !s.Ready || s.Reason != "" {
			t.Errorf("status = %+v, want served at images.refresh", s)
		}
	})
}

func TestReloadResolvesARefusedRegistryAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ref = "registry.lan:5000/ci/runner:1"
		p := &config.Profile{Name: "lab", Image: ref}
		hs := newHarness(t, offlineHost(), p)
		answer := netip.MustParseAddr("192.168.5.5")
		hs.cfg.Lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{answer}, nil }
		r := shipped("sha256:l1", "", digest(5))
		r.img.RepoDigests = []string{"registry.lan:5000/ci/runner@" + digest(5)}
		hs.d.serve(ref, r)
		hs.start()
		defer hs.stop()
		h := offlineHost()
		h.Egress.Allow[0].To = netip.MustParsePrefix("192.168.5.0/24")
		hs.m.Reload(h, []*config.Profile{p})
		synctest.Wait()
		if s := hs.status(t, "lab"); !s.Ready {
			t.Errorf("status = %+v, want served at once after the reload", s)
		}
	})
}

func TestFailedLookupBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ref = "registry.lan:5000/ci/runner:1"
		hs := newHarness(t, offlineHost(), &config.Profile{Name: "lab", Image: ref})
		var mu sync.Mutex
		var at []time.Time
		hs.cfg.Lookup = func(context.Context, string) ([]netip.Addr, error) {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, time.Now())
			return nil, errors.New("no such host")
		}
		hs.start()
		defer hs.stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		s := hs.status(t, "lab")
		if s.Reason != hostapi.ReasonPulling || !strings.Contains(s.Failure, "resolving the registry registry.lan") {
			t.Errorf("status = %+v, want the failed lookup reported as a failed pull", s)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(at) != 3 || at[1].Sub(at[0]) != retryFirst || at[2].Sub(at[1]) != 2*retryFirst {
			t.Errorf("lookups at %v, want the pull backoff", at)
		}
	})
}

func TestGitHubRegistryReachedByName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := newHarness(t, offlineHost(), profileOf("small", "base"))
		hs.cfg.ByName = func(name string) bool { return name == "ghcr.io" }
		hs.cfg.Lookup = func(context.Context, string) ([]netip.Addr, error) {
			t.Error("a registry in GitHub's list was resolved")
			return nil, nil
		}
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.start()
		defer hs.stop()
		if s := hs.status(t, "small"); !s.Ready {
			t.Errorf("status = %+v, want ghcr.io pulled by name", s)
		}
	})
}

func TestReloadKeepsAnUnchangedProfile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		small := profileOf("small", "base")
		hs := newHarness(t, internetHost(), small)
		hs.d.serve(baseRef, shipped("sha256:a1", "base", digest(1)))
		hs.d.serve(buildRef, shipped("sha256:b1", "build", digest(2)))
		hs.start()
		defer hs.stop()
		hs.d.pullErr[baseRef] = errors.New("down")
		hs.m.Reload(internetHost(), []*config.Profile{profileOf("small", "base"), profileOf("build", "build")})
		if id, ok := hs.m.ForStart("small"); !ok || id != "sha256:a1" {
			t.Errorf("ForStart(small) = %q, %v; want its image kept across the reload", id, ok)
		}
		synctest.Wait()
		if s := hs.status(t, "build"); !s.Ready {
			t.Errorf("status = %+v, want the new profile brought into service", s)
		}
		if names := len(hs.m.Status()); names != 2 {
			t.Errorf("profiles = %d, want 2", names)
		}
	})
}
