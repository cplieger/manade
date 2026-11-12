package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/forgeapi/v2/creds"
)

const header = `capacity: {cpus: 16, memory: 64GiB}
scopes:
  - repo: acme/app
  - repo: acme/web
  - org: acme
`

// testPEM is a throwaway App key generated per test binary, so no key is
// ever committed.
var testPEM = sync.OnceValue(func() []byte {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
})

// tokenEnv is the environment's part of a Config under a PAT.
func tokenEnv() *Config {
	return &Config{GitHub: GitHub{Token: Secret{v: "test-token"}}, PoolName: "manade", DockerSocket: "/var/run/docker.sock"}
}

func appEnv(t *testing.T) *Config {
	t.Helper()
	key, err := creds.ParseAppKeyPEM("Iv1.test", testPEM())
	if err != nil {
		t.Fatal(err)
	}
	return &Config{GitHub: GitHub{AppKey: key}, PoolName: "manade", DockerSocket: "/var/run/docker.sock"}
}

// withProfile builds header plus one profile "p" whose body is the flow
// mapping's contents.
func withProfile(body string) string {
	return header + "profiles:\n  p: {" + body + "}\n"
}

func mustBuild(t *testing.T, cfg *Config, doc string) *Config {
	t.Helper()
	if err := build(cfg, []byte(doc)); err != nil {
		t.Fatalf("build: %v", err)
	}
	return cfg
}

// wantErr asserts err holds every substring.
func wantErr(t *testing.T, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %q", subs)
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("err = %q, want it to contain %q", err, s)
		}
	}
}

func profile(t *testing.T, cfg *Config, name string) *Profile {
	t.Helper()
	for i := range cfg.Profiles {
		if cfg.Profiles[i].Name == name {
			return &cfg.Profiles[i]
		}
	}
	t.Fatalf("no profile %q", name)
	return nil
}
