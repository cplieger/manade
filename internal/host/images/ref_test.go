package images

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/docker"
)

func TestRepository(t *testing.T) {
	// Docker's normalization, distribution/reference ParseNormalizedNamed.
	for ref, want := range map[string]string{
		"ubuntu":                              "docker.io/library/ubuntu",
		"ubuntu:24.04":                        "docker.io/library/ubuntu",
		"library/ubuntu@" + digest(1):         "docker.io/library/ubuntu",
		"docker.io/library/ubuntu:24.04":      "docker.io/library/ubuntu",
		"team/app:1":                          "docker.io/team/app",
		"ghcr.io/cplieger/manade-runner:base": "ghcr.io/cplieger/manade-runner",
		"registry.lan:5000/ci/runner:1":       "registry.lan:5000/ci/runner",
		"registry.lan:5000/ci/runner":         "registry.lan:5000/ci/runner",
		"localhost/app@" + digest(2):          "localhost/app",
	} {
		if got := repository(ref); got != want {
			t.Errorf("repository(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestDigestOf(t *testing.T) {
	img := &docker.Image{RepoDigests: []string{"ubuntu@" + digest(1), "ghcr.io/x/y@" + digest(2)}}
	for ref, want := range map[string]string{
		"docker.io/library/ubuntu:24.04": digest(1),
		"ghcr.io/x/y:1":                  digest(2),
		"ghcr.io/x/z:1":                  "",
	} {
		if got := digestOf(ref, img); got != want {
			t.Errorf("digestOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestPinned(t *testing.T) {
	if !pinned("ghcr.io/x/y@"+digest(1)) || pinned("registry.lan:5000/x:1") {
		t.Error("pinned misread a reference")
	}
}

func decodeAuth(t *testing.T, s string) map[string]string {
	t.Helper()
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("X-Registry-Auth %q is not base64url: %v", s, err)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEncodeAuth(t *testing.T) {
	file := []byte(`{"auths": {
		"https://index.docker.io/v1/": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("hubuser:hub:pass")) + `"},
		"registry.lan:5000": {"username": "ci", "password": "s3cret"},
		"https://token.example/v2/": {"identitytoken": "tok"}
	}}`)
	t.Run("dockerHubUnderTheCLIKey", func(t *testing.T) {
		got, err := encodeAuth(file, "team/app:1", "/auth.json")
		a := decodeAuth(t, got)
		if err != nil || a["username"] != "hubuser" || a["password"] != "hub:pass" || a["serveraddress"] != "https://index.docker.io/v1/" {
			t.Errorf("auth = %v, %v", a, err)
		}
	})
	t.Run("aRegistryWithAPort", func(t *testing.T) {
		got, err := encodeAuth(file, "registry.lan:5000/ci/runner:1", "/auth.json")
		a := decodeAuth(t, got)
		if err != nil || a["username"] != "ci" || a["password"] != "s3cret" {
			t.Errorf("auth = %v, %v", a, err)
		}
	})
	t.Run("anIdentityToken", func(t *testing.T) {
		got, err := encodeAuth(file, "token.example/app:1", "/auth.json")
		a := decodeAuth(t, got)
		if err != nil || a["identitytoken"] != "tok" || a["password"] != "" {
			t.Errorf("auth = %v, %v", a, err)
		}
	})
	t.Run("noEntryIsNoAuth", func(t *testing.T) {
		if got, err := encodeAuth(file, "ghcr.io/x/y:1", "/auth.json"); got != "" || err != nil {
			t.Errorf("auth = %q, %v; want none", got, err)
		}
	})
	t.Run("aBadFileNeverQuotesItself", func(t *testing.T) {
		for _, bad := range []string{`not json s3cret`, `{"auths": {"registry.lan:5000": {"auth": "s3cret!"}}}`} {
			_, err := encodeAuth([]byte(bad), "registry.lan:5000/x:1", "/auth.json")
			if !errors.Is(err, errAuth) || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "/auth.json") {
				t.Errorf("encodeAuth(%q) = %v; want errAuth naming the path, never the content", bad, err)
			}
		}
	})
}

func TestAuthReadsTheFileAtEachCall(t *testing.T) {
	hs := newHarness(t, internetHost())
	path := filepath.Join(t.TempDir(), "auth.json")
	hs.cfg.Host.RegistryAuth = path
	m := New(hs.cfg)
	if _, err := m.auth(t.Context(), "registry.lan:5000/x:1"); !errors.Is(err, errAuth) {
		t.Errorf("auth with no file = %v, want errAuth", err)
	}
	for _, user := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(`{"auths":{"registry.lan:5000":{"username":"`+user+`","password":"p"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := m.auth(t.Context(), "registry.lan:5000/x:1")
		if err != nil || decodeAuth(t, got)["username"] != user {
			t.Errorf("auth = %v, %v; want the file's current %s", decodeAuth(t, got), err, user)
		}
	}
	hs.cfg.Host.RegistryAuth = ""
	if got, err := New(hs.cfg).auth(t.Context(), "registry.lan:5000/x:1"); got != "" || err != nil {
		t.Errorf("auth without registry_auth = %q, %v; want none", got, err)
	}
}
