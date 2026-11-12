package jobspec

import (
	"encoding/json"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/docker"
)

func TestRunnerDefaultProfile(t *testing.T) {
	h, p := profileOf(t, "")
	req := h.Runner(p, &testRunner)
	hc := req.HostConfig

	if req.Image != "sha256:feed" || req.User != "" {
		t.Errorf("Image, User = %q, %q, want the checked ID and the image's own user", req.Image, req.User)
	}
	if !slices.Equal(req.Entrypoint, []string{"/home/runner/run.sh"}) || req.Cmd != nil {
		t.Errorf("Entrypoint, Cmd = %q, %q, want [/home/runner/run.sh] and none", req.Entrypoint, req.Cmd)
	}
	if !slices.Equal(req.Env, []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=test-jit"}) {
		t.Errorf("Env = %q, want only the JIT config", req.Env)
	}
	wantLabels := map[string]string{
		"manade.scope": "acme/app", "manade.profile": "p", "manade.runner": "p-abc123", "manade.runner-id": "42",
	}
	if !maps.Equal(req.Labels, wantLabels) {
		t.Errorf("Labels = %v, want %v", req.Labels, wantLabels)
	}
	if !slices.Equal(hc.CapDrop, []string{"ALL"}) || !slices.Equal(hc.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Errorf("CapDrop, SecurityOpt = %q, %q", hc.CapDrop, hc.SecurityOpt)
	}
	if hc.Memory != 4<<30 || hc.MemorySwap != 4<<30 || hc.NanoCPUs != 2e9 || hc.PidsLimit == nil || *hc.PidsLimit != 4096 {
		t.Errorf("limits = memory %d, swap %d, cpus %d, pids %v", hc.Memory, hc.MemorySwap, hc.NanoCPUs, hc.PidsLimit)
	}
	if hc.Init == nil || !*hc.Init || hc.Runtime != "runc" || hc.NetworkMode != "manade-isolated" {
		t.Errorf("Init, Runtime, NetworkMode = %v, %q, %q", hc.Init, hc.Runtime, hc.NetworkMode)
	}
	if hc.LogConfig.Type != "json-file" || !maps.Equal(hc.LogConfig.Config, map[string]string{"max-size": "10m", "max-file": "3"}) {
		t.Errorf("LogConfig = %+v", hc.LogConfig)
	}

	// The wire form is what Docker reads: no key that grants access may
	// appear in it at all.
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct{ HostConfig map[string]json.RawMessage }
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"Binds", "Mounts", "Devices", "CapAdd", "Privileged", "GroupAdd", "StorageOpt", "Tmpfs", "Dns", "ShmSize"} {
		if v, ok := wire.HostConfig[k]; ok {
			t.Errorf("HostConfig.%s = %s, want it absent", k, v)
		}
	}
}

func TestRunnerDockerImageUsesWrapper(t *testing.T) {
	h, p := profileOf(t, "runner_image: docker, allow_privilege_escalation: true, privileged: true, scopes: [acme/app]")
	req := h.Runner(p, &testRunner)
	if !slices.Equal(req.Entrypoint, []string{"/usr/local/bin/manade-dockerd-run"}) || req.Cmd != nil {
		t.Errorf("Entrypoint, Cmd = %q, %q, want the wrapper alone", req.Entrypoint, req.Cmd)
	}
}

