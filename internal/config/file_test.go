package config

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildDefaults(t *testing.T) {
	cfg := mustBuild(t, tokenEnv(), withProfile(""))
	if len(cfg.Hosts) != 1 {
		t.Fatalf("hosts = %d, want the one default host", len(cfg.Hosts))
	}
	h := cfg.Hosts[0]
	t.Run("oneDefaultHostFromTopLevelCapacity", func(t *testing.T) {
		if h.Name != "default" {
			t.Errorf("name = %q, want default", h.Name)
		}
		if h.Capacity != (Capacity{MilliCPUs: 16000, Memory: 64 << 30}) {
			t.Errorf("capacity = %+v", h.Capacity)
		}
	})
	t.Run("addressIsTheSocket", func(t *testing.T) {
		if h.Address != (Address{Socket: "/run/manade/hosts/default/api.sock"}) {
			t.Errorf("address = %+v", h.Address)
		}
	})
	t.Run("dns", func(t *testing.T) {
		want := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}
		if !slices.Equal(h.DNS, want) {
			t.Errorf("dns = %v, want %v", h.DNS, want)
		}
	})
	t.Run("egress", func(t *testing.T) {
		e := h.Egress
		if !e.Internet || e.Open || e.IPv6 || e.IPv6Echo != "" || e.Allow != nil || e.Block6 != nil {
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
	t.Run("registryAuth", func(t *testing.T) {
		if h.RegistryAuth != "" {
			t.Errorf("registry_auth = %q, want none", h.RegistryAuth)
		}
	})
	t.Run("profileHosts", func(t *testing.T) {
		if got := profile(t, cfg, "p").Hosts; !slices.Equal(got, []string{"default"}) {
			t.Errorf("hosts = %v, want [default]", got)
		}
	})
}

func TestBuildHosts(t *testing.T) {
	doc := withHosts(`  default:
    capacity: {cpus: 8, memory: 24GiB, disk: 100GiB, max_runners: 6}
    images: {refresh: 30m}
  nas:
    address: https://nas.example:7464
    capacity: {cpus: 2, memory: 3GiB}
    dns: [10.0.0.53, "2001:db8::53"]
    network: {subnet: 10.200.0.0/24}
    registry_auth: /auth/config.json
    egress:
      ipv6: true
      block6: ["2001:db8:1::/48"]
      allow:
        - {to: 10.0.0.50, ports: [5432]}
        - {to: 192.168.2.0/24, ports: ["8000-8100", 9000], protocol: both}
        - {to: "fd00::1", ports: [53], protocol: udp}
  lan:
    address: https://10.0.0.9
    capacity: {cpus: 1.5, memory: 512MiB}
    egress: {open: true}
`, "nas, default")
	cfg := mustBuild(t, tokenEnv(), "images: {refresh: 2h}\n"+doc)
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
		if h.Address != (Address{HostPort: "nas.example:7464"}) {
			t.Errorf("address = %+v", h.Address)
		}
		if h.ImageRefresh != 2*time.Hour {
			t.Errorf("images.refresh = %v, want the top level's 2h", h.ImageRefresh)
		}
		if want := []netip.Addr{netip.MustParseAddr("10.0.0.53"), netip.MustParseAddr("2001:db8::53")}; !slices.Equal(h.DNS, want) {
			t.Errorf("dns = %v", h.DNS)
		}
		if h.Subnet != netip.MustParsePrefix("10.200.0.0/24") {
			t.Errorf("subnet = %v", h.Subnet)
		}
		if h.RegistryAuth != "/auth/config.json" {
			t.Errorf("registry_auth = %q", h.RegistryAuth)
		}
	})
	t.Run("nasEgress", func(t *testing.T) {
		e := host(t, cfg, "nas").Egress
		if !e.Internet || !e.IPv6 || e.Open || e.IPv6Echo != "https://cloudflare.com/cdn-cgi/trace" {
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
		if h.Address != (Address{HostPort: "10.0.0.9:7464"}) {
			t.Errorf("address = %+v, want the API's port 7464", h.Address)
		}
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

func TestIPv6EchoOnlyWhereLearned(t *testing.T) {
	tests := []struct {
		name, egress, want string
	}{
		{"ipv6Off", `{ipv6_echo: "https://echo.example/ip"}`, ""},
		{"internetOff", `{ipv6: true, internet: false}`, ""},
		{"default", `{ipv6: true}`, DefaultIPv6Echo},
		{"own", `{ipv6: true, ipv6_echo: "https://echo.example/ip"}`, "https://echo.example/ip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := withHosts("  h:\n    capacity: {cpus: 1, memory: 1GiB}\n    egress: "+tt.egress+"\n", "h")
			cfg := &Config{}
			// The cross-field rules refuse an ipv6_echo without ipv6; the
			// parsed value is what this pins.
			_ = build(cfg, []byte(doc))
			if h, ok := cfg.Host("h"); !ok || h.Egress.IPv6Echo != tt.want {
				t.Errorf("ipv6_echo = %+v, want %q", cfg.Hosts, tt.want)
			}
		})
	}
}

func TestBuildRefusals(t *testing.T) {
	const h = "  h:\n    capacity: {cpus: 1, memory: 1GiB}\n"
	hosted := func(section string) string { return withHosts(h+section, "h") }
	tests := []struct {
		name, doc string
		want      []string
	}{
		{"unknownTopLevel", withProfile("") + "pool: x\n", []string{"pool"}},
		{"secondDocument", withProfile("") + "---\nqueue: {}\n", []string{"document"}},
		{"topLevelNetwork", withProfile("") + "network: {subnet: 10.0.0.0/24}\n", []string{"network"}},
		{"profileNetwork", withProfile("network: host"), []string{"network"}},
		{"bridgeSubnet", hosted("    network: {bridge_subnet: 172.30.1.0/24}\n"), []string{"bridge_subnet"}},
		{"noScopes", "capacity: {cpus: 1, memory: 1GiB}\nprofiles: {p: {}}\n", []string{"scopes: list at least one scope"}},
		{"noProfiles", header, []string{"profiles: define at least one profile"}},
		{"noCapacity", scopes + "profiles: {p: {}}\n", []string{"capacity: required"}},
		{"capacityBesideHosts", "capacity: {cpus: 1, memory: 1GiB}\n" + hosted(""), []string{"capacity: set each host's budget under hosts.<name>.capacity"}},
		{"emptyHosts", scopes + "hosts: {}\nprofiles: {p: {}}\n", []string{"hosts: define at least one host"}},
		{"hostCapacityMissing", withHosts("  h: {}\n", "h"), []string{"hosts.h.capacity: required"}},
		{"hostCPUsMissing", withHosts("  h: {capacity: {memory: 1GiB}}\n", "h"), []string{"hosts.h.capacity.cpus: required"}},
		{"hostMemoryBad", withHosts("  h: {capacity: {cpus: 1, memory: 1G}}\n", "h"), []string{"hosts.h.capacity.memory: want a whole number with a unit"}},
		{"hostDiskBad", withHosts("  h: {capacity: {cpus: 1, memory: 1GiB, disk: 0}}\n", "h"), []string{"hosts.h.capacity.disk: want more than 0"}},
		{"hostMaxRunnersBad", withHosts("  h: {capacity: {cpus: 1, memory: 1GiB, max_runners: 0}}\n", "h"), []string{"hosts.h.capacity.max_runners: want a whole number from 1"}},
		{"topLevelCPUsBad", scopes + "capacity: {cpus: -1, memory: 1GiB}\nprofiles: {p: {}}\n", []string{"capacity.cpus: want a CPU count"}},
		{"addressScheme", hosted("    address: http://nas:7464\n"), []string{"hosts.h.address: want unix:///path/api.sock or https://host:port"}},
		{"addressUserinfo", hosted("    address: https://u:p@nas:7464\n"), []string{"hosts.h.address"}},
		{"addressPath", hosted("    address: https://nas:7464/v1\n"), []string{"hosts.h.address"}},
		{"addressPort", hosted("    address: https://nas:70000\n"), []string{"hosts.h.address: want host:port"}},
		{"addressSocketRelative", hosted("    address: unix://run/api.sock\n"), []string{"hosts.h.address: want an absolute, clean path"}},
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
		{"registryAuthRelative", hosted("    registry_auth: auth.json\n"), []string{"hosts.h.registry_auth: want an absolute, clean path"}},
		{"internetBad", hosted("    egress: {internet: yes}\n"), []string{"hosts.h.egress.internet: want true or false"}},
		{"openBad", hosted("    egress: {open: 1}\n"), []string{"hosts.h.egress.open: want true or false"}},
		{"ipv6Bad", hosted("    egress: {ipv6: on}\n"), []string{"hosts.h.egress.ipv6: want true or false"}},
		{"echoNotHTTPS", hosted("    egress: {ipv6: true, ipv6_echo: \"http://echo.example\"}\n"), []string{"hosts.h.egress.ipv6_echo: want an https:// URL"}},
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
			wantErr(t, build(tokenEnv(), []byte(tt.doc)), tt.want...)
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
	}
	for _, tt := range tests {
		t.Run(strings.NewReplacer("/", "slash", ".", "dot").Replace(tt.name), func(t *testing.T) {
			doc := withHosts("  \""+tt.name+"\": {capacity: {cpus: 1, memory: 1GiB}}\n", "default")
			err := build(tokenEnv(), []byte(doc))
			refused := err != nil && strings.Contains(err.Error(), "want a lowercase DNS label")
			if refused == tt.ok {
				t.Errorf("host %q: err = %v, want accepted %v", tt.name, err, tt.ok)
			}
		})
	}
}

func TestNoVariableExpansion(t *testing.T) {
	t.Setenv("RUNNER_GROUP", "expanded")
	doc := header + `profiles: {p: {command: [/run.sh, "${GITHUB_TOKEN}"]}}` + "\n"
	doc = strings.Replace(doc, "org: acme", `{org: acme, runner_group: "${RUNNER_GROUP}"}`, 1)
	cfg := mustBuild(t, tokenEnv(), doc)
	if g := cfg.Scopes[2].RunnerGroup; g != "${RUNNER_GROUP}" {
		t.Errorf("runner_group = %q, want it literal", g)
	}
	if c := profile(t, cfg, "p").Command; c[1] != "${GITHUB_TOKEN}" {
		t.Errorf("command = %q, want it literal", c)
	}
}
