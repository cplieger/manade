package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv unsets every variable LoadHost reads, so the runner's own
// environment cannot leak into a case.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CONFIG_FILE", "LOG_LEVEL", "TLS_DIR", "MANADE_HOST", "MANADE_API_LISTEN", "MANADE_API_UID",
		"MANADE_IPV6_ECHO", "MANADE_REGISTRY_AUTH", "MANADE_REGISTRY_AUTH_FILE",
		"GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_TOKEN", "GITHUB_TOKEN_FILE",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// touch writes path and its directories, and answers path.
func touch(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// symlink makes link point at target and answers link.
func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// hostDoc is a document whose hosts are nas and default.
var hostDoc = withHosts("  nas: {capacity: {cpus: 2, memory: 3GiB}}\n  default: {capacity: {cpus: 8, memory: 24GiB}}\n", "nas, default")

// loadHost loads a host whose document is y under env.
func loadHost(t *testing.T, y string, env map[string]string) (*Config, error) {
	t.Helper()
	clearEnv(t)
	t.Setenv("CONFIG_FILE", touch(t, filepath.Join(t.TempDir(), "config.json"), string(doc(y))))
	for k, v := range env {
		t.Setenv(k, v)
	}
	return LoadHost(t.Context())
}

func TestLoadHost(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas"})
		if err != nil {
			t.Fatal(err)
		}
		want := Self{
			Name: "nas", Listen: Address{Socket: "/run/manade/hosts/nas/api.sock"}, TLSDir: "/tls", APIUID: 65532,
			IPv6Echo: "https://cloudflare.com/cdn-cgi/trace",
		}
		if *cfg.Self != want {
			t.Errorf("self = %+v, want %+v", *cfg.Self, want)
		}
	})
	t.Run("tcpListen", func(t *testing.T) {
		cfg, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": ":7464", "MANADE_API_UID": "1000", "TLS_DIR": "/run/tls"})
		if err != nil {
			t.Fatal(err)
		}
		if s := cfg.Self; s.Listen != (Address{HostPort: ":7464", Port: 7464}) || s.TLSDir != "/run/tls" || s.APIUID != 1000 {
			t.Errorf("self = %+v", *s)
		}
	})
	t.Run("socketListen", func(t *testing.T) {
		cfg, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas", "MANADE_API_LISTEN": "unix:///run/manade/api.sock"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Self.Listen != (Address{Socket: "/run/manade/api.sock"}) {
			t.Errorf("listen = %+v", cfg.Self.Listen)
		}
	})
	t.Run("ownEcho", func(t *testing.T) {
		cfg, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas", "MANADE_IPV6_ECHO": "https://echo.example/ip"})
		if err != nil || cfg.Self.IPv6Echo != "https://echo.example/ip" {
			t.Errorf("echo = %+v, %v; want the host's own", cfg, err)
		}
	})
	// The host sizes its memory reserve from its own budget, so its own
	// section must set one; another host's section need not.
	t.Run("capacity", func(t *testing.T) {
		d := withHosts("  nas: {}\n  default: {capacity: {cpus: 8, memory: 24GiB}}\n", "nas, default")
		_, err := loadHost(t, d, map[string]string{"MANADE_HOST": "nas"})
		wantErr(t, err, "hosts.nas.capacity: required on the host itself: set cpus and memory")
		if _, err := loadHost(t, d, map[string]string{"MANADE_HOST": "default"}); err != nil {
			t.Errorf("another host's section without capacity: err = %v", err)
		}
		for name, tc := range map[string]struct{ section, want string }{
			"invalid": {"{capacity: {cpus: x, memory: y}}", "hosts.nas.capacity.memory: "},
			"empty":   {"{capacity: {}}", "hosts.nas.capacity.cpus: required"},
		} {
			t.Run(name, func(t *testing.T) {
				d := withHosts("  nas: "+tc.section+"\n  default: {capacity: {cpus: 8, memory: 24GiB}}\n", "nas, default")
				_, err := loadHost(t, d, map[string]string{"MANADE_HOST": "nas"})
				wantErr(t, err, tc.want)
				if err != nil && strings.Contains(err.Error(), "required on the host itself") {
					t.Errorf("err = %q calls a set capacity missing", err)
				}
			})
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
			_, err := loadHost(t, hostDoc, tt.env)
			wantErr(t, err, tt.want...)
			if err != nil && strings.Contains(err.Error(), "test-token") {
				t.Errorf("err = %q quotes the credential", err)
			}
		})
	}
}

