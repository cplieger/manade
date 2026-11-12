package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/filesafe"
)

func TestFileDefaults(t *testing.T) {
	t.Parallel()
	cfg := mustBuild(t, tokenEnv(), header+"profiles: {default: {}}\n")
	if cfg.Capacity != (Capacity{MilliCPUs: 16000, Memory: 64 << 30}) {
		t.Errorf("capacity = %+v", cfg.Capacity)
	}
	if cfg.HeadReserveAfter != 2*time.Minute || cfg.ImageRefresh != 6*time.Hour {
		t.Errorf("head reserve %v, refresh %v", cfg.HeadReserveAfter, cfg.ImageRefresh)
	}
	if cfg.Network.Subnet != netip.MustParsePrefix("172.30.0.0/24") ||
		cfg.Network.BridgeSubnet != netip.MustParsePrefix("172.30.1.0/24") || cfg.Network.DNS != nil {
		t.Errorf("network = %+v", cfg.Network)
	}
	want := []Scope{
		{Key: "acme/app", Owner: "acme", Repo: "app", Kind: ScopeRepo},
		{Key: "acme/web", Owner: "acme", Repo: "web", Kind: ScopeRepo},
		{Key: "acme", Owner: "acme", RunnerGroup: "Default", Kind: ScopeOrg},
	}
	if !slices.Equal(cfg.Scopes, want) {
		t.Errorf("scopes = %+v", cfg.Scopes)
	}
}

func TestFileValidForms(t *testing.T) {
	t.Parallel()
	cfg := mustBuild(t, appEnv(t), `capacity: {cpus: 19.5, memory: 100GiB, disk: 400GiB, max_runners: 12}
queue: {head_reserve_after: 5m}
images: {refresh: 10m}
network: {subnet: 10.88.0.0/16, bridge_subnet: 192.168.250.0/28, dns: [192.0.2.1, 9.9.9.9]}
scopes:
  - {org: acme, runner_group: CI runners, installation_id: 12345}
profiles: {b: {cpus: 4}, a: {}}
`)
	if cfg.Capacity != (Capacity{MilliCPUs: 19500, Memory: 100 << 30, Disk: 400 << 30, MaxRunners: 12}) {
		t.Errorf("capacity = %+v", cfg.Capacity)
	}
	if cfg.HeadReserveAfter != 5*time.Minute || cfg.ImageRefresh != 10*time.Minute {
		t.Errorf("head reserve %v, refresh %v", cfg.HeadReserveAfter, cfg.ImageRefresh)
	}
	n := cfg.Network
	if n.Subnet.String() != "10.88.0.0/16" || n.BridgeSubnet.String() != "192.168.250.0/28" ||
		!slices.Equal(n.DNS, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("9.9.9.9")}) {
		t.Errorf("network = %+v", n)
	}
	if s := cfg.Scopes[0]; s.RunnerGroup != "CI runners" || s.InstallationID != 12345 {
		t.Errorf("scope = %+v", s)
	}
	if cfg.Profiles[0].Name != "a" || cfg.Profiles[1].Name != "b" {
		t.Errorf("profiles not sorted: %s, %s", cfg.Profiles[0].Name, cfg.Profiles[1].Name)
	}
}

// The RFC 1918 blocks themselves are the widest subnets accepted.
func TestFileSubnetWholeBlocks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ subnet, bridge string }{{"10.0.0.0/8", "172.16.0.0/12"}, {"192.168.0.0/16", "10.0.0.0/28"}} {
		cfg := mustBuild(t, tokenEnv(), header+"network: {subnet: "+c.subnet+", bridge_subnet: "+c.bridge+"}\nprofiles: {default: {}}\n")
		if cfg.Network.Subnet.String() != c.subnet || cfg.Network.BridgeSubnet.String() != c.bridge {
			t.Errorf("network = %+v, want %s and %s", cfg.Network, c.subnet, c.bridge)
		}
	}
}

