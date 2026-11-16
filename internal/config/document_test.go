package config

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildDefaults(t *testing.T) {
	cfg := mustBuild(t, &Config{}, withProfile(""))
	h := host(t, cfg, "default")
	t.Run("dns", func(t *testing.T) {
		want := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}
		if !slices.Equal(h.DNS, want) {
			t.Errorf("dns = %v, want %v", h.DNS, want)
		}
	})
	t.Run("egress", func(t *testing.T) {
		e := h.Egress
		if !e.Internet || e.Open || e.IPv6 || e.Allow != nil || e.Block6 != nil {
			t.Errorf("egress = %+v, want internet only", e)
		}
	})
	t.Run("subnetPickedByTheHost", func(t *testing.T) {
		if h.Subnet.IsValid() {
			t.Errorf("subnet = %v, want none", h.Subnet)
		}
	})
	t.Run("refresh", func(t *testing.T) {
		if h.ImageRefresh != time.Hour {
			t.Errorf("images.refresh = %v, want 1h", h.ImageRefresh)
		}
	})
	t.Run("headReserve", func(t *testing.T) {
		if cfg.HeadReserveAfter != 2*time.Minute {
			t.Errorf("head_reserve_after = %v, want 2m", cfg.HeadReserveAfter)
		}
	})
	t.Run("profileHosts", func(t *testing.T) {
		if got := profile(t, cfg, "p").Hosts; !slices.Equal(got, []string{"default"}) {
			t.Errorf("hosts = %v, want [default]", got)
		}
	})
	t.Run("scope", func(t *testing.T) {
		want := Scope{Key: "acme/app", Owner: "acme", Repo: "app", DefaultProfile: "p", Kind: ScopeRepo, path: "scopes[0]"}
		if len(cfg.Scopes) != 1 || cfg.Scopes[0] != want {
			t.Errorf("scopes = %+v, want %+v", cfg.Scopes, want)
		}
	})
}

// A host's budget is the pool's to learn from the host's first report, so a
// host with none set builds with a zero budget.
func TestBuildHostWithoutCapacity(t *testing.T) {
	cfg := mustBuild(t, &Config{}, scopes+"hosts:\n  default: {}\nprofiles:\n  p: {}\n")
	if c := host(t, cfg, "default").Capacity; c != (Capacity{}) {
		t.Errorf("capacity = %+v, want none", c)
	}
}

// A document with no scope is a first run's: nothing is served yet.
func TestBuildWithoutScopes(t *testing.T) {
	cfg := mustBuild(t, &Config{}, "hosts:\n  default: {}\nprofiles:\n  default: {}\n")
	if len(cfg.Scopes) != 0 || profile(t, cfg, "default") == nil {
		t.Errorf("scopes = %+v, want none", cfg.Scopes)
	}
}

