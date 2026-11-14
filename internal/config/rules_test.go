package config

import (
	"strings"
	"testing"
)

// oneHost builds scopes, one host h whose section adds lines (each indented
// four spaces) and one profile p on h whose flow mapping holds body.
func oneHost(lines, body string) string {
	return scopes + "hosts:\n  h:\n    capacity: {cpus: 4, memory: 8GiB}\n" + lines +
		"profiles:\n  p: {hosts: [h], scopes: [acme/app]" + body + "}\n"
}

func TestHostEgressRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, lines string
		want        []string
	}{
		{"openBesideInternet", "    egress: {open: true, internet: true}\n", []string{"hosts.h.egress.internet: refused beside egress.open: true"}},
		{"openBesideInternetOff", "    egress: {open: true, internet: false}\n", []string{"hosts.h.egress.internet: refused beside egress.open: true"}},
		{"openBesideAllow", "    egress: {open: true, allow: [{to: 10.0.0.5, ports: [22]}]}\n", []string{"hosts.h.egress.allow: refused beside egress.open: true"}},
		{"openBesideIPv6", "    egress: {open: true, ipv6: false}\n", []string{"hosts.h.egress.ipv6: refused beside egress.open: true"}},
		{"openBesideBlock6", "    egress: {open: true, block6: [\"2001:db8:1::/48\"]}\n", []string{"hosts.h.egress.block6: refused beside egress.open: true", "hosts.h.egress.block6: needs egress.ipv6: true"}},
		{"block6WithoutIPv6", "    egress: {block6: [\"2001:db8:1::/48\"]}\n", []string{"hosts.h.egress.block6: needs egress.ipv6: true"}},
		{"echoWithoutIPv6", "    egress: {ipv6_echo: \"https://echo.example/ip\"}\n", []string{"hosts.h.egress.ipv6_echo: needs egress.ipv6: true"}},
		{"echoBesideInternetOff", "    egress: {ipv6: true, internet: false, ipv6_echo: \"https://echo.example/ip\"}\n", []string{"hosts.h.egress.ipv6_echo: refused beside egress.internet: false"}},
		{"allowIPv6WithoutIPv6", "    egress: {allow: [{to: \"fd00::5\", ports: [22]}]}\n", []string{"hosts.h.egress.allow[0].to: fd00::5 is IPv6, which needs egress.ipv6: true"}},
		{"allowPublic", "    egress: {allow: [{to: 10.0.0.5, ports: [22]}, {to: 8.8.8.8, ports: [53]}]}\n", []string{"hosts.h.egress.allow[1].to: 8.8.8.8 is not private", "192.168.0.0/16"}},
		{"allowStraddlesPrivate", "    egress: {allow: [{to: 172.0.0.0/8, ports: [22]}]}\n", []string{"hosts.h.egress.allow[0].to: 172.0.0.0/8 is not private"}},
		{"allowIPv6LinkLocal", "    egress: {ipv6: true, allow: [{to: \"fe80::1\", ports: [22]}]}\n", []string{"hosts.h.egress.allow[0].to: fe80::1 lies outside fc00::/7 or 2000::/3"}},
		{"allowIPv4Mapped", "    egress: {ipv6: true, allow: [{to: \"::ffff:10.0.0.5\", ports: [22]}]}\n", []string{"hosts.h.egress.allow[0].to: ::ffff:10.0.0.5 lies outside"}},
		{"subnetContainsResolver", "    dns: [1.1.1.1, 10.20.0.53]\n    network: {subnet: 10.20.0.0/24}\n", []string{"hosts.h.network.subnet: 10.20.0.0/24 contains the resolver 10.20.0.53 of hosts.h.dns"}},
		{"subnetOverlapsAllow", "    network: {subnet: 10.20.5.0/24}\n    egress: {allow: [{to: 10.20.0.0/16, ports: [22]}]}\n", []string{"hosts.h.network.subnet: 10.20.5.0/24 overlaps 10.20.0.0/16 of hosts.h.egress.allow[0]"}},
		{"subnetInsideAllow", "    network: {subnet: 10.20.0.0/16}\n    egress: {allow: [{to: 10.20.5.7, ports: [22]}]}\n", []string{"hosts.h.network.subnet: 10.20.0.0/16 overlaps 10.20.5.7 of hosts.h.egress.allow[0]"}},
		{"dnsIPv6WithoutIPv6", "    dns: [1.1.1.1, \"2606:4700:4700::1111\"]\n", []string{"hosts.h.dns: 2606:4700:4700::1111 is IPv6, which needs egress.ipv6: true"}},
		{"registryAuthUnderConfig", "    registry_auth: /config/registry/auth.json\n", []string{`hosts.h.registry_auth: "/config/registry/auth.json" lies at or under the config file's directory "/config"`, "a mount of its own"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tokenEnv()
			cfg.File = "/config/config.yaml"
			wantErr(t, build(cfg, []byte(oneHost(tc.lines, ""))), tc.want...)
		})
	}
}

func TestHostEgressAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, lines string }{
		{"openAlone", "    egress: {open: true}\n"},
		{"openFalseBesideInternet", "    egress: {open: false, internet: false}\n"},
		{"allowEveryPrivateRange", "    egress: {allow: [{to: 10.0.0.0/8, ports: [1]}, {to: 100.64.0.0/10, ports: [1]}, {to: 169.254.169.254, ports: [80]}, {to: 172.16.0.0/12, ports: [1]}, {to: 192.168.1.0/24, ports: [1]}]}\n"},
		{"allowIPv6", "    egress: {ipv6: true, allow: [{to: \"fd00::/8\", ports: [22]}, {to: \"2001:db8:5::7\", ports: [443]}]}\n"},
		{"echoWithIPv6", "    egress: {ipv6: true, ipv6_echo: \"https://echo.example/ip\"}\n"},
		{"ipv6InternetOffNoEcho", "    egress: {ipv6: true, internet: false}\n"},
		{"block6WithIPv6", "    egress: {ipv6: true, block6: [\"2001:db8:1::/48\"]}\n"},
		{"subnetBesideResolverAndAllow", "    dns: [10.20.1.53]\n    network: {subnet: 10.20.0.0/24}\n    egress: {allow: [{to: 10.20.2.0/24, ports: [22]}]}\n"},
		{"dnsIPv6WithIPv6", "    dns: [\"2606:4700:4700::1111\"]\n    egress: {ipv6: true}\n"},
		{"dnsIPv6OnOpenHost", "    dns: [\"2606:4700:4700::1111\"]\n    egress: {open: true}\n"},
		{"registryAuthOwnMount", "    registry_auth: /registry/auth.json\n"},
		{"registryAuthBesideConfigDir", "    registry_auth: /configs/auth.json\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tokenEnv()
			cfg.File = "/config/config.yaml"
			mustBuild(t, cfg, oneHost(tc.lines, ""))
		})
	}
}

func TestHostTLSDirUnderConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tls string
		refused   bool
	}{
		{"under", "/config/tls", true},
		{"equal", "/config", true},
		{"ownMount", "/tls", false},
		{"siblingPrefix", "/configtls", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{File: "/config/config.yaml", Self: &Self{Name: "h", TLSDir: tc.tls}}
			err := build(cfg, []byte(oneHost("", "")))
			if !tc.refused {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			wantErr(t, err, `TLS_DIR: "`+tc.tls+`" lies at or under the config file's directory "/config"`, "a mount of their own")
		})
	}
}

func TestElevatedHostNeedsScopes(t *testing.T) {
	t.Parallel()
	const doc = scopes + `hosts:
  default: {capacity: {cpus: 4, memory: 8GiB}}
  lan:
    capacity: {cpus: 2, memory: 4GiB}
    egress: {allow: [{to: 10.0.0.50, ports: [5432]}]}
  dev:
    capacity: {cpus: 2, memory: 4GiB}
    egress: {open: true}
profiles:
  plain: {}
  integration: {hosts: [default, lan]}
  build: {hosts: [dev]}
  scoped: {hosts: [lan, dev], scopes: [acme/app]}
`
	err := build(tokenEnv(), []byte(doc))
	wantErr(t, err,
		`profiles.integration.scopes: required: host "lan" has allow entries`,
		`profiles.build.scopes: required: host "dev" is open`)
	for _, ok := range []string{"profiles.plain", "profiles.scoped"} {
		if err != nil && strings.Contains(err.Error(), ok+".") {
			t.Errorf("err = %v, want nothing on %s", err, ok)
		}
	}
}