func TestFileInvalidForms(t *testing.T) {
	t.Parallel()
	const scopes = "scopes: [{repo: acme/app}]\n"
	const profiles = "profiles: {p: {}}\n"
	const capacity = "capacity: {cpus: 4, memory: 8GiB}\n"
	for _, tc := range []struct{ name, doc, want string }{
		{"noCapacity", scopes + profiles, "capacity.cpus: required"},
		{"noMemory", "capacity: {cpus: 4}\n" + scopes + profiles, "capacity.memory: required"},
		{"capacityDiskZero", "capacity: {cpus: 4, memory: 8GiB, disk: 0}\n" + scopes + profiles, "capacity.disk: want more than 0"},
		{"capacityMaxRunners", "capacity: {cpus: 4, memory: 8GiB, max_runners: many}\n" + scopes + profiles, "capacity.max_runners"},
		{"headReserve", capacity + "queue: {head_reserve_after: soon}\n" + scopes + profiles, "queue.head_reserve_after"},
		{"refreshShort", capacity + "images: {refresh: 9m59s}\n" + scopes + profiles, "images.refresh: want at least 10m"},
		{"refreshTypo", capacity + "images: {refresh: 1s}\n" + scopes + profiles, "images.refresh: want at least 10m"},
		{"subnetPublic", capacity + "network: {subnet: 8.8.8.0/24}\n" + scopes + profiles, "network.subnet: want a private IPv4 subnet"},
		{"subnetSpansPublic15", capacity + "network: {subnet: 192.168.0.0/15}\n" + scopes + profiles, "network.subnet: want a private IPv4 subnet"},
		{"subnetSpansPublic7", capacity + "network: {subnet: 10.0.0.0/7}\n" + scopes + profiles, "network.subnet"},
		{"bridgeSpansPublic", capacity + "network: {bridge_subnet: 192.168.0.0/14}\n" + scopes + profiles, "network.bridge_subnet"},
		{"subnetIPv6", capacity + "network: {subnet: \"fd00::/64\"}\n" + scopes + profiles, "network.subnet"},
		{"subnetUnmasked", capacity + "network: {subnet: 172.30.0.1/24}\n" + scopes + profiles, "network.subnet"},
		{"subnetTiny", capacity + "network: {subnet: 172.30.0.0/30}\n" + scopes + profiles, "network.subnet"},
		{"subnetsOverlap", capacity + "network: {subnet: 10.0.0.0/16, bridge_subnet: 10.0.1.0/24}\n" + scopes + profiles, "network.bridge_subnet: overlaps network.subnet"},
		{"dnsName", capacity + "network: {dns: [dns.local]}\n" + scopes + profiles, "network.dns[0]: want a unicast IPv4 resolver"},
		{"dnsLoopback", capacity + "network: {dns: [127.0.0.53]}\n" + scopes + profiles, "network.dns[0]"},
		{"dnsIPv6", capacity + "network: {dns: [\"2606:4700:4700::1111\"]}\n" + scopes + profiles, "network.dns[0]"},
		{"noScopes", capacity + profiles, "scopes: list at least one scope"},
		{"noProfiles", capacity + scopes, "profiles: define at least one profile"},
		{"scopeTwoKinds", capacity + "scopes: [{repo: acme/app, org: acme}]\n" + profiles, "scopes[0]: set exactly one of repo, org or enterprise"},
		{"scopeNone", capacity + "scopes: [{runner_group: Default}]\n" + profiles, "scopes[0]: set exactly one"},
		{"repoNoSlash", capacity + "scopes: [{repo: acme}]\n" + profiles, "scopes[0].repo: want owner/name"},
		{"repoTraversal", capacity + "scopes: [{repo: acme/..}]\n" + profiles, "scopes[0].repo"},
		{"repoNested", capacity + "scopes: [{repo: acme/app/x}]\n" + profiles, "scopes[0].repo"},
		{"orgSlash", capacity + "scopes: [{org: acme/x}]\n" + profiles, "scopes[0].org"},
		{"scopeAllThree", capacity + "scopes: [{repo: acme/app, org: acme, enterprise: acme}]\n" + profiles, "scopes[0]: set exactly one"},
		{"scopeRepeat", capacity + "scopes: [{repo: acme/app}, {repo: ACME/App}]\n" + profiles, "scopes[1]: repeats scope"},
		{"installationWord", capacity + "scopes: [{repo: acme/app, installation_id: seven}]\n" + profiles, "scopes[0].installation_id: want a whole number"},
		{"allowPublic", capacity + "scopes: [{repo: acme/app, allow_public: true}]\n" + profiles, "allow_public"},
		{"unknownTopLevel", capacity + scopes + profiles + "pool: x\n", "pool"},
		{"secondDocument", capacity + scopes + profiles + "---\nqueue: {}\n", "document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := build(tokenEnv(), []byte(tc.doc))
			wantErr(t, err, tc.want)
		})
	}
}

