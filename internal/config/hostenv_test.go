package config

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// clearEnv unsets every variable LoadSelf reads, so the runner's own
// environment cannot leak into a case.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"LOG_LEVEL", "TLS_DIR", "MANADE_HOST", "MANADE_API_LISTEN", "MANADE_API_UID",
		"MANADE_IPV6_ECHO", "MANADE_REGISTRY_AUTH", "MANADE_REGISTRY_AUTH_FILE",
		"GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_TOKEN", "GITHUB_TOKEN_FILE",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// loadSelf loads a host's environment env.
func loadSelf(t *testing.T, env map[string]string) (*Self, error) {
	t.Helper()
	clearEnv(t)
	for k, v := range env {
		t.Setenv(k, v)
	}
	return LoadSelf()
}

func TestLoadSelf(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		self, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas"})
		if err != nil {
			t.Fatal(err)
		}
		want := Self{
			Name: "nas", Listen: Address{Socket: "/run/manade/hosts/nas/api.sock"}, TLSDir: "/tls", APIUID: 65532,
			IPv6Echo: "https://cloudflare.com/cdn-cgi/trace", LogLevel: slog.LevelInfo,
		}
		if *self != want {
			t.Errorf("self = %+v, want %+v", *self, want)
		}
	})
	t.Run("tcpListen", func(t *testing.T) {
		cfg, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": ":7464", "MANADE_API_UID": "1000", "TLS_DIR": "/run/tls"})
		if err != nil {
			t.Fatal(err)
		}
		if s := cfg; s.Listen != (Address{HostPort: ":7464", Port: 7464}) || s.TLSDir != "/run/tls" || s.APIUID != 1000 {
			t.Errorf("self = %+v", *s)
		}
	})
	t.Run("socketListen", func(t *testing.T) {
		cfg, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": "unix:///run/manade/api.sock"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Listen != (Address{Socket: "/run/manade/api.sock"}) {
			t.Errorf("listen = %+v", cfg.Listen)
		}
	})
	t.Run("ownEcho", func(t *testing.T) {
		cfg, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "MANADE_IPV6_ECHO": "https://echo.example/ip"})
		if err != nil || cfg.IPv6Echo != "https://echo.example/ip" {
			t.Errorf("echo = %+v, %v; want the host's own", cfg, err)
		}
	})
	t.Run("logLevel", func(t *testing.T) {
		cfg, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "LOG_LEVEL": "debug"})
		if err != nil || cfg.LogLevel != slog.LevelDebug || cfg.LogLevelInvalid {
			t.Errorf("self = %+v, %v; want debug", cfg, err)
		}
		cfg, err = loadSelf(t, map[string]string{"MANADE_HOST": "nas", "LOG_LEVEL": "loud"})
		if err != nil || cfg.LogLevel != slog.LevelInfo || !cfg.LogLevelInvalid {
			t.Errorf("self = %+v, %v; want info flagged invalid", cfg, err)
		}
	})
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"nameMissing", map[string]string{}, []string{"MANADE_HOST: required"}},
		{"nameIPLiteral", map[string]string{"MANADE_HOST": "10.0.0.1"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"nameSlash", map[string]string{"MANADE_HOST": "../nas"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"nameUpper", map[string]string{"MANADE_HOST": "NAS"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"nameReserved", map[string]string{"MANADE_HOST": "orchestrator"}, []string{"MANADE_HOST: want a name other than ca and orchestrator"}},
		{"listenBad", map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": "7464"}, []string{"MANADE_API_LISTEN: want host:port"}},
		{"listenSignedPort", map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": ":+7464"}, []string{"MANADE_API_LISTEN: want host:port"}},
		{"listenSocketRelative", map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": "unix://api.sock"}, []string{"MANADE_API_LISTEN: want an absolute, clean path"}},
		{"uidBad", map[string]string{"MANADE_HOST": "nas", "MANADE_API_UID": "-1"}, []string{"MANADE_API_UID: want a numeric uid"}},
		{"tlsDirRelative", map[string]string{"MANADE_HOST": "nas", "TLS_DIR": "tls"}, []string{"TLS_DIR: want an absolute, clean path"}},
		{"echoNotHTTPS", map[string]string{"MANADE_HOST": "nas", "MANADE_IPV6_ECHO": "http://echo.example"}, []string{"MANADE_IPV6_ECHO: want an https:// URL"}},
		{"echoUserinfo", map[string]string{"MANADE_HOST": "nas", "MANADE_IPV6_ECHO": "https://u@echo.example"}, []string{"MANADE_IPV6_ECHO: want an https:// URL"}},
		{"registryAuthBoth", map[string]string{"MANADE_HOST": "nas", "MANADE_REGISTRY_AUTH": "{}", "MANADE_REGISTRY_AUTH_FILE": "/auth.json"}, []string{"set MANADE_REGISTRY_AUTH or MANADE_REGISTRY_AUTH_FILE, not both"}},
		{"registryAuthRelative", map[string]string{"MANADE_HOST": "nas", "MANADE_REGISTRY_AUTH_FILE": "auth.json"}, []string{"MANADE_REGISTRY_AUTH_FILE: want an absolute, clean path"}},
		{"credentialGiven", map[string]string{"MANADE_HOST": "nas", "GITHUB_TOKEN": "test-token"}, []string{"GITHUB_TOKEN: a host is never given the control plane's GitHub credential"}},
		{"keyFileGiven", map[string]string{"MANADE_HOST": "nas", "GITHUB_APP_PRIVATE_KEY_FILE": "/secrets/github-app.pem"}, []string{"GITHUB_APP_PRIVATE_KEY_FILE: a host is never given"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadSelf(t, tt.env)
			wantErr(t, err, tt.want...)
			if err != nil && strings.Contains(err.Error(), "test-token") {
				t.Errorf("err = %q quotes the credential", err)
			}
		})
	}
}

func TestLoadSelfRegistryAuth(t *testing.T) {
	t.Run("value", func(t *testing.T) {
		const secret = `{"auths": {"r.example": {"auth": "s3cret"}}}`
		cfg, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "MANADE_REGISTRY_AUTH": secret})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.RegistryAuth; a.File != "" || a.Value.Reveal() != secret {
			t.Errorf("registry auth = %+v, want the value", a)
		}
	})
	t.Run("fileKeepsItsPath", func(t *testing.T) {
		cfg, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "MANADE_REGISTRY_AUTH_FILE": "/run/secrets/registry"})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.RegistryAuth; a.File != "/run/secrets/registry" || a.Value.Reveal() != "" {
			t.Errorf("registry auth = %+v, want the file's path", a)
		}
	})
	t.Run("none", func(t *testing.T) {
		cfg, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas"})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.RegistryAuth; a.File != "" || a.Value.Reveal() != "" {
			t.Errorf("registry auth = %+v, want none", a)
		}
	})
}