// changed names the settings two requests differ in: HostConfig.<field> or
// a top-level field.
func changed(t *testing.T, a, b *docker.CreateRequest) []string {
	t.Helper()
	fa, fb := flatten(t, a), flatten(t, b)
	set := map[string]bool{}
	for _, m := range []map[string]string{fa, fb} {
		for k := range m {
			if fa[k] == fb[k] {
				continue
			}
			parts := strings.SplitN(k, ".", 3)
			if parts[0] == "HostConfig" && len(parts) > 1 {
				set[parts[0]+"."+parts[1]] = true
			} else {
				set[parts[0]] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

func TestRunnerEachFieldChangesOnlyItsSetting(t *testing.T) {
	const sudoCaps = "capabilities: [SETGID, SETUID], scopes: [acme/app]"
	tests := []struct {
		name, base, field string
		want              []string
	}{
		{"allowPrivilegeEscalation", sudoCaps, "allow_privilege_escalation: true", []string{"HostConfig.SecurityOpt"}},
		{"capabilities", "", "capabilities: [NET_ADMIN, NET_RAW], scopes: [acme/app]", []string{"HostConfig.CapAdd"}},
		{"capabilitiesDockerDefault", "", "capabilities: docker-default, scopes: [acme/app]", []string{"HostConfig.CapAdd"}},
		{"allowRootChangesNoSetting", "", "allow_root: true, scopes: [acme/app]", nil},
		{"networkBridge", "", "network: bridge, scopes: [acme/app]", []string{"HostConfig.NetworkMode"}},
		{"networkHost", "", "network: host, scopes: [acme/app]", []string{"HostConfig.NetworkMode"}},
		{"dockerHostSocket", "", "docker: host-socket, scopes: [acme/app]", []string{"HostConfig.GroupAdd", "HostConfig.Mounts"}},
		{"runtimeRunsc", "", "runtime: runsc, scopes: [acme/app]", []string{"HostConfig.Runtime"}},
		{"runtimeSysbox", "", "runtime: sysbox-runc, scopes: [acme/app]", []string{"HostConfig.Runtime"}},
		{"privileged", "", "privileged: true, scopes: [acme/app]", []string{"HostConfig.Privileged"}},
		{"mountVolume", "", "mounts: [{volume: cache, target: /cache}], scopes: [acme/app]", []string{"HostConfig.Mounts"}},
		{"user", "", "user: '1001:1001'", []string{"User"}},
		{"command", "", "command: [/opt/runner/run.sh, --once]", []string{"Entrypoint"}},
		{"cpus", "", "cpus: 0.5", []string{"HostConfig.NanoCpus"}},
		{"memory", "", "memory: 8GiB", []string{"HostConfig.Memory", "HostConfig.MemorySwap"}},
		{"swap", "", "swap: 1GiB", []string{"HostConfig.MemorySwap"}},
		{"pids", "", "pids: 512", []string{"HostConfig.PidsLimit"}},
		{"disk", "", "disk: 10GiB", []string{"HostConfig.StorageOpt"}},
		{"shmSize", "", "shm_size: 1GiB", []string{"HostConfig.ShmSize"}},
		{"tmpfs", "", "tmpfs: [{path: /tmp, size: 1GiB}]", []string{"HostConfig.Tmpfs"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bh, base := profileOf(t, tt.base)
			sep := ""
			if tt.base != "" {
				sep = ", "
			}
			h, p := profileOf(t, tt.base+sep+tt.field)
			if got := changed(t, bh.Runner(base, &testRunner), h.Runner(p, &testRunner)); !slices.Equal(got, tt.want) {
				t.Errorf("changed settings = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunnerFieldValues(t *testing.T) {
	t.Run("hostSocket", func(t *testing.T) {
		h, p := profileOf(t, "docker: host-socket, scopes: [acme/app]")
		hc := h.Runner(p, &testRunner).HostConfig
		want := []docker.Mount{{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}}
		if !slices.Equal(hc.Mounts, want) || !slices.Equal(hc.GroupAdd, []string{"999"}) {
			t.Errorf("Mounts, GroupAdd = %+v, %q, want the socket bound and group 999 added", hc.Mounts, hc.GroupAdd)
		}
	})
	t.Run("mounts", func(t *testing.T) {
		h, p := profileOf(t, "mounts: [{volume: cache, target: /cache}, {bind: /srv/data, target: /data, read_only: true}], scopes: [acme/app]")
		want := []docker.Mount{
			{Type: "volume", Source: "cache", Target: "/cache"},
			{Type: "bind", Source: "/srv/data", Target: "/data", ReadOnly: true},
		}
		if got := h.Runner(p, &testRunner).HostConfig.Mounts; !slices.Equal(got, want) {
			t.Errorf("Mounts = %+v, want %+v", got, want)
		}
	})
	t.Run("resources", func(t *testing.T) {
		h, p := profileOf(t, "cpus: 1.5, memory: 2GiB, swap: 512MiB, pids: 100, shm_size: 64MiB")
		hc := h.Runner(p, &testRunner).HostConfig
		if hc.NanoCPUs != 1_500_000_000 || hc.Memory != 2<<30 || hc.MemorySwap != 2<<30+512<<20 || *hc.PidsLimit != 100 || hc.ShmSize != 64<<20 {
			t.Errorf("HostConfig = %+v", hc)
		}
	})
	t.Run("diskIsAByteCountDockerReadsBack", func(t *testing.T) {
		h, p := profileOf(t, "disk: 10GiB")
		var ctr docker.Container
		ctr.HostConfig.StorageOpt = h.Runner(p, &testRunner).HostConfig.StorageOpt
		if n, err := ctr.DiskBytes(); err != nil || n != 10<<30 {
			t.Errorf("DiskBytes = %d, %v, want %d", n, err, 10<<30)
		}
	})
	t.Run("tmpfsKeepsExec", func(t *testing.T) {
		h, p := profileOf(t, "tmpfs: [{path: /tmp, size: 1GiB}]")
		want := map[string]string{"/tmp": "size=1073741824,exec"}
		if got := h.Runner(p, &testRunner).HostConfig.Tmpfs; !maps.Equal(got, want) {
			t.Errorf("Tmpfs = %v, want %v", got, want)
		}
	})
	t.Run("requestOwnsItsSlices", func(t *testing.T) {
		h, p := profileOf(t, "capabilities: [NET_ADMIN], scopes: [acme/app]")
		req := h.Runner(p, &testRunner)
		req.Entrypoint[0], req.HostConfig.CapAdd[0] = "x", "x"
		if p.Command[0] != "/home/runner/run.sh" || p.Capabilities[0] != "NET_ADMIN" {
			t.Errorf("profile = %q, %q after editing the request, want it unchanged", p.Command, p.Capabilities)
		}
	})
}

func TestRunnerDNS(t *testing.T) {
	cfg := load(t, `network: {dns: [192.0.2.1, 192.0.2.53]}
profiles:
  iso: {}
  br: {network: bridge, scopes: [acme/app]}
  hst: {network: host, scopes: [acme/app]}
`)
	h := NewHost(cfg, 0)
	for i := range cfg.Profiles {
		p := &cfg.Profiles[i]
		got := h.Runner(p, &testRunner).HostConfig.DNS
		want := []string{"192.0.2.1", "192.0.2.53"}
		if p.Name == "hst" {
			want = nil // Docker refuses Dns beside --network host.
		}
		if !slices.Equal(got, want) {
			t.Errorf("profile %s: Dns = %q, want %q", p.Name, got, want)
		}
	}
}

func TestProbe(t *testing.T) {
	h, p := profileOf(t, "command: [/opt/runner/run.sh], disk: 1GiB, cpus: 4, capabilities: [NET_ADMIN], scopes: [acme/app]")
	tests := []struct {
		want map[string]string
		kind ProbeKind
	}{
		{kind: ProbeUser, want: map[string]string{
			"Image": `"sha256:feed"`, "Entrypoint.0": `"/opt/runner/run.sh"`, "Labels.manade.probe": `"user"`,
			"HostConfig.NetworkMode": `"none"`,
		}},
		{kind: ProbeDisk, want: map[string]string{
			"Image": `"sha256:feed"`, "Entrypoint.0": `"/opt/runner/run.sh"`, "Labels.manade.probe": `"disk"`,
			"HostConfig.NetworkMode": `"none"`, "HostConfig.StorageOpt.size": `"1073741824"`,
		}},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := flatten(t, h.Probe(p, tt.kind, "sha256:feed")); !maps.Equal(got, tt.want) {
				t.Errorf("probe = %v, want exactly %v", got, tt.want)
			}
		})
	}
}

func TestNetworks(t *testing.T) {
	isolated := docker.NetworkSpec{Name: "manade-isolated", Bridge: "manade0", Subnet: netip.MustParsePrefix("172.30.0.0/24")}
	bridge := docker.NetworkSpec{Name: "manade-bridge", Bridge: "manade1", Subnet: netip.MustParsePrefix("172.30.1.0/24"), ICC: true}
	tests := []struct {
		name, profiles string
		want           []docker.NetworkSpec
	}{
		{"isolatedOnly", "a: {}", []docker.NetworkSpec{isolated}},
		{"both", "a: {}\n  b: {network: bridge, scopes: [acme/app]}", []docker.NetworkSpec{isolated, bridge}},
		{"hostNeedsNone", "a: {network: host, scopes: [acme/app]}", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := load(t, "profiles:\n  "+tt.profiles+"\n")
			h := NewHost(cfg, 0)
			if got := h.Networks(); !slices.Equal(got, tt.want) {
				t.Errorf("Networks = %+v, want %+v", got, tt.want)
			}
			for i := range cfg.Profiles {
				mode := h.Runner(&cfg.Profiles[i], &testRunner).HostConfig.NetworkMode
				if mode != "host" && !slices.ContainsFunc(tt.want, func(n docker.NetworkSpec) bool { return n.Name == mode }) {
					t.Errorf("profile %s: NetworkMode %q names no network Networks returns", cfg.Profiles[i].Name, mode)
				}
			}
		})
	}
}

func TestImageRef(t *testing.T) {
	tests := []struct {
		name, body, want string
		dev              bool
	}{
		{"defaultStable", "", "ghcr.io/cplieger/manade-runner:base", false},
		{"defaultDev", "", "ghcr.io/cplieger/manade-runner:base-dev", true},
		{"shippedBuild", "runner_image: build", "ghcr.io/cplieger/manade-runner:build", false},
		{"ownImageIgnoresChannel", "image: 'example.com/runner:2.330.0'", "example.com/runner:2.330.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, p := profileOf(t, tt.body)
			if got := ImageRef(p, tt.dev); got != tt.want {
				t.Errorf("ImageRef = %q, want %q", got, tt.want)
			}
		})
	}
}