func TestProfileHostsMustExist(t *testing.T) {
	t.Parallel()
	t.Run("named", func(t *testing.T) {
		t.Parallel()
		doc := scopes + "hosts:\n  h: {capacity: {cpus: 1, memory: 1GiB}}\nprofiles:\n  p: {hosts: [h, nas]}\n"
		wantErr(t, build(tokenEnv(), []byte(doc)), `profiles.p.hosts: names no host "nas"`)
	})
	t.Run("defaultLeftOut", func(t *testing.T) {
		t.Parallel()
		doc := scopes + "hosts:\n  h: {capacity: {cpus: 1, memory: 1GiB}}\nprofiles:\n  p: {}\n"
		wantErr(t, build(tokenEnv(), []byte(doc)), `profiles.p.hosts: names no host "default"`)
	})
}

func TestConfinedHostRefusals(t *testing.T) {
	t.Parallel()
	const remedy = `is refused on host "h", whose egress is not open; give the profile an open host`
	for _, tc := range []struct{ name, body, want string }{
		{"privileged", ", privileged: true", "profiles.p.privileged: privileged: true " + remedy},
		{"hostSocket", ", docker: host-socket", "profiles.p.docker: docker: host-socket " + remedy},
		{"sysAdmin", ", capabilities: [NET_ADMIN, SYS_ADMIN, SYS_PTRACE]", "profiles.p.capabilities: SYS_ADMIN, SYS_PTRACE, outside Docker's default set and NET_ADMIN, " + remedy},
		{"netBroadcast", ", capabilities: [NET_BROADCAST]", "profiles.p.capabilities: NET_BROADCAST,"},
		{"dockerImage", ", runner_image: docker, allow_privilege_escalation: true, privileged: true", "profiles.p.privileged: privileged: true " + remedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wantErr(t, build(tokenEnv(), []byte(oneHost("", tc.body))), tc.want)
		})
	}
	for _, tc := range []struct{ name, body string }{
		{"netAdmin", ", capabilities: [NET_ADMIN, NET_RAW]"},
		{"dockerDefault", ", capabilities: docker-default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mustBuild(t, tokenEnv(), oneHost("", tc.body))
		})
	}
	t.Run("openHostTakesAll", func(t *testing.T) {
		t.Parallel()
		body := ", privileged: true, docker: host-socket, capabilities: [SYS_ADMIN], mounts: [{bind: /var/run/docker.sock, target: /var/run/docker.sock}]"
		mustBuild(t, tokenEnv(), oneHost("    egress: {open: true}\n", body))
	})
	t.Run("eachHostOfTheProfile", func(t *testing.T) {
		t.Parallel()
		doc := scopes + `hosts:
  dev: {capacity: {cpus: 2, memory: 4GiB}, egress: {open: true}}
  h: {capacity: {cpus: 2, memory: 4GiB}}
profiles:
  p: {hosts: [dev, h], scopes: [acme/app], privileged: true}
`
		wantErr(t, build(tokenEnv(), []byte(doc)), "profiles.p.privileged: privileged: true "+remedy)
	})
}

