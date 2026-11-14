package docker

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const imageID = "sha256:b3492d593e717af76e71e25dff4bb82d4fb35be4269f377cc2f4f40bd29b7c73"

func TestInspectImage(t *testing.T) {
	d := newDaemon(t, "1.43")
	d.handle("GET /images/", func(w http.ResponseWriter, r *http.Request) {
		ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		switch ref {
		case "ghcr.io/cplieger/manade-runner:base":
			writeJSON(w, http.StatusOK, map[string]any{
				"Id": imageID, "RepoTags": []string{"ghcr.io/cplieger/manade-runner:base"},
				"RepoDigests": []string{"ghcr.io/cplieger/manade-runner@sha256:759a"},
				"Config":      map[string]any{"User": "runner", "Labels": map[string]string{LabelRunnerImage: "base"}},
			})
		default:
			writeError(w, http.StatusNotFound, "No such image: "+ref)
		}
	})
	c := d.dial(t)
	t.Run("readsTheFields", func(t *testing.T) {
		img, found, err := c.InspectImage(t.Context(), "ghcr.io/cplieger/manade-runner:base")
		if err != nil || !found {
			t.Fatalf("InspectImage = %v, %v", found, err)
		}
		if img.ID != imageID || img.Config.User != "runner" || img.Config.Labels[LabelRunnerImage] != "base" {
			t.Errorf("image = %+v", img)
		}
		if !slices.Equal(img.RepoDigests, []string{"ghcr.io/cplieger/manade-runner@sha256:759a"}) {
			t.Errorf("RepoDigests = %v", img.RepoDigests)
		}
	})
	t.Run("missingIsNotFoundWithoutError", func(t *testing.T) {
		if _, found, err := c.InspectImage(t.Context(), "absent:1"); found || err != nil {
			t.Errorf("InspectImage = %v, %v; want not found, nil", found, err)
		}
	})
}