func TestBuildHosts(t *testing.T) {
	doc := withHosts(`  default:
    capacity: {cpus: 8, memory: 24GiB, disk: 100GiB, max_runners: 6}
    images: {refresh: 30m}
  nas:
    capacity: {cpus: 2, memory: 3GiB}
    dns: [10.0.0.53, "2001:db8::53"]
    network: {subnet: 10.200.0.0/24}
    egress:
      ipv6: true
      block6: ["2001:db8:1::/48"]
      allow:
        - {to: 10.0.0.50, ports: [5432]}
        - {to: 192.168.2.0/24, ports: ["8000-8100", 9000], protocol: both}
        - {to: "fd00::1", ports: [53], protocol: udp}
  lan:
    capacity: {cpus: 1.5, memory: 512MiB}
    egress: {open: true}
`, "nas, default")
	cfg := mustBuild(t, &Config{}, "images: {refresh: 2h}\n"+doc)
	t.Run("sortedByName", func(t *testing.T) {
		var names []string
		for _, h := range cfg.Hosts {
			names = append(names, h.Name)
		}
		if !slices.Equal(names, []string{"default", "lan", "nas"}) {
			t.Errorf("hosts = %v", names)
		}
	})
	t.Run("default", func(t *testing.T) {
		h := host(t, cfg, "default")
		if h.Capacity != (Capacity{MilliCPUs: 8000, Memory: 24 << 30, Disk: 100 << 30, MaxRunners: 6}) {
			t.Errorf("capacity = %+v", h.Capacity)
		}
		if h.ImageRefresh != 30*time.Minute {
			t.Errorf("images.refresh = %v, want its own 30m", h.ImageRefresh)
		}
	})
	t.Run("nas", func(t *testing.T) {
		h := host(t, cfg, "nas")
		if h.ImageRefresh != 2*time.Hour {
			t.Errorf("images.refresh = %v, want the top level's 2h", h.ImageRefresh)
		}
		if want := []netip.Addr{netip.MustParseAddr("10.0.0.53"), netip.MustParseAddr("2001:db8::53")}; !slices.Equal(h.DNS, want) {
			t.Errorf("dns = %v", h.DNS)
		}
		if h.Subnet != netip.MustParsePrefix("10.200.0.0/24") {
			t.Errorf("subnet = %v", h.Subnet)
		}
	})
	t.Run("nasEgress", func(t *testing.T) {
		e := host(t, cfg, "nas").Egress
		if !e.Internet || !e.IPv6 || e.Open {
			t.Errorf("egress = %+v", e)
		}
		if !slices.Equal(e.Block6, []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/48")}) {
			t.Errorf("block6 = %v", e.Block6)
		}
		want := []Allow{
			{To: netip.MustParsePrefix("10.0.0.50/32"), Ports: []PortRange{{5432, 5432}}, Protocol: ProtocolTCP},
			{To: netip.MustParsePrefix("192.168.2.0/24"), Ports: []PortRange{{8000, 8100}, {9000, 9000}}, Protocol: ProtocolBoth},
			{To: netip.MustParsePrefix("fd00::1/128"), Ports: []PortRange{{53, 53}}, Protocol: ProtocolUDP},
		}
		if !slices.EqualFunc(e.Allow, want, func(a, b Allow) bool {
			return a.To == b.To && a.Protocol == b.Protocol && slices.Equal(a.Ports, b.Ports)
		}) {
			t.Errorf("allow = %+v\nwant %+v", e.Allow, want)
		}
	})
	t.Run("lan", func(t *testing.T) {
		h := host(t, cfg, "lan")
		if h.Capacity.MilliCPUs != 1500 || !h.Egress.Open {
			t.Errorf("lan = %+v", h)
		}
	})
	t.Run("profileHostsInOrder", func(t *testing.T) {
		if got := profile(t, cfg, "p").Hosts; !slices.Equal(got, []string{"nas", "default"}) {
			t.Errorf("hosts = %v", got)
		}
	})
}