func TestFileJoinsEveryProblem(t *testing.T) {
	t.Parallel()
	err := build(tokenEnv(), []byte("scopes: [{repo: x}]\nprofiles: {p: {cpus: 0, memory: 1}}\n"))
	wantErr(t, err, "capacity.cpus", "capacity.memory", "scopes[0].repo", "profiles.p.cpus", "profiles.p.memory")
}

func TestFileInstallationID(t *testing.T) {
	t.Parallel()
	err := build(appEnv(t), []byte(header+"  - {repo: acme/api, installation_id: 0}\nprofiles: {p: {}}\n"))
	wantErr(t, err, "scopes[3].installation_id: want a whole number from 1")
}

func TestEnterpriseScope(t *testing.T) {
	t.Parallel()
	doc := "capacity: {cpus: 4, memory: 8GiB}\nscopes: [{enterprise: bigco}]\nprofiles: {p: {}}\n"
	cfg := mustBuild(t, tokenEnv(), doc)
	if s := cfg.Scopes[0]; s.Key != "enterprises/bigco" || s.Kind != scopeEnterprise || s.RunnerGroup != "Default" {
		t.Errorf("scope = %+v", s)
	}
	mustBuild(t, tokenEnv(), strings.Replace(doc, "profiles: {p: {}}", "profiles: {p: {scopes: [enterprises/bigco]}}", 1))
	err := build(appEnv(t), []byte(doc))
	wantErr(t, err, "scopes[0].enterprise: an enterprise scope needs GITHUB_TOKEN")
}

func TestFileExpandsNoVariable(t *testing.T) {
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "leaked-key-material")
	t.Setenv("RUNNER_GROUP", "expanded")
	cfg := mustBuild(t, tokenEnv(), `capacity: {cpus: 4, memory: 8GiB}
scopes: [{org: acme, runner_group: "${RUNNER_GROUP}"}]
profiles: {p: {command: [/run.sh, "${GITHUB_APP_PRIVATE_KEY}"]}}
`)
	if g := cfg.Scopes[0].RunnerGroup; g != "${RUNNER_GROUP}" {
		t.Errorf("runner_group = %q, want it literal", g)
	}
	if c := cfg.Profiles[0].Command; c[1] != "${GITHUB_APP_PRIVATE_KEY}" {
		t.Errorf("command = %q, want it literal", c)
	}
}