func TestInternetOffRegistry(t *testing.T) {
	t.Parallel()
	const off = "    egress: {internet: false}\n"
	allow := func(entry string) string { return "    egress: {internet: false, allow: [" + entry + "]}\n" }
	const off6 = "    egress: {internet: false, ipv6: true}\n"
	for _, tc := range []struct{ name, lines, image, want string }{
		{"hubShortName", off, "alpine:3.22", `profiles.p.image: "alpine:3.22" is on Docker Hub, which host "h" cannot reach with egress.internet: false`},
		{"hubOrgName", off, "acme/runner:1", `"acme/runner:1" is on Docker Hub`},
		{"hubDockerIO", off, "docker.io/library/alpine", "is on Docker Hub"},
		{"hubIndex", off, "index.docker.io/acme/runner", "is on Docker Hub"},
		{"hubRegistry1", off, "registry-1.docker.io/acme/runner", "is on Docker Hub"},
		{"ipNoAllow", off, "10.0.0.5:5000/runner", `profiles.p.image: "10.0.0.5:5000/runner" is on the registry 10.0.0.5, which no allow entry of host "h" covers on TCP port 5000`},
		{"ipOtherPort", allow("{to: 10.0.0.0/24, ports: [443]}"), "10.0.0.5:5000/runner", "on TCP port 5000"},
		{"ipUDPOnly", allow("{to: 10.0.0.0/24, ports: [5000], protocol: udp}"), "10.0.0.5:5000/runner", "on TCP port 5000"},
		{"ipBelowRange", allow("{to: 10.0.0.0/24, ports: [\"6000-7000\"]}"), "10.0.0.5:5000/runner", "on TCP port 5000"},
		{"ipOtherAddress", allow("{to: 10.0.1.0/24, ports: [443]}"), "10.0.0.5/runner", "on TCP port 443"},
		{"ipv6NoAllow", off6, "[fd00::5]:5000/runner", `profiles.p.image: "[fd00::5]:5000/runner" is on the registry fd00::5, which no allow entry of host "h" covers on TCP port 5000`},
		{"mappedNoAllow", off, "[::ffff:a00:5]:5000/runner", `is on the registry 10.0.0.5, which no allow entry of host "h" covers on TCP port 5000`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wantErr(t, build(tokenEnv(), []byte(oneHost(tc.lines, ", image: \""+tc.image+"\""))), tc.want)
		})
	}
	for _, tc := range []struct{ name, lines, image string }{
		{"ghcr", off, "ghcr.io/acme/runner:1"},
		{"namedRegistry", off, "registry.example/acme/runner:1"},
		{"localhost", off, "localhost:5000/runner"},
		{"ipAllowed", allow("{to: 10.0.0.0/24, ports: [5000]}"), "10.0.0.5:5000/runner"},
		{"ipAllowedRange", allow("{to: 10.0.0.5, ports: [\"400-500\"], protocol: both}"), "10.0.0.5/runner"},
		{"hubWithInternet", "", "alpine:3.22"},
		{"ipv6Allowed", "    egress: {internet: false, ipv6: true, allow: [{to: \"fd00::/64\", ports: [5000]}]}\n", "[fd00::5]:5000/runner"},
		{"mappedAllowed", allow("{to: 10.0.0.0/24, ports: [5000]}"), "[::ffff:a00:5]:5000/runner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mustBuild(t, tokenEnv(), oneHost(tc.lines, ", image: \""+tc.image+"\""))
		})
	}
	t.Run("shippedImage", func(t *testing.T) {
		t.Parallel()
		mustBuild(t, tokenEnv(), oneHost(off, ", runner_image: build"))
	})
	t.Run("registryPortOutOfRange", func(t *testing.T) {
		t.Parallel()
		wantErr(t, build(tokenEnv(), []byte(oneHost("", ", image: \"10.0.0.5:70000/runner\""))), "profiles.p.image: want a registry port from 1 to 65535")
	})
}

// The cases follow distribution/reference's splitDockerDomain.
func TestRegistry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ref, name string
		port      uint16
	}{
		{"alpine", "", 443},
		{"alpine:3.22", "", 443},
		{"library/alpine", "", 443},
		{"docker.io/library/alpine", "", 443},
		{"Docker.IO/acme/x", "", 443},
		{"ghcr.io/acme/runner", "ghcr.io", 443},
		{"localhost/runner", "localhost", 443},
		{"localhost:5000/runner", "localhost", 5000},
		{"registry:5000/runner", "registry", 5000},
		{"Registry/runner", "Registry", 443},
		{"10.0.0.5/runner@sha256:" + hex64, "10.0.0.5", 443},
		{"10.0.0.5:0/runner", "10.0.0.5", 0},
		{"[fd00::5]:5000/runner", "fd00::5", 5000},
		{"[fd00::5]/runner:1", "fd00::5", 443},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			if name, port := Registry(tc.ref); name != tc.name || port != tc.port {
				t.Errorf("Registry(%q) = %q, %d; want %q, %d", tc.ref, name, port, tc.name, tc.port)
			}
		})
	}
}

// The example of the configuration reference parses as written.
func TestConfigExample(t *testing.T) {
	t.Parallel()
	const doc = `scopes:
  - repo: acme/app
  - repo: acme/infra

hosts:
  default:
    capacity: {cpus: 8, memory: 24GiB}
  nas:
    address: https://nas.example:7464
    capacity: {cpus: 2, memory: 3GiB}
  lan:
    capacity: {cpus: 2, memory: 4GiB}
    egress:
      allow:
        - {to: 10.0.0.50, ports: [5432]}

profiles:
  small:
    cpus: 1
    memory: 1536MiB
    hosts: [nas, default]
  medium:
    cpus: 2
    memory: 4GiB
  large:
    cpus: 4
    memory: 8GiB
  integration:
    hosts: [lan]
    scopes: [acme/infra]
`
	cfg := mustBuild(t, tokenEnv(), doc)
	if len(cfg.Hosts) != 3 || len(cfg.Profiles) != 4 {
		t.Errorf("hosts %d, profiles %d; want 3 and 4", len(cfg.Hosts), len(cfg.Profiles))
	}
}
