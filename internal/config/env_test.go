package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every variable Load reads, so the runner's own
// environment cannot leak into a case.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CONFIG_FILE", "LOG_LEVEL", "POOL_NAME", "DIAG_DIR", "DIAG_RETENTION", "TLS_DIR", "METRICS_LISTEN_ADDR",
		"GITHUB_PROXY", "GITHUB_APP_CLIENT_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_FILE",
		"GITHUB_TOKEN", "GITHUB_TOKEN_FILE", "MANADE_HOST", "MANADE_API_LISTEN", "MANADE_API_UID",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// writeConfig writes doc as the config file Load reads.
func writeConfig(t *testing.T, doc string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
}

func TestLoadOrchestrator(t *testing.T) {
	clearEnv(t)
	writeConfig(t, withProfile(""))
	t.Setenv("GITHUB_TOKEN", "test-token")
	cfg, err := Load(t.Context(), RoleOrchestrator)
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.Orchestrator
	if o == nil || cfg.Self != nil {
		t.Fatalf("orchestrator = %v, self = %v, want the orchestrator's environment only", o, cfg.Self)
	}
	t.Run("tlsDir", func(t *testing.T) {
		if o.TLSDir != "/secrets/tls" {
			t.Errorf("TLS_DIR = %q, want /secrets/tls", o.TLSDir)
		}
	})
	t.Run("poolName", func(t *testing.T) {
		if o.PoolName != "manade" {
			t.Errorf("POOL_NAME = %q", o.PoolName)
		}
	})
	t.Run("diagRetention", func(t *testing.T) {
		if o.DiagRetention != 168*time.Hour {
			t.Errorf("DIAG_RETENTION = %v", o.DiagRetention)
		}
	})
	t.Run("token", func(t *testing.T) {
		if o.GitHub.Token.Reveal() != "test-token" || o.GitHub.AppKey != nil {
			t.Errorf("credential = %v", o.GitHub)
		}
	})
}

