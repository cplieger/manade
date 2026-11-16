package config

import (
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// clearEnv unsets every variable LoadSelf reads, so the runner's own
// environment cannot leak into a case.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"LOG_LEVEL", "MANADE_HOST",
		"MANADE_IPV6_ECHO", "MANADE_REGISTRY_AUTH", "MANADE_REGISTRY_AUTH_FILE",
		"MANADE_ALLOW_OPEN", "MANADE_ALLOW_INTERNET", "MANADE_ALLOW_IPV6", "MANADE_ALLOW_PRIVATE", "MANADE_BLOCK6",
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
		self, err := loadSelf(t, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		want := Self{Name: "default", IPv6Echo: "https://cloudflare.com/cdn-cgi/trace", LogLevel: slog.LevelInfo}
		if !reflect.DeepEqual(*self, want) {
			t.Errorf("self = %+v, want %+v", *self, want)
		}
	})
	t.Run("name", func(t *testing.T) {
		self, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas"})
		if err != nil || self.Name != "nas" {
			t.Errorf("self = %+v, %v; want the name nas", self, err)
		}
	})
	t.Run("formerlyReservedName", func(t *testing.T) {
		self, err := loadSelf(t, map[string]string{"MANADE_HOST": "orchestrator"})
		if err != nil || self.Name != "orchestrator" {
			t.Errorf("self = %+v, %v; want the name accepted", self, err)
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
		{"nameIPLiteral", map[string]string{"MANADE_HOST": "10.0.0.1"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"nameSlash", map[string]string{"MANADE_HOST": "../nas"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
		{"nameUpper", map[string]string{"MANADE_HOST": "NAS"}, []string{"MANADE_HOST: want a lowercase DNS label"}},
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

// The shipped compose files set no ceiling variable, so a new host is
// closed: no open egress, no IPv6 and no private destination.
func TestLoadSelfCeilingDefaults(t *testing.T) {
	self, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas"})
	if err != nil {
		t.Fatal(err)
	}
	if c := self.Ceiling; !reflect.DeepEqual(c, Ceiling{}) {
		t.Errorf("ceiling = %+v, want the zero ceiling", c)
	}
}

// Each ceiling variable's fail direction (tier 3): a toggle reads true only
// as "true", anything else but "false" reading false with a warning; a
// malformed MANADE_ALLOW_PRIVATE entry is skipped with a warning, which
// narrows reach; a malformed MANADE_BLOCK6 entry refuses start, since
// skipping it would widen reach.
func TestLoadSelfCeiling(t *testing.T) {
	for _, tc := range []struct {
		env      map[string]string
		want     Ceiling
		name     string
		warnings []string
	}{
		{name: "openTrue", env: map[string]string{"MANADE_ALLOW_OPEN": "true"}, want: Ceiling{Open: true}},
		{name: "openFalse", env: map[string]string{"MANADE_ALLOW_OPEN": "false"}},
		{name: "openMisspelt", env: map[string]string{"MANADE_ALLOW_OPEN": "TRUE"}, warnings: []string{"MANADE_ALLOW_OPEN: \"TRUE\" is neither true nor false, so it reads false"}},
		{name: "openOne", env: map[string]string{"MANADE_ALLOW_OPEN": "1"}, warnings: []string{"MANADE_ALLOW_OPEN: \"1\""}},
		{name: "internetFalse", env: map[string]string{"MANADE_ALLOW_INTERNET": "false"}, want: Ceiling{NoInternet: true}},
		{name: "internetTrue", env: map[string]string{"MANADE_ALLOW_INTERNET": "true"}},
		{name: "internetMisspelt", env: map[string]string{"MANADE_ALLOW_INTERNET": "yes"}, want: Ceiling{NoInternet: true}, warnings: []string{"MANADE_ALLOW_INTERNET: \"yes\""}},
		{name: "ipv6True", env: map[string]string{"MANADE_ALLOW_IPV6": "true"}, want: Ceiling{IPv6: true}},
		{name: "ipv6Misspelt", env: map[string]string{"MANADE_ALLOW_IPV6": "on"}, warnings: []string{"MANADE_ALLOW_IPV6: \"on\""}},
		{
			name: "private", env: map[string]string{"MANADE_ALLOW_PRIVATE": "10.0.0.0/8, fd00::/8,192.168.1.53"},
			want: Ceiling{Private: []netip.Prefix{pfx("10.0.0.0/8"), pfx("fd00::/8"), pfx("192.168.1.53/32")}},
		},
		{
			name: "privateMalformedSkipped", env: map[string]string{"MANADE_ALLOW_PRIVATE": "10.0.0.0/8,lan,10.0.0.1/8"},
			want:     Ceiling{Private: []netip.Prefix{pfx("10.0.0.0/8")}},
			warnings: []string{"MANADE_ALLOW_PRIVATE: skipped \"lan\"", "MANADE_ALLOW_PRIVATE: skipped \"10.0.0.1/8\""},
		},
		{
			name: "block6", env: map[string]string{"MANADE_BLOCK6": "2001:db8:1::/48 , 2001:db8:2::/48"},
			want: Ceiling{Block6: []netip.Prefix{pfx("2001:db8:1::/48"), pfx("2001:db8:2::/48")}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"MANADE_HOST": "nas"}
			maps.Copy(env, tc.env)
			self, err := loadSelf(t, env)
			if err != nil {
				t.Fatal(err)
			}
			got := self.Ceiling
			warnings := got.Warnings
			got.Warnings = nil
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ceiling = %+v, want %+v", got, tc.want)
			}
			if len(warnings) != len(tc.warnings) {
				t.Fatalf("warnings = %q, want %d", warnings, len(tc.warnings))
			}
			for i, w := range tc.warnings {
				if !strings.Contains(warnings[i], w) {
					t.Errorf("warning %d = %q, want it to hold %q", i, warnings[i], w)
				}
			}
		})
	}
	for _, tc := range []struct{ name, raw, want string }{
		{"block6Malformed", "2001:db8:1::/48,lan", `MANADE_BLOCK6: want an IP address or a prefix such as 10.0.0.0/24, got "lan"`},
		{"block6IPv4", "10.0.0.0/8", `MANADE_BLOCK6: want an IPv6 prefix such as 2001:db8:1::/48, got "10.0.0.0/8"`},
		{"block6Unmasked", "2001:db8:1::1/48", `MANADE_BLOCK6: want an IP address or a prefix`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "MANADE_BLOCK6": tc.raw})
			wantErr(t, err, tc.want)
		})
	}
	t.Run("aBadToggleNamesNoOtherVariable", func(t *testing.T) {
		self, err := loadSelf(t, map[string]string{"MANADE_HOST": "nas", "MANADE_ALLOW_OPEN": "maybe", "MANADE_ALLOW_IPV6": "true"})
		if err != nil || !self.Ceiling.IPv6 || self.Ceiling.Open || !slices.ContainsFunc(self.Ceiling.Warnings, func(w string) bool { return strings.HasPrefix(w, "MANADE_ALLOW_OPEN:") }) {
			t.Errorf("self = %+v, %v", self, err)
		}
	})
}