// setEnv sets a minimal valid environment for Load: a token and a config file.
func setEnv(t *testing.T, doc string) string {
	t.Helper()
	keys := []string{
		"GITHUB_APP_CLIENT_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_TOKEN_FILE",
		"GITHUB_PROXY", "DIAG_DIR", "DIAG_RETENTION", "DOCKER_SOCKET", "POOL_NAME", "METRICS_LISTEN_ADDR", "LOG_LEVEL",
	}
	for _, k := range keys {
		t.Setenv(k, "")
		_ = os.Unsetenv(k) // t.Setenv restores it
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
	t.Setenv("GITHUB_TOKEN", "test-token")
	return path
}

const validDoc = "capacity: {cpus: 4, memory: 8GiB}\nscopes: [{repo: acme/app}]\nprofiles: {p: {}}\n"

func TestLoadEnvironmentDefaults(t *testing.T) {
	setEnv(t, validDoc)
	cfg, err := Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolName != "manade" || cfg.DockerSocket != "/var/run/docker.sock" || cfg.DiagDir != "" ||
		cfg.DiagRetention != 168*time.Hour || cfg.MetricsListenAddr != "" || cfg.LogLevel != slog.LevelInfo || cfg.LogLevelInvalid {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.GitHub.Token.Reveal() != "test-token" || cfg.GitHub.AppKey != nil || cfg.GitHub.Proxy != nil {
		t.Errorf("github = %+v", cfg.GitHub)
	}
}

func TestLoadEnvironmentValues(t *testing.T) {
	setEnv(t, validDoc)
	t.Setenv("POOL_NAME", "ci-pool-2")
	t.Setenv("DOCKER_SOCKET", "/run/user/1000/docker.sock")
	t.Setenv("DIAG_DIR", "/diag")
	t.Setenv("DIAG_RETENTION", "72h")
	t.Setenv("METRICS_LISTEN_ADDR", " :9464\n")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("GITHUB_PROXY", "http://user:pw@proxy.example:3128")
	cfg, err := Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolName != "ci-pool-2" || cfg.DockerSocket != "/run/user/1000/docker.sock" || cfg.DiagDir != "/diag" ||
		cfg.DiagRetention != 72*time.Hour || cfg.MetricsListenAddr != ":9464" || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("config = %+v", cfg)
	}
	if p := cfg.GitHub.Proxy; p == nil || p.Host != "proxy.example:3128" || strings.Contains(p.Redacted(), "pw") {
		t.Errorf("proxy = %v", p)
	}
}

func TestLoadEnvironmentInvalid(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"POOL_NAME", "Manade", "POOL_NAME: want 1 to 14"},
		{"POOL_NAME", "manade-pool-two", "POOL_NAME"},
		{"DOCKER_SOCKET", "tcp://127.0.0.1:2375", "DOCKER_SOCKET: want an absolute, clean path"},
		{"DOCKER_SOCKET", "/var/run/../docker.sock", "DOCKER_SOCKET"},
		{"DIAG_RETENTION", "7d", "DIAG_RETENTION"},
		{"DIAG_RETENTION", "-1h", "DIAG_RETENTION: want a positive duration"},
		{"DIAG_RETENTION", "0s", "DIAG_RETENTION: want a positive duration"},
		{"GITHUB_PROXY", "socks5://user:hunter2@proxy:1080", "GITHUB_PROXY: want an http:// or https://"},
		{"GITHUB_PROXY", "http://proxy:3128/path", "GITHUB_PROXY"},
	} {
		t.Run(tc.key+"_"+strings.ReplaceAll(tc.value, "/", "_"), func(t *testing.T) {
			setEnv(t, validDoc)
			t.Setenv(tc.key, tc.value)
			_, err := Load(t.Context())
			wantErr(t, err, tc.want)
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("err = %q echoes the proxy password", err)
			}
		})
	}
}

func TestLoadLogLevelInvalid(t *testing.T) {
	setEnv(t, validDoc)
	t.Setenv("LOG_LEVEL", "verbose")
	cfg, err := Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LogLevelInvalid || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("level %v, invalid %v: want info, flagged", cfg.LogLevel, cfg.LogLevelInvalid)
	}
}

func TestLoadCredential(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, testPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("appFromFile", func(t *testing.T) {
		setEnv(t, validDoc)
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.abc")
		t.Setenv("GITHUB_APP_PRIVATE_KEY_FILE", keyFile)
		cfg, err := Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GitHub.AppKey == nil || cfg.GitHub.Token.Reveal() != "" {
			t.Errorf("github = %+v, want the App key alone", cfg.GitHub)
		}
	})
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"neither", map[string]string{"GITHUB_TOKEN": ""}, "set GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY, or GITHUB_TOKEN"},
		{"both", map[string]string{"GITHUB_APP_CLIENT_ID": "Iv1.abc", "GITHUB_APP_PRIVATE_KEY_FILE": keyFile}, "not both"},
		{"clientIDOnly", map[string]string{"GITHUB_TOKEN": "", "GITHUB_APP_CLIENT_ID": "Iv1.abc"}, "set GITHUB_APP_CLIENT_ID with"},
		{"keyOnly", map[string]string{"GITHUB_TOKEN": "", "GITHUB_APP_PRIVATE_KEY_FILE": keyFile}, "set GITHUB_APP_CLIENT_ID with"},
		{"keyNotPEM", map[string]string{"GITHUB_TOKEN": "", "GITHUB_APP_CLIENT_ID": "Iv1.abc", "GITHUB_APP_PRIVATE_KEY": "not-a-key"}, "GITHUB_APP_PRIVATE_KEY: "},
		{"whitespaceTokenFile", map[string]string{"GITHUB_TOKEN": "", "GITHUB_TOKEN_FILE": " "}, "read secret file for GITHUB_TOKEN"},
		{"missingTokenFile", map[string]string{"GITHUB_TOKEN_FILE": "/nonexistent/token"}, "/nonexistent/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, validDoc)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(t.Context())
			wantErr(t, err, tc.want)
			if err != nil && strings.Contains(err.Error(), "not-a-key") {
				t.Errorf("err = %q echoes the key", err)
			}
		})
	}
}

