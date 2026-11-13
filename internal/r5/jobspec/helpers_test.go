package jobspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cplieger/manade/internal/r5/config"
)

const header = `capacity: {cpus: 16, memory: 64GiB}
scopes:
  - repo: acme/app
`

// load validates header plus extra through config.Load, so every profile a
// test reads carries config's own defaults.
func load(t *testing.T, extra string) *config.Config {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(header+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"CONFIG_FILE": file, "GITHUB_TOKEN": "test-token", "POOL_NAME": "manade", "DOCKER_SOCKET": "/var/run/docker.sock",
		"GITHUB_APP_CLIENT_ID": "", "GITHUB_APP_PRIVATE_KEY": "", "GITHUB_APP_PRIVATE_KEY_FILE": "", "GITHUB_TOKEN_FILE": "",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load(t.Context())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// profileOf loads one profile "p" whose body is a flow mapping's contents.
func profileOf(t *testing.T, body string) (*Host, *config.Profile) {
	t.Helper()
	cfg := load(t, "profiles:\n  p: {"+body+"}\n")
	return NewHost(cfg, 999), &cfg.Profiles[0]
}

var testRunner = Runner{Scope: "acme/app", Name: "p-abc123", ImageID: "sha256:feed", JITConfig: "test-jit", ID: 42}

// flatten maps every leaf of v's JSON form to its value, keyed by its path.
func flatten(t *testing.T, v any) map[string]string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var walk func(path string, n any)
	walk = func(path string, n any) {
		switch n := n.(type) {
		case map[string]any:
			for k, c := range n {
				walk(path+"."+k, c)
			}
		case []any:
			for i, c := range n {
				walk(path+"."+strconv.Itoa(i), c)
			}
		default:
			b, _ := json.Marshal(n)
			out[path[1:]] = string(b)
		}
	}
	walk("", tree)
	return out
}