func TestLoadHostRegistryAuth(t *testing.T) {
	t.Run("value", func(t *testing.T) {
		const secret = `{"auths": {"r.example": {"auth": "s3cret"}}}`
		cfg, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas", "MANADE_REGISTRY_AUTH": secret})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.Self.RegistryAuth; a.File != "" || a.Value.Reveal() != secret {
			t.Errorf("registry auth = %+v, want the value", a)
		}
	})
	t.Run("fileKeepsItsPath", func(t *testing.T) {
		cfg, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas", "MANADE_REGISTRY_AUTH_FILE": "/run/secrets/registry"})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.Self.RegistryAuth; a.File != "/run/secrets/registry" || a.Value.Reveal() != "" {
			t.Errorf("registry auth = %+v, want the file's path", a)
		}
	})
	t.Run("none", func(t *testing.T) {
		cfg, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas"})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.Self.RegistryAuth; a.File != "" || a.Value.Reveal() != "" {
			t.Errorf("registry auth = %+v, want none", a)
		}
	})
}

func TestLoadRelativeConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	clearEnv(t)
	d := oneHost("", ", mounts: [{bind: "+dir+"/data, target: /m}]")
	touch(t, filepath.Join(dir, "config.json"), string(doc(d)))
	t.Setenv("CONFIG_FILE", "config.json")
	t.Setenv("MANADE_HOST", "h")
	t.Setenv("TLS_DIR", dir+"/tls")
	t.Setenv("MANADE_REGISTRY_AUTH_FILE", dir+"/auth.json")
	_, err := LoadHost(t.Context())
	const under = "lies at or under the config file's directory"
	wantLine(t, err, "TLS_DIR", under)
	wantLine(t, err, "MANADE_REGISTRY_AUTH_FILE", under, "give the file a mount of its own")
	wantLine(t, err, "profiles.p.mounts[0].bind", " reaches "+dir+`, which host "h" protects`)
}

// A symlink into the config file's directory is refused like a path under
// it; one that does not exist yet is compared as written.
func TestLoadSymlinkIntoConfigDir(t *testing.T) {
	const under = "lies at or under the config file's directory"
	host := func(t *testing.T) string {
		t.Helper()
		clearEnv(t)
		dir := t.TempDir()
		t.Setenv("CONFIG_FILE", touch(t, dir+"/config/config.json", string(doc(oneHost("", "")))))
		t.Setenv("MANADE_HOST", "h")
		return dir
	}
	t.Run("tlsDir", func(t *testing.T) {
		dir := host(t)
		touch(t, dir+"/config/tls/h.pem", "")
		t.Setenv("TLS_DIR", symlink(t, dir+"/config/tls", dir+"/tls"))
		_, err := LoadHost(t.Context())
		wantLine(t, err, "TLS_DIR", under)
	})
	t.Run("registryAuth", func(t *testing.T) {
		dir := host(t)
		t.Setenv("TLS_DIR", dir+"/tls")
		t.Setenv("MANADE_REGISTRY_AUTH_FILE", symlink(t, touch(t, dir+"/config/auth.json", "{}"), dir+"/auth.json"))
		_, err := LoadHost(t.Context())
		wantLine(t, err, "MANADE_REGISTRY_AUTH_FILE", under)
	})
	t.Run("configDir", func(t *testing.T) {
		dir := host(t)
		t.Setenv("CONFIG_FILE", symlink(t, dir+"/config", dir+"/link")+"/config.json")
		if err := os.Mkdir(dir+"/config/tls", 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TLS_DIR", dir+"/config/tls")
		_, err := LoadHost(t.Context())
		wantLine(t, err, "TLS_DIR", under)
	})
	t.Run("absent", func(t *testing.T) {
		dir := host(t)
		t.Setenv("TLS_DIR", dir+"/tls")
		if _, err := LoadHost(t.Context()); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLoadFileBounds(t *testing.T) {
	t.Run("overOneMiB", func(t *testing.T) {
		_, err := loadHost(t, hostDoc, map[string]string{"MANADE_HOST": "nas"})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONFIG_FILE", touch(t, filepath.Join(t.TempDir(), "big.json"), `{"x": "`+strings.Repeat("x", 1<<20)+`"}`))
		_, err = LoadHost(t.Context())
		wantErr(t, err, "CONFIG_FILE")
	})
	t.Run("missing", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MANADE_HOST", "nas")
		t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "absent.json"))
		_, err := LoadHost(t.Context())
		wantErr(t, err, "CONFIG_FILE")
	})
}