func TestBuildRefusals(t *testing.T) {
	const h = "  h:\n    capacity: {cpus: 1, memory: 1GiB}\n"
	hosted := func(section string) string { return withHosts(h+section, "h") }
	tests := []struct {
		name, doc string
		want      []string
	}{
		{"unknownTopLevel", withProfile("") + "pool: x\n", []string{"pool: unknown key"}},
		{"topLevelCapacity", withProfile("") + "capacity: {cpus: 1, memory: 1GiB}\n", []string{"capacity: unknown key"}},
		{"address", hosted("    address: https://nas:7464\n"), []string{"hosts.h.address: refused: hosts dial the control plane"}},
		{"registryAuth", hosted("    registry_auth: /auth.json\n"), []string{"hosts.h.registry_auth: refused: set MANADE_REGISTRY_AUTH or MANADE_REGISTRY_AUTH_FILE"}},
		{"ipv6Echo", hosted("    egress: {ipv6: true, ipv6_echo: \"https://echo.example\"}\n"), []string{"hosts.h.egress.ipv6_echo: refused: set MANADE_IPV6_ECHO"}},
		{"scopeNoTarget", strings.Replace(withProfile(""), "repo: acme/app", "runner_group: g", 1), []string{"scopes[0]: set exactly one of repo, org or enterprise"}},
		{"scopeTwoTargets", strings.Replace(withProfile(""), "repo: acme/app", "repo: acme/app, org: acme", 1), []string{"scopes[0]: set exactly one"}},
		{"scopeRepoShape", strings.Replace(withProfile(""), "acme/app", "acme", 1), []string{"scopes[0].repo: want owner/name"}},
		{"scopeOrgShape", strings.Replace(withProfile(""), "repo: acme/app", "org: -acme", 1), []string{"scopes[0].org: want an organization name"}},
		{"scopeEnterpriseShape", strings.Replace(withProfile(""), "repo: acme/app", "enterprise: a_b", 1), []string{"scopes[0].enterprise: want an enterprise slug"}},
		{"scopeRepeated", strings.Replace(withProfile(""), "  - {repo: acme/app, default_profile: p}\n", "  - {repo: acme/app, default_profile: p}\n  - {repo: ACME/app, default_profile: p}\n", 1), []string{`scopes[1]: repeats scope "ACME/app"`}},
		{"scopeInstallation", strings.Replace(withProfile(""), "default_profile: p", "default_profile: p, installation_id: 0", 1), []string{"scopes[0].installation_id: want a whole number from 1"}},
		{"scopeState", strings.Replace(withProfile(""), "default_profile: p", "default_profile: p, state: paused", 1), []string{"scopes[0].state: want one of served, stopping"}},
		{"scopeDefaultName", strings.Replace(withProfile(""), "default_profile: p", "default_profile: Big", 1), []string{"scopes[0].default_profile: want a name of 1 to 32"}},
		{"topLevelNetwork", withProfile("") + "network: {subnet: 10.0.0.0/24}\n", []string{"network: unknown key"}},
		{"profileNetwork", withProfile("network: host"), []string{"profiles.p.network: unknown key"}},
		{"bridgeSubnet", hosted("    network: {bridge_subnet: 172.30.1.0/24}\n"), []string{"hosts.h.network.bridge_subnet: unknown key"}},
		{"noProfiles", header, []string{"profiles: define at least one profile"}},
		{"hostMemoryMissing", withHosts("  h: {capacity: {cpus: 1}}\n", "h"), []string{"hosts.h.capacity.memory: required"}},
		{"hostCPUsMissing", withHosts("  h: {capacity: {memory: 1GiB}}\n", "h"), []string{"hosts.h.capacity.cpus: required"}},
		{"hostMemoryBad", withHosts("  h: {capacity: {cpus: 1, memory: 1G}}\n", "h"), []string{"hosts.h.capacity.memory: want a whole number with a unit"}},
		{"hostDiskBad", withHosts("  h: {capacity: {cpus: 1, memory: 1GiB, disk: 0}}\n", "h"), []string{"hosts.h.capacity.disk: want more than 0"}},
		{"hostMaxRunnersBad", withHosts("  h: {capacity: {cpus: 1, memory: 1GiB, max_runners: 0}}\n", "h"), []string{"hosts.h.capacity.max_runners: want a whole number from 1"}},
		{"dnsEmpty", hosted("    dns: []\n"), []string{"hosts.h.dns: list at least one resolver"}},
		{"dnsLoopback", hosted("    dns: [127.0.0.1]\n"), []string{"hosts.h.dns[0]: want a unicast resolver"}},
		{"dnsLinkLocal", hosted("    dns: [1.1.1.1, \"fe80::1\"]\n"), []string{"hosts.h.dns[1]: want a unicast resolver"}},
		{"dnsName", hosted("    dns: [one.one.one.one]\n"), []string{"hosts.h.dns[0]"}},
		{"subnetPublic", hosted("    network: {subnet: 8.8.0.0/16}\n"), []string{"hosts.h.network.subnet: want a private IPv4 subnet"}},
		{"subnetNarrow", hosted("    network: {subnet: 10.0.0.0/29}\n"), []string{"hosts.h.network.subnet"}},
		{"subnetUnmasked", hosted("    network: {subnet: 10.0.0.1/24}\n"), []string{"hosts.h.network.subnet"}},
		{"subnetIPv6", hosted("    network: {subnet: \"fd00::/64\"}\n"), []string{"hosts.h.network.subnet"}},
		{"topRefreshShort", "images: {refresh: 5m}\n" + withProfile(""), []string{"images.refresh: want at least 10m"}},
		{"hostRefreshBad", hosted("    images: {refresh: soon}\n"), []string{"hosts.h.images.refresh: want a positive duration"}},
		{"internetBad", hosted("    egress: {internet: yes}\n"), []string{"hosts.h.egress.internet: want true or false"}},
		{"openBad", hosted("    egress: {open: 1}\n"), []string{"hosts.h.egress.open: want true or false"}},
		{"ipv6Bad", hosted("    egress: {ipv6: on}\n"), []string{"hosts.h.egress.ipv6: want true or false"}},
		{"block6IPv4", hosted("    egress: {ipv6: true, block6: [10.0.0.0/8]}\n"), []string{"hosts.h.egress.block6[0]: want an IPv6 prefix"}},
		{"block6Unmasked", hosted("    egress: {ipv6: true, block6: [\"2001:db8::1/48\"]}\n"), []string{"hosts.h.egress.block6[0]"}},
		{"allowNoTo", hosted("    egress: {allow: [{ports: [1]}]}\n"), []string{"hosts.h.egress.allow[0].to: required"}},
		{"allowToName", hosted("    egress: {allow: [{to: db.lan, ports: [1]}]}\n"), []string{"hosts.h.egress.allow[0].to: want an IP address or a prefix"}},
		{"allowNoPorts", hosted("    egress: {allow: [{to: 10.0.0.1}]}\n"), []string{"hosts.h.egress.allow[0].ports: required"}},
		{"allowPortZero", hosted("    egress: {allow: [{to: 10.0.0.1, ports: [0]}]}\n"), []string{"hosts.h.egress.allow[0].ports[0]: want a port"}},
		{"allowPortHigh", hosted("    egress: {allow: [{to: 10.0.0.1, ports: [1, 65536]}]}\n"), []string{"hosts.h.egress.allow[0].ports[1]"}},
		{"allowRangeBackwards", hosted("    egress: {allow: [{to: 10.0.0.1, ports: [\"9-8\"]}]}\n"), []string{"hosts.h.egress.allow[0].ports[0]"}},
		{"allowProtocol", hosted("    egress: {allow: [{to: 10.0.0.1, ports: [1], protocol: icmp}]}\n"), []string{"hosts.h.egress.allow[0].protocol: want tcp, udp or both"}},
		{"headReserveBad", "queue: {head_reserve_after: 0s}\n" + withProfile(""), []string{"queue.head_reserve_after"}},
		{"profileHostsEmpty", header + "profiles: {p: {hosts: []}}\n", []string{"profiles.p.hosts: list at least one host"}},
		{"profileHostsRepeat", header + "profiles: {p: {hosts: [default, default]}}\n", []string{"profiles.p.hosts[1]: repeats host"}},
		{"profileHostsBadName", header + "profiles: {p: {hosts: [Default]}}\n", []string{"profiles.p.hosts[0]: want a lowercase DNS label"}},
		{"profileRuntime", withProfile("runtime: sysbox-runc"), []string{"profiles.p.runtime: want one of runc"}},
		{"dockerImageUnprivileged", withProfile("runner_image: docker, allow_privilege_escalation: true, capabilities: docker-default, scopes: [acme/app]"), []string{"profiles.p.privileged: runner_image: docker needs privileged: true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantErr(t, buildDoc(&Config{}, tt.doc), tt.want...)
		})
	}
}

