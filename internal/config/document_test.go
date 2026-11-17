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
	cfg := mustBuildJSON(t, &Config{}, object(jsonScopes, jsonHost, jsonProfile("")))
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
	t.Run("poolName", func(t *testing.T) {
		if cfg.PoolName != "manade" {
			t.Errorf("pool.name = %q, want manade", cfg.PoolName)
		}
	})
	t.Run("runnerVariable", func(t *testing.T) {
		if cfg.RunnerVariable != "MANADE_RUNNER" {
			t.Errorf("pool.runner_variable = %q, want MANADE_RUNNER", cfg.RunnerVariable)
		}
	})
	t.Run("history", func(t *testing.T) {
		if want := (History{Runs: 90 * 24 * time.Hour, Evidence: 7 * 24 * time.Hour}); cfg.History != want {
			t.Errorf("history = %+v, want %+v", cfg.History, want)
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

func TestParsePoolName(t *testing.T) {
	cfg, err := Parse([]byte(object(jsonScopes, jsonHost, jsonProfile(""), `"pool": {"name": "ci-2"}`)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolName != "ci-2" {
		t.Errorf("pool.name = %q, want ci-2", cfg.PoolName)
	}
}

func TestParseHistorySettings(t *testing.T) {
	cfg, err := Parse([]byte(object(jsonScopes, jsonHost, jsonProfile(""),
		`"history": {"runs": "400d", "evidence": "1d"}`)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (History{Runs: 400 * 24 * time.Hour, Evidence: 24 * time.Hour}); cfg.History != want {
		t.Errorf("history = %+v, want %+v", cfg.History, want)
	}
}

func TestParseRunnerVariable(t *testing.T) {
	cfg, err := Parse([]byte(object(jsonScopes, jsonHost, jsonProfile(""), `"pool": {"runner_variable": "_CI_size2"}`)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerVariable != "_CI_size2" {
		t.Errorf("pool.runner_variable = %q, want _CI_size2", cfg.RunnerVariable)
	}
}

// Parse answers no model with its refusal.
func TestParseRefusal(t *testing.T) {
	cfg, err := Parse([]byte(`{"pool": {"name": ""}, "queue": {"head_reserve_after": "0s"}}`), nil, nil)
	if cfg != nil {
		t.Errorf("Parse = %+v, want no model", cfg)
	}
	wantErr(t, err, "queue.head_reserve_after")
}

// The control plane's document may leave a host's budget unset: a host
// with none builds with a zero budget.
func TestBuildHostWithoutCapacity(t *testing.T) {
	cfg := mustBuildJSON(t, &Config{}, object(jsonScopes, `"hosts": {"default": {}}`, jsonProfile("")))
	if c := host(t, cfg, "default").Capacity; c != (Capacity{}) {
		t.Errorf("capacity = %+v, want none", c)
	}
}

// A document with no scope is a first run's: nothing is served yet.
func TestBuildWithoutScopes(t *testing.T) {
	cfg := mustBuildJSON(t, &Config{}, `{"hosts": {"default": {}}, "profiles": {"default": {}}}`)
	if len(cfg.Scopes) != 0 || profile(t, cfg, "default") == nil {
		t.Errorf("scopes = %+v, want none", cfg.Scopes)
	}
}

func TestBuildHosts(t *testing.T) {
	const d = `{
		"images": {"refresh": "2h"},
		"scopes": [{"repo": "acme/app", "default_profile": "p"}],
		"hosts": {
			"default": {
				"capacity": {"cpus": 8, "memory": "24GiB", "disk": "100GiB", "max_runners": 6},
				"images": {"refresh": "30m"}
			},
			"nas": {
				"capacity": {"cpus": 2, "memory": "3GiB"},
				"dns": ["10.0.0.53", "2001:db8::53"],
				"network": {"subnet": "10.200.0.0/24"},
				"egress": {
					"ipv6": true,
					"block6": ["2001:db8:1::/48"],
					"allow": [
						{"to": "10.0.0.50", "ports": [5432]},
						{"to": "192.168.2.0/24", "ports": ["8000-8100", 9000], "protocol": "both"},
						{"to": "fd00::1", "ports": [53], "protocol": "udp"}
					]
				}
			},
			"lan": {"capacity": {"cpus": 1.5, "memory": "512MiB"}, "egress": {"open": true}}
		},
		"profiles": {"p": {"hosts": ["nas", "default"], "scopes": ["acme/app"], "cpus": 1, "memory": "512MiB"}}
	}`
	cfg := mustBuildJSON(t, &Config{}, d)
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

// onHosts is a document with the hosts members hosts and profile p on the
// hosts names lists.
func onHosts(hosts, names string) string {
	return object(jsonScopes, `"hosts": {`+hosts+`}`,
		`"profiles": {"p": {"hosts": [`+names+`], "scopes": ["acme/app"], "cpus": 1, "memory": "512MiB"}}`)
}

func TestBuildRefusals(t *testing.T) {
	hosted := func(section string) string {
		return onHosts(`"h": {"capacity": {"cpus": 1, "memory": "1GiB"}, `+section+`}`, `"h"`)
	}
	withMember := func(member string) string { return object(jsonScopes, jsonHost, jsonProfile(""), member) }
	scoped := func(rows string) string { return object(`"scopes": [`+rows+`]`, jsonHost, jsonProfile("")) }
	profiled := func(body string) string { return object(jsonScopes, jsonHost, jsonProfile(body)) }
	capacity := func(c string) string { return onHosts(`"h": {"capacity": {`+c+`}}`, `"h"`) }
	tests := []struct {
		name, doc string
		want      []string
	}{
		{"unknownTopLevel", withMember(`"orchestrator": "x"`), []string{"orchestrator: unknown key"}},
		{"poolNameLong", withMember(`"pool": {"name": "a-fifteen-chars"}`), []string{"pool.name: want 1 to 14 lowercase letters"}},
		{"poolNameUpper", withMember(`"pool": {"name": "Manade"}`), []string{"pool.name: want 1 to 14 lowercase letters"}},
		{"poolNameDigitFirst", withMember(`"pool": {"name": "1pool"}`), []string{"pool.name: want 1 to 14 lowercase letters"}},
		{"runnerVariableDigitFirst", withMember(`"pool": {"runner_variable": "2RUNNER"}`), []string{"pool.runner_variable: want letters, digits or underscores"}},
		{"runnerVariableDash", withMember(`"pool": {"runner_variable": "MANADE-RUNNER"}`), []string{"pool.runner_variable: want letters, digits or underscores"}},
		{"runnerVariableGitHub", withMember(`"pool": {"runner_variable": "github_runner"}`), []string{"pool.runner_variable: want letters, digits or underscores"}},
		{"runsBelow", withMember(`"history": {"runs": "6d"}`), []string{"history.runs: want whole days from 7d to 400d"}},
		{"runsAbove", withMember(`"history": {"runs": "401d"}`), []string{"history.runs: want whole days from 7d to 400d"}},
		{"runsHours", withMember(`"history": {"runs": "2160h"}`), []string{"history.runs: want whole days"}},
		{"runsSigned", withMember(`"history": {"runs": "+90d"}`), []string{"history.runs: want whole days"}},
		{"runsBare", withMember(`"history": {"runs": 90}`), []string{"history.runs: want whole days"}},
		{"evidenceBelow", withMember(`"history": {"evidence": "0d"}`), []string{"history.evidence: want whole days from 1d to 30d"}},
		{"evidenceAbove", withMember(`"history": {"evidence": "31d"}`), []string{"history.evidence: want whole days from 1d to 30d"}},
		{"historyUnknown", withMember(`"history": {"blobs": "7d"}`), []string{"history.blobs: unknown key"}},
		{"poolUnknown", withMember(`"pool": {"owner": "x"}`), []string{"pool.owner: unknown key"}},
		{"topLevelCapacity", withMember(`"capacity": {"cpus": 1, "memory": "1GiB"}`), []string{"capacity: unknown key"}},
		{"address", hosted(`"address": "https://nas:7464"`), []string{"hosts.h.address: refused: hosts dial the control plane"}},
		{"registryAuth", hosted(`"registry_auth": "/auth.json"`), []string{"hosts.h.registry_auth: refused: set MANADE_REGISTRY_AUTH or MANADE_REGISTRY_AUTH_FILE"}},
		{"ipv6Echo", hosted(`"egress": {"ipv6": true, "ipv6_echo": "https://echo.example"}`), []string{"hosts.h.egress.ipv6_echo: refused: set MANADE_IPV6_ECHO"}},
		{"scopeNoTarget", scoped(`{"runner_group": "g", "default_profile": "p"}`), []string{"scopes[0]: set exactly one of repo, org or enterprise"}},
		{"scopeTwoTargets", scoped(`{"repo": "acme/app", "org": "acme", "default_profile": "p"}`), []string{"scopes[0]: set exactly one"}},
		{"scopeRepoShape", scoped(`{"repo": "acme", "default_profile": "p"}`), []string{"scopes[0].repo: want owner/name"}},
		{"scopeOrgShape", scoped(`{"org": "-acme", "default_profile": "p"}`), []string{"scopes[0].org: want an organization name"}},
		{"scopeEnterpriseShape", scoped(`{"enterprise": "a_b", "default_profile": "p"}`), []string{"scopes[0].enterprise: want an enterprise slug"}},
		{"scopeRepeated", scoped(`{"repo": "acme/app", "default_profile": "p"}, {"repo": "ACME/app", "default_profile": "p"}`), []string{`scopes[1]: repeats scope "ACME/app"`}},
		{"scopeInstallation", scoped(`{"repo": "acme/app", "default_profile": "p", "installation_id": 0}`), []string{"scopes[0].installation_id: want a whole number from 1"}},
		{"scopeState", scoped(`{"repo": "acme/app", "default_profile": "p", "state": "paused"}`), []string{"scopes[0].state: want one of served, stopping"}},
		{"scopeDefaultName", scoped(`{"repo": "acme/app", "default_profile": "Big"}`), []string{"scopes[0].default_profile: want a name of 1 to 32"}},
		{"topLevelNetwork", withMember(`"network": {"subnet": "10.0.0.0/24"}`), []string{"network: unknown key"}},
		{"profileNetwork", profiled(`"network": "host"`), []string{"profiles.p.network: unknown key"}},
		{"bridgeSubnet", hosted(`"network": {"bridge_subnet": "172.30.1.0/24"}`), []string{"hosts.h.network.bridge_subnet: unknown key"}},
		{"noProfiles", object(jsonScopes, jsonHost), []string{"profiles: define at least one profile"}},
		{"hostMemoryMissing", capacity(`"cpus": 1`), []string{"hosts.h.capacity.memory: required"}},
		{"hostCPUsMissing", capacity(`"memory": "1GiB"`), []string{"hosts.h.capacity.cpus: required"}},
		{"hostMemoryBad", capacity(`"cpus": 1, "memory": "1G"`), []string{"hosts.h.capacity.memory: want a whole number with a unit"}},
		{"hostDiskBad", capacity(`"cpus": 1, "memory": "1GiB", "disk": 0`), []string{"hosts.h.capacity.disk: want more than 0"}},
		{"hostMaxRunnersBad", capacity(`"cpus": 1, "memory": "1GiB", "max_runners": 0`), []string{"hosts.h.capacity.max_runners: want a whole number from 1"}},
		{"dnsEmpty", hosted(`"dns": []`), []string{"hosts.h.dns: list at least one resolver"}},
		{"dnsLoopback", hosted(`"dns": ["127.0.0.1"]`), []string{"hosts.h.dns[0]: want a unicast resolver"}},
		{"dnsLinkLocal", hosted(`"dns": ["1.1.1.1", "fe80::1"]`), []string{"hosts.h.dns[1]: want a unicast resolver"}},
		{"dnsName", hosted(`"dns": ["one.one.one.one"]`), []string{"hosts.h.dns[0]"}},
		{"subnetPublic", hosted(`"network": {"subnet": "8.8.0.0/16"}`), []string{"hosts.h.network.subnet: want a private IPv4 subnet"}},
		{"subnetNarrow", hosted(`"network": {"subnet": "10.0.0.0/29"}`), []string{"hosts.h.network.subnet"}},
		{"subnetUnmasked", hosted(`"network": {"subnet": "10.0.0.1/24"}`), []string{"hosts.h.network.subnet"}},
		{"subnetIPv6", hosted(`"network": {"subnet": "fd00::/64"}`), []string{"hosts.h.network.subnet"}},
		{"topRefreshShort", withMember(`"images": {"refresh": "5m"}`), []string{"images.refresh: want at least 10m"}},
		{"hostRefreshBad", hosted(`"images": {"refresh": "soon"}`), []string{"hosts.h.images.refresh: want a positive duration"}},
		{"internetBad", hosted(`"egress": {"internet": "yes"}`), []string{"hosts.h.egress.internet: want true or false"}},
		{"openBad", hosted(`"egress": {"open": 1}`), []string{"hosts.h.egress.open: want true or false"}},
		{"ipv6Bad", hosted(`"egress": {"ipv6": "on"}`), []string{"hosts.h.egress.ipv6: want true or false"}},
		{"block6IPv4", hosted(`"egress": {"ipv6": true, "block6": ["10.0.0.0/8"]}`), []string{"hosts.h.egress.block6[0]: want an IPv6 prefix"}},
		{"block6Unmasked", hosted(`"egress": {"ipv6": true, "block6": ["2001:db8::1/48"]}`), []string{"hosts.h.egress.block6[0]"}},
		{"allowNoTo", hosted(`"egress": {"allow": [{"ports": [1]}]}`), []string{"hosts.h.egress.allow[0].to: required"}},
		{"allowToName", hosted(`"egress": {"allow": [{"to": "db.lan", "ports": [1]}]}`), []string{"hosts.h.egress.allow[0].to: want an IP address or a prefix"}},
		{"allowNoPorts", hosted(`"egress": {"allow": [{"to": "10.0.0.1"}]}`), []string{"hosts.h.egress.allow[0].ports: required"}},
		{"allowPortZero", hosted(`"egress": {"allow": [{"to": "10.0.0.1", "ports": [0]}]}`), []string{"hosts.h.egress.allow[0].ports[0]: want a port"}},
		{"allowPortHigh", hosted(`"egress": {"allow": [{"to": "10.0.0.1", "ports": [1, 65536]}]}`), []string{"hosts.h.egress.allow[0].ports[1]"}},
		{"allowRangeBackwards", hosted(`"egress": {"allow": [{"to": "10.0.0.1", "ports": ["9-8"]}]}`), []string{"hosts.h.egress.allow[0].ports[0]"}},
		{"allowProtocol", hosted(`"egress": {"allow": [{"to": "10.0.0.1", "ports": [1], "protocol": "icmp"}]}`), []string{"hosts.h.egress.allow[0].protocol: want tcp, udp or both"}},
		{"headReserveBad", withMember(`"queue": {"head_reserve_after": "0s"}`), []string{"queue.head_reserve_after"}},
		{"profileHostsEmpty", profiled(`"hosts": []`), []string{"profiles.p.hosts: list at least one host"}},
		{"profileHostsRepeat", profiled(`"hosts": ["default", "default"]`), []string{"profiles.p.hosts[1]: repeats host"}},
		{"profileHostsBadName", profiled(`"hosts": ["Default"]`), []string{"profiles.p.hosts[0]: want a lowercase DNS label"}},
		{"profileRuntime", profiled(`"runtime": "sysbox-runc"`), []string{"profiles.p.runtime: want one of runc"}},
		{"dockerImageUnprivileged", profiled(`"runner_image": "docker", "allow_privilege_escalation": true, "capabilities": "docker-default", "scopes": ["acme/app"]`), []string{"profiles.p.privileged: runner_image: docker needs privileged: true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantErr(t, build(&Config{}, []byte(tt.doc), nil, nil), tt.want...)
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
		{"ca", true},
		{"orchestrator", true},
	}
	for _, tt := range tests {
		t.Run(strings.NewReplacer("/", "slash", ".", "dot").Replace(tt.name), func(t *testing.T) {
			d := onHosts(`"`+tt.name+`": {"capacity": {"cpus": 1, "memory": "1GiB"}}`, `"default"`)
			err := build(&Config{}, []byte(d), nil, nil)
			refused := err != nil && strings.Contains(err.Error(), "want a lowercase DNS label")
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
			err := build(&Config{}, []byte(tc.doc), nil, nil)
			var fe FieldErrors
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
	if err := build(cfg, []byte(d), nil, nil); err != nil {
		t.Fatal(err)
	}
	if c := host(t, cfg, "default").Capacity; c != (Capacity{MilliCPUs: 1500, Memory: 2 << 30, MaxRunners: 3}) {
		t.Errorf("capacity = %+v", c)
	}
	if a := host(t, cfg, "default").Egress.Allow; len(a) != 1 || !slices.Equal(a[0].Ports, []PortRange{{53, 53}, {8000, 8100}}) {
		t.Errorf("allow = %+v", a)
	}
	if p := profile(t, cfg, "p"); p.MilliCPUs != 500 || p.AllowRoot || p.MaxLifetime != DefaultMaxLifetime {
		t.Errorf("profile = %+v; want a null max_lifetime read as unset", p)
	}
	if cfg.Scopes[0].InstallationID != 42 {
		t.Errorf("installation_id = %d", cfg.Scopes[0].InstallationID)
	}
}

// Every refusal of one document is reported, each at its own path.
func TestBuildReportsEveryPath(t *testing.T) {
	d := object(jsonScopes, `"hosts": {"default": {"capacity": {"cpus": 0}, "dns": ["127.0.0.1"]}}`, jsonProfile(`"memory": "1MiB", "user": "root"`))
	got := paths(t, build(&Config{}, []byte(d), nil, nil))
	want := []string{"hosts.default.capacity.memory", "hosts.default.capacity.cpus", "hosts.default.dns[0]", "profiles.p.user", "profiles.p.memory"}
	if !slices.Equal(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
}

// A key reaches the scope its settings entry named: what the pool reads a
// scale set of a scope the settings dropped under.
func TestScopeOfAKeyIsItsParsedScope(t *testing.T) {
	cfg := mustBuildJSON(t, &Config{}, object(
		`"scopes": [{"repo": "acme/app", "default_profile": "p"}, {"org": "acme", "default_profile": "p"}, {"enterprise": "big", "default_profile": "p"}]`,
		jsonHost, jsonProfile(`"scopes": ["acme/app", "acme", "enterprises/big"]`),
	))
	for i := range cfg.Scopes {
		want := cfg.Scopes[i]
		t.Run(strings.ReplaceAll(want.Key, "/", "_"), func(t *testing.T) {
			got := ScopeOf(want.Key)
			if got.Key != want.Key || got.Kind != want.Kind || got.Owner != want.Owner || got.Repo != want.Repo || got.RunnerGroup != want.RunnerGroup {
				t.Errorf("ScopeOf(%q) = %+v, want %+v", want.Key, got, want)
			}
		})
	}
}