func TestLoadConfigFile(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		setEnv(t, validDoc)
		t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "absent.yaml"))
		_, err := Load(t.Context())
		wantErr(t, err, "CONFIG_FILE")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("err = %v, want not-exist", err)
		}
	})
	t.Run("tooLarge", func(t *testing.T) {
		path := setEnv(t, "")
		if err := os.WriteFile(path, []byte(validDoc+"#"+strings.Repeat("x", 1<<20)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(t.Context())
		if !errors.Is(err, filesafe.ErrFileTooLarge) {
			t.Errorf("err = %v, want too large", err)
		}
	})
	t.Run("joinsEnvironmentAndFile", func(t *testing.T) {
		setEnv(t, "scopes: [{repo: acme/app}]\nprofiles: {p: {}}\n")
		t.Setenv("POOL_NAME", "Bad")
		_, err := Load(t.Context())
		wantErr(t, err, "POOL_NAME", "capacity.cpus: required")
	})
	t.Run("directory", func(t *testing.T) {
		setEnv(t, validDoc)
		t.Setenv("CONFIG_FILE", t.TempDir())
		_, err := Load(t.Context())
		if !errors.Is(err, filesafe.ErrNotRegular) {
			t.Errorf("err = %v, want not regular", err)
		}
	})
}

func TestSecretRedacts(t *testing.T) {
	t.Parallel()
	s := newSecret("test-token")
	type held struct{ s Secret }
	type nested struct{ h held }
	var logged strings.Builder
	for _, v := range []any{s, &s, held{s}, &held{s}, nested{held{s}}, &nested{held{s}}} {
		slog.New(slog.NewTextHandler(&logged, nil)).Info("v", "v", v)
		slog.New(slog.NewJSONHandler(&logged, nil)).Info("v", "v", v)
	}
	outs := []string{logged.String(), s.LogValue().String()}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		outs = append(outs, fmt.Sprintf(verb, s), fmt.Sprintf(verb, held{s}), fmt.Sprintf(verb, &held{s}),
			fmt.Sprintf(verb, nested{held{s}}), fmt.Sprintf(verb, &nested{held{s}}))
	}
	for _, out := range outs {
		if strings.Contains(out, "test-token") {
			t.Errorf("printed %q", out)
		}
	}
	if s.Reveal() != "test-token" || s.String() != "[redacted]" || (Secret{}).String() != "" || newSecret("") != (Secret{}) {
		t.Error("Reveal or String wrong")
	}
	g := GitHub{Token: s}
	if out := fmt.Sprintf("%+v", g); strings.Contains(out, "test-token") {
		t.Errorf("GitHub printed %q", out)
	}
}

// TestExampleConfigLoads keeps the shipped example valid: the README tells a
// reader to copy it as their first config.
func TestExampleConfigLoads(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustBuild(t, tokenEnv(), string(data))
	if got := len(cfg.Scopes); got != 2 {
		t.Errorf("scopes = %d, want the example's 2", got)
	}
	build := profile(t, cfg, "build")
	if build.RunnerImage != "build" || build.Allowed("your-account/another-repo") || !build.Allowed("your-account/your-repo") {
		t.Errorf("build profile = %+v, want the build image on your-account/your-repo only", build)
	}
	if g := profile(t, cfg, "default").Grants(); g != nil {
		t.Errorf("default profile grants %v, want none", g)
	}
}