func TestPullImage(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("fromImage") {
		case "denied:1":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"Pulling from library/denied"}` + "\n" + `{"errorDetail":{"message":"x"},"error":"pull access denied\nfor denied"}` + "\n"))
		case "missing:1":
			writeError(w, http.StatusNotFound, "manifest unknown")
		case "huge:1":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"` + strings.Repeat("x", messageLimit) + `"}` + "\n"))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"Pulling fs layer","id":"7a48"}` + "\n" + `{"status":"Status: Downloaded newer image"}` + "\n"))
		}
	})
	c := d.dial(t)
	t.Run("aTaggedReferencePullsThatTag", func(t *testing.T) {
		if err := c.PullImage(t.Context(), "ghcr.io/actions/actions-runner:2.338.2", ""); err != nil {
			t.Fatalf("PullImage: %v", err)
		}
		reqs := d.seen(http.MethodPost, "/images/create")
		last := reqs[len(reqs)-1]
		if last.query["fromImage"][0] != "ghcr.io/actions/actions-runner:2.338.2" || last.query["tag"] != nil {
			t.Errorf("query = %v, want fromImage only", last.query)
		}
	})
	t.Run("aBareNamePullsLatestOnly", func(t *testing.T) {
		if err := c.PullImage(t.Context(), "registry.example:5000/team/runner", ""); err != nil {
			t.Fatalf("PullImage: %v", err)
		}
		reqs := d.seen(http.MethodPost, "/images/create")
		if q := reqs[len(reqs)-1].query; q["tag"] == nil || q["tag"][0] != "latest" {
			t.Errorf("query = %v, want tag=latest", q)
		}
	})
	t.Run("aStreamErrorFailsThePull", func(t *testing.T) {
		err := c.PullImage(t.Context(), "denied:1", "")
		if err == nil || !strings.Contains(err.Error(), "pull access denied for denied") {
			t.Errorf("PullImage = %v, want the stream's error on one line", err)
		}
	})
	t.Run("aStatusErrorFailsThePull", func(t *testing.T) {
		if err := c.PullImage(t.Context(), "missing:1", ""); !errors.Is(err, errNotFound) {
			t.Errorf("PullImage = %v, want errNotFound", err)
		}
	})
	t.Run("anOverlongMessageFailsThePull", func(t *testing.T) {
		if err := c.PullImage(t.Context(), "huge:1", ""); err == nil {
			t.Error("PullImage with a message over the cap succeeded")
		}
	})
}

func TestNameOnly(t *testing.T) {
	for ref, want := range map[string]bool{
		"ubuntu":                                               true,
		"ubuntu:24.04":                                         false,
		"localhost:5000/runner":                                true,
		"localhost:5000/runner:v1":                             false,
		"ghcr.io/actions/actions-runner":                       true,
		"runner@sha256:" + strings.Repeat("a", 64):             false,
		"host:5000/runner@sha256:" + strings.Repeat("a", 64):   false,
		"ghcr.io/actions/actions-runner:2.338.2@sha256:" + "0": false,
	} {
		if got := nameOnly(ref); got != want {
			t.Errorf("nameOnly(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestListImages(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /images/json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{{
			"Id": imageID, "RepoTags": []string{"ghcr.io/cplieger/manade-runner:base-v1"},
			"RepoDigests": []string{"ghcr.io/cplieger/manade-runner@sha256:759a"},
			"Labels":      map[string]string{LabelRunnerImage: "base"},
		}})
	})
	got, err := d.dial(t).ListImages(t.Context())
	if err != nil || len(got) != 1 {
		t.Fatalf("ListImages = %+v, %v", got, err)
	}
	if got[0].ID != imageID || got[0].Labels[LabelRunnerImage] != "base" ||
		!slices.Equal(got[0].RepoTags, []string{"ghcr.io/cplieger/manade-runner:base-v1"}) ||
		!slices.Equal(got[0].RepoDigests, []string{"ghcr.io/cplieger/manade-runner@sha256:759a"}) {
		t.Errorf("ListImages = %+v", got[0])
	}
	if q := d.seen(http.MethodGet, "/images/json")[0].query; q["all"] != nil {
		t.Errorf("query = %v, want intermediate layers left out", q)
	}
}

func TestRemoveImage(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("DELETE /images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("ref") {
		case "sha256:ddd":
			writeError(w, http.StatusConflict, "image is being used by stopped container 1a2b")
		case "sha256:eee", "gone:1":
			writeError(w, http.StatusNotFound, "No such image: "+r.PathValue("ref"))
		case "sha256:fff":
			writeError(w, http.StatusInternalServerError, "driver failed")
		default:
			writeJSON(w, http.StatusOK, []map[string]string{{"Untagged": r.PathValue("ref")}})
		}
	})
	c := d.dial(t)
	t.Run("removesWithoutForce", func(t *testing.T) {
		for _, ref := range []string{"sha256:aaa", "ghcr.io/cplieger/manade-runner:base-v1"} {
			if err := c.RemoveImage(t.Context(), ref); err != nil {
				t.Errorf("RemoveImage(%s): %v", ref, err)
			}
			del := d.seen(http.MethodDelete, "/images/"+ref)
			if len(del) != 1 || del[0].query["force"] != nil {
				t.Errorf("deletes of %s = %+v, want one without force", ref, del)
			}
		}
	})
	t.Run("anImageInUseIsAConflict", func(t *testing.T) {
		if err := c.RemoveImage(t.Context(), "sha256:ddd"); !errors.Is(err, ErrConflict) {
			t.Errorf("RemoveImage = %v, want ErrConflict", err)
		}
	})
	t.Run("anImageAlreadyGoneIsNoError", func(t *testing.T) {
		for _, ref := range []string{"sha256:eee", "gone:1"} {
			if err := c.RemoveImage(t.Context(), ref); err != nil {
				t.Errorf("RemoveImage(%s) = %v, want nil", ref, err)
			}
		}
	})
	t.Run("anotherFailureIsAnError", func(t *testing.T) {
		if err := c.RemoveImage(t.Context(), "sha256:fff"); err == nil || errors.Is(err, ErrConflict) {
			t.Errorf("RemoveImage = %v, want the daemon's failure", err)
		}
	})
}
