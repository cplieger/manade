package docker

import (
	"net/http"
	"strings"
	"testing"
)

func TestDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("ab", 32)
	d := newDaemon(t, "1.51")
	d.handle("GET /distribution/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/distribution/"), "/json") {
		case "ghcr.io/cplieger/manade-runner:base-v1-dev":
			writeJSON(w, http.StatusOK, map[string]any{"Descriptor": map[string]any{"digest": good, "mediaType": "application/vnd.oci.image.index.v1+json"}})
		case "ghcr.io/x/bad:1":
			writeJSON(w, http.StatusOK, map[string]any{"Descriptor": map[string]any{"digest": "md5:\nabc"}})
		default:
			writeError(w, http.StatusUnauthorized, "authentication required")
		}
	})
	c := d.dial(t)
	t.Run("answers the tag's digest and sends the credential", func(t *testing.T) {
		got, err := c.Digest(t.Context(), "ghcr.io/cplieger/manade-runner:base-v1-dev", "eyJ1c2VybmFtZSI6InUifQ==")
		if err != nil || got != good {
			t.Fatalf("Digest = %q, %v", got, err)
		}
		seen := d.seen(http.MethodGet, "/distribution/ghcr.io/cplieger/manade-runner:base-v1-dev/json")
		if len(seen) != 1 || seen[0].header.Get(authHeader) != "eyJ1c2VybmFtZSI6InUifQ==" {
			t.Errorf("requests = %+v", seen)
		}
	})
	t.Run("no credential sends no header", func(t *testing.T) {
		if _, err := c.Digest(t.Context(), "ghcr.io/cplieger/manade-runner:base-v1-dev", ""); err != nil {
			t.Fatal(err)
		}
		seen := d.seen(http.MethodGet, "/distribution/ghcr.io/cplieger/manade-runner:base-v1-dev/json")
		if _, sent := seen[len(seen)-1].header[authHeader]; sent {
			t.Error("an empty credential sent the header")
		}
	})
	t.Run("a malformed digest fails on one line", func(t *testing.T) {
		if _, err := c.Digest(t.Context(), "ghcr.io/x/bad:1", ""); err == nil || strings.Contains(err.Error(), "\n") {
			t.Errorf("Digest = %v", err)
		}
	})
	t.Run("a registry refusal fails", func(t *testing.T) {
		if _, err := c.Digest(t.Context(), "ghcr.io/x/private:1", ""); err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("Digest = %v", err)
		}
	})
}

func TestPullImageSendsTheCredential(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /images/create", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"Status: Image is up to date"}` + "\n"))
	})
	c := d.dial(t)
	if err := c.PullImage(t.Context(), "ghcr.io/x/private:1", "Y3JlZA=="); err != nil {
		t.Fatal(err)
	}
	if h := d.seen(http.MethodPost, "/images/create")[0].header.Get(authHeader); h != "Y3JlZA==" {
		t.Errorf("%s = %q", authHeader, h)
	}
}

func TestLoadImage(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /images/load", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-tar" {
			writeError(w, http.StatusBadRequest, "not a tar")
			return
		}
		_, _ = w.Write([]byte(`{"stream":"Loaded image: manade-probe:v1.0.0-dev.11\n"}` + "\n"))
	})
	c := d.dial(t)
	if err := c.LoadImage(t.Context(), strings.NewReader("tar bytes")); err != nil {
		t.Fatalf("LoadImage = %v", err)
	}
	seen := d.seen(http.MethodPost, "/images/load")
	if len(seen) != 1 || string(seen[0].body) != "tar bytes" || seen[0].query["quiet"][0] != "1" {
		t.Errorf("requests = %+v", seen)
	}
	t.Run("a stream error fails the load", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /images/load", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"error":"unexpected EOF"}` + "\n"))
		})
		if err := d.dial(t).LoadImage(t.Context(), strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Errorf("LoadImage = %v", err)
		}
	})
}