func TestLoadHost(t *testing.T) {
	doc := withHosts("  nas: {capacity: {cpus: 2, memory: 3GiB}}\n  default: {capacity: {cpus: 8, memory: 24GiB}}\n", "nas, default")
	load := func(t *testing.T, env map[string]string) (*Config, error) {
		t.Helper()
		clearEnv(t)
		writeConfig(t, doc)
		for k, v := range env {
			t.Setenv(k, v)
		}
		return Load(t.Context(), RoleHost)
	}
	t.Run("defaults", func(t *testing.T) {
		cfg, err := load(t, map[string]string{"MANADE_HOST": "nas"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Orchestrator != nil {
			t.Errorf("orchestrator = %v, want none on a host", cfg.Orchestrator)
		}
		want := Self{Name: "nas", Listen: Address{Socket: "/run/manade/hosts/nas/api.sock"}, TLSDir: "/tls", APIUID: 65532}
		if *cfg.Self != want {
			t.Errorf("self = %+v, want %+v", *cfg.Self, want)
		}
	})
	t.Run("tcpListen", func(t *testing.T) {
		cfg, err := load(t, map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": ":7464", "MANADE_API_UID": "1000", "TLS_DIR": "/run/tls"})
		if err != nil {
			t.Fatal(err)
		}
		want := Self{Name: "nas", Listen: Address{HostPort: ":7464"}, TLSDir: "/run/tls", APIUID: 1000}
		if *cfg.Self != want {
			t.Errorf("self = %+v, want %+v", *cfg.Self, want)
		}
	})
	t.Run("socketListen", func(t *testing.T) {
		cfg, err := load(t, map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": "unix:///run/manade/api.sock"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Self.Listen != (Address{Socket: "/run/manade/api.sock"}) {
			t.Errorf("listen = %+v", cfg.Self.Listen)
		}
	})
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"nameMissing", map[string]string{}, []string{"MANADE_HOST: required"}},
		{"nameUnknown", map[string]string{"MANADE_HOST": "lan"}, []string{`names no host "lan"`}},
		{"nameIPLiteral", map[string]string{"MANADE_HOST": "10.0.0.1"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"nameSlash", map[string]string{"MANADE_HOST": "../nas"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"nameUpper", map[string]string{"MANADE_HOST": "NAS"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"listenBad", map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": "7464"}, []string{"MANADE_API_LISTEN: want host:port"}},
		{"listenSocketRelative", map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": "unix://api.sock"}, []string{"MANADE_API_LISTEN: want an absolute, clean path"}},
		{"uidBad", map[string]string{"MANADE_HOST": "nas", "MANADE_API_UID": "-1"}, []string{"MANADE_API_UID: want a numeric uid"}},
		{"tlsDirRelative", map[string]string{"MANADE_HOST": "nas", "TLS_DIR": "tls"}, []string{"TLS_DIR: want an absolute, clean path"}},
		{"credentialGiven", map[string]string{"MANADE_HOST": "nas", "GITHUB_TOKEN": "test-token"}, []string{"GITHUB_TOKEN: a host is never given the orchestrator's GitHub credential"}},
		{"keyFileGiven", map[string]string{"MANADE_HOST": "nas", "GITHUB_APP_PRIVATE_KEY_FILE": "/secrets/github-app.pem"}, []string{"GITHUB_APP_PRIVATE_KEY_FILE: a host is never given"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.env)
			wantErr(t, err, tt.want...)
			if err != nil && strings.Contains(err.Error(), "test-token") {
				t.Errorf("err = %q quotes the credential", err)
			}
		})
	}
}

func TestLoadOrchestratorRefusals(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"noCredential", map[string]string{}, []string{"set GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY, or GITHUB_TOKEN"}},
		{"tlsDirRelative", map[string]string{"GITHUB_TOKEN": "t", "TLS_DIR": "secrets/tls"}, []string{"TLS_DIR: want an absolute, clean path"}},
		{"poolNameLong", map[string]string{"GITHUB_TOKEN": "t", "POOL_NAME": "a-fifteen-chars"}, []string{"POOL_NAME"}},
		{"retention", map[string]string{"GITHUB_TOKEN": "t", "DIAG_RETENTION": "-1h"}, []string{"DIAG_RETENTION: want a positive duration"}},
		{"proxy", map[string]string{"GITHUB_TOKEN": "t", "GITHUB_PROXY": "socks5://p:1080"}, []string{"GITHUB_PROXY: want an http:// or https:// proxy URL"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			writeConfig(t, withProfile(""))
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load(t.Context(), RoleOrchestrator)
			wantErr(t, err, tt.want...)
		})
	}
}

func TestLoadRelativeConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	clearEnv(t)
	doc := oneHost("    registry_auth: "+dir+"/auth.json\n", ", mounts: [{bind: "+dir+"/data, target: /m}]")
	if err := os.WriteFile("config.yaml", []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", "config.yaml")
	t.Setenv("MANADE_HOST", "h")
	t.Setenv("TLS_DIR", dir+"/tls")
	_, err := Load(t.Context(), RoleHost)
	wantErr(t, err,
		`TLS_DIR: "`+dir+`/tls" lies at or under the config file's directory "`+dir+`"`,
		`hosts.h.registry_auth: "`+dir+`/auth.json" lies at or under the config file's directory "`+dir+`"`,
		`profiles.p.mounts[0].bind: "`+dir+`/data" reaches `+dir+`, which host "h" protects`)
}

func TestLoadFileBounds(t *testing.T) {
	clearEnv(t)
	t.Setenv("GITHUB_TOKEN", "t")
	t.Run("overOneMiB", func(t *testing.T) {
		writeConfig(t, withProfile("")+"#"+strings.Repeat("x", 1<<20)+"\n")
		_, err := Load(t.Context(), RoleOrchestrator)
		wantErr(t, err, "CONFIG_FILE")
	})
	t.Run("missing", func(t *testing.T) {
		t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "absent.yaml"))
		_, err := Load(t.Context(), RoleOrchestrator)
		wantErr(t, err, "CONFIG_FILE")
	})
}

func TestEnterpriseNeedsToken(t *testing.T) {
	doc := strings.Replace(withProfile(""), "org: acme", "enterprise: acme", 1)
	t.Run("app", func(t *testing.T) {
		wantErr(t, build(appEnv(t), []byte(doc)), "scopes[2].enterprise: an enterprise scope needs GITHUB_TOKEN")
	})
	t.Run("token", func(t *testing.T) {
		mustBuild(t, tokenEnv(), doc)
	})
	t.Run("host", func(t *testing.T) {
		mustBuild(t, &Config{File: "/config/config.yaml", Self: &Self{Name: "default", TLSDir: "/tls"}}, doc)
	})
}