func TestHostNames(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"default", true},
		{"nas-2", true},
		{"a", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"10.0.0.1", false},
		{"[::1]", false},
		{"a/b", false},
		{"..", false},
		{"Nas", false},
		{"-nas", false},
		{"nas-", false},
		{"nas_2", false},
		{"ca", false},
		{"orchestrator", false},
	}
	for _, tt := range tests {
		t.Run(strings.NewReplacer("/", "slash", ".", "dot").Replace(tt.name), func(t *testing.T) {
			doc := withHosts("  \""+tt.name+"\": {capacity: {cpus: 1, memory: 1GiB}}\n", "default")
			err := buildDoc(&Config{}, doc)
			refused := err != nil && (strings.Contains(err.Error(), "want a lowercase DNS label") || strings.Contains(err.Error(), "want a name other than ca and orchestrator"))
			if refused == tt.ok {
				t.Errorf("host %q: err = %v, want accepted %v", tt.name, err, tt.ok)
			}
		})
	}
}

// A value the document cannot decode is refused at its path: a decode stops
// at its first refusal.
func TestDecodeRefusals(t *testing.T) {
	for _, tc := range []struct{ name, doc, path, want string }{
		{"notObject", `[1]`, "", "want an object"},
		{"syntax", `{"hosts": `, "hosts", "want one JSON object"},
		{"trailing", `{} {}`, "", "want one JSON object"},
		{"duplicateKey", `{"queue": {}, "queue": {}}`, "queue", "want one JSON object"},
		{"unknownHostKey", `{"hosts": {"nas": {"cpus": 2}}}`, "hosts.nas.cpus", "unknown key"},
		{"numericHostName", `{"hosts": {"10": {"x": 1}}}`, "hosts.10.x", "unknown key"},
		{"listIndex", `{"scopes": [{"repo": "a/b"}, {"x": 1}]}`, "scopes[1].x", "unknown key"},
		{"nestedIndex", `{"profiles": {"p": {"tmpfs": [{"path": "/t", "y": 1}]}}}`, "profiles.p.tmpfs[0].y", "unknown key"},
		{"listForObject", `{"hosts": []}`, "hosts", "want an object"},
		{"stringForList", `{"hosts": {"nas": {"dns": "1.1.1.1"}}}`, "hosts.nas.dns", "want a list"},
		{"numberForString", `{"profiles": {"p": {"image": 7}}}`, "profiles.p.image", "want a string"},
		{"objectForScalar", `{"profiles": {"p": {"cpus": {"n": 1}}}}`, "profiles.p.cpus", "want a string, a number, true or false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := build(&Config{}, []byte(tc.doc), nil)
			var fe fieldErrors
			if !errors.As(err, &fe) || len(fe) != 1 || fe[0].Path != tc.path || !strings.Contains(fe[0].Message, tc.want) {
				t.Errorf("err = %#v, want one refusal at %q holding %q", err, tc.path, tc.want)
			}
		})
	}
}

// A scalar is read as written, whichever JSON kind carries it.
func TestDecodeScalars(t *testing.T) {
	const d = `{"scopes": [{"repo": "acme/app", "default_profile": "p", "installation_id": 42}],
		"hosts": {"default": {"capacity": {"cpus": 1.5, "memory": "2GiB", "max_runners": "3"}, "egress": {"ipv6": true, "allow": [{"to": "fd00::1", "ports": [53, "8000-8100"]}]}}},
		"profiles": {"p": {"scopes": ["acme/app"], "cpus": "0.5", "memory": "512MiB", "allow_root": false, "max_lifetime": null}}}`
	cfg := &Config{}
	if err := build(cfg, []byte(d), nil); err != nil {
		t.Fatal(err)
	}
	if c := host(t, cfg, "default").Capacity; c != (Capacity{MilliCPUs: 1500, Memory: 2 << 30, MaxRunners: 3}) {
		t.Errorf("capacity = %+v", c)
	}
	if a := host(t, cfg, "default").Egress.Allow; len(a) != 1 || !slices.Equal(a[0].Ports, []PortRange{{53, 53}, {8000, 8100}}) {
		t.Errorf("allow = %+v", a)
	}
	if p := profile(t, cfg, "p"); p.MilliCPUs != 500 || p.AllowRoot || p.MaxLifetime != defaultMaxLifetime {
		t.Errorf("profile = %+v; want a null max_lifetime read as unset", p)
	}
	if cfg.Scopes[0].InstallationID != 42 {
		t.Errorf("installation_id = %d", cfg.Scopes[0].InstallationID)
	}
}

// Every refusal of one document is reported, each at its own path.
func TestBuildReportsEveryPath(t *testing.T) {
	d := scopes + "hosts:\n  default: {capacity: {cpus: 0}, dns: [127.0.0.1]}\nprofiles:\n  p: {memory: 1MiB, user: root}\n"
	got := paths(t, buildDoc(&Config{}, d))
	want := []string{"hosts.default.capacity.memory", "hosts.default.capacity.cpus", "hosts.default.dns[0]", "profiles.p.user", "profiles.p.memory"}
	if !slices.Equal(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
}
