package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProfileDefaults(t *testing.T) {
	t.Parallel()
	p := profile(t, mustBuild(t, tokenEnv(), withProfile("")), "p")
	if p.RunnerImage != "base" || p.Image != "" {
		t.Errorf("image = %q/%q, want the shipped base", p.RunnerImage, p.Image)
	}
	if !slices.Equal(p.Command, []string{"/home/runner/run.sh"}) {
		t.Errorf("command = %q", p.Command)
	}
	if p.MilliCPUs != 2000 || p.Memory != 4<<30 || p.Swap != 0 || p.PIDs != 4096 {
		t.Errorf("resources = %d mCPU, %d, %d swap, %d pids", p.MilliCPUs, p.Memory, p.Swap, p.PIDs)
	}
	if p.MaxLifetime != 2*time.Hour || p.IdleTimeout != 10*time.Minute {
		t.Errorf("lifetime %v, idle %v", p.MaxLifetime, p.IdleTimeout)
	}
	if p.Docker != "none" || p.Runtime != "runc" {
		t.Errorf("docker %q, runtime %q", p.Docker, p.Runtime)
	}
	if g := p.Grants(); g != nil || p.Scopes != nil || !p.Allowed("acme/web") {
		t.Errorf("grants %q, scopes %q: want none and every scope", g, p.Scopes)
	}
	if p.Disk != 0 || p.ShmSize != 0 || p.MaxRunners != 0 || p.User != "" {
		t.Errorf("optional fields set: %+v", p)
	}
}

func TestProfileValidForms(t *testing.T) {
	t.Parallel()
	cfg := mustBuild(t, tokenEnv(), withProfile(`cpus: 0.5, memory: 6MiB, swap: 1GiB, pids: 1, disk: 20GiB,
    shm_size: 256MiB, max_lifetime: 30m, idle_timeout: 1m, max_runners: 3, user: "1001:1001",
    image: "ghcr.io/acme/runner:2.330.0@sha256:`+hex64+`", command: [/opt/runner/run.sh, --once],
    tmpfs: [{path: /scratch, size: 1GiB}], scopes: [ACME/App, acme/gone],
    capabilities: [NET_RAW, NET_ADMIN, NET_RAW]`))
	p := profile(t, cfg, "p")
	if p.MilliCPUs != 500 || p.Memory != 6<<20 || p.Swap != 1<<30 || p.PIDs != 1 || p.Disk != 20<<30 || p.ShmSize != 256<<20 {
		t.Errorf("resources = %+v", p)
	}
	if p.MaxLifetime != 30*time.Minute || p.IdleTimeout != time.Minute || p.MaxRunners != 3 || p.User != "1001:1001" {
		t.Errorf("limits = %+v", p)
	}
	if !p.Pinned() || p.RunnerImage != "" {
		t.Errorf("image %q: want a pinned operator image", p.Image)
	}
	if !slices.Equal(p.Command, []string{"/opt/runner/run.sh", "--once"}) {
		t.Errorf("command = %q", p.Command)
	}
	if !slices.Equal(p.Tmpfs, []Tmpfs{{Path: "/scratch", Size: 1 << 30}}) {
		t.Errorf("tmpfs = %+v", p.Tmpfs)
	}
	if !p.Allowed("acme/app") || p.Allowed("acme/web") || p.Allowed("acme") {
		t.Errorf("scopes = %q: want acme/app alone, matched without case", p.Scopes)
	}
	if !slices.Equal(p.Capabilities, []string{"NET_ADMIN", "NET_RAW"}) {
		t.Errorf("capabilities = %q, want them sorted once each", p.Capabilities)
	}
}

func TestProfileEmptyScopesServeNothing(t *testing.T) {
	t.Parallel()
	p := profile(t, mustBuild(t, tokenEnv(), withOpenProfile("privileged: true, scopes: []")), "p")
	if p.Scopes == nil || p.Allowed("acme/app") || p.Allowed("acme") {
		t.Errorf("scopes = %#v: want an empty list that allows no scope", p.Scopes)
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestProfileImageFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, runner, image string
	}{
		{"onlyImage", "image: alpine:3.22", "", "alpine:3.22"},
		{"onlyRunnerImage", "runner_image: build", "build", ""},
		{"neither", "", "base", ""},
		{"registryPort", "image: registry.local:5000/team/runner", "", "registry.local:5000/team/runner"},
		{"ipv6Registry", `image: "[fd00::5]:5000/team/runner"`, "", "[fd00::5]:5000/team/runner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := profile(t, mustBuild(t, tokenEnv(), withProfile(tc.body)), "p")
			if p.RunnerImage != tc.runner || p.Image != tc.image {
				t.Errorf("runner_image %q, image %q; want %q, %q", p.RunnerImage, p.Image, tc.runner, tc.image)
			}
			if p.Pinned() {
				t.Error("a tag reports pinned")
			}
		})
	}
	t.Run("both", func(t *testing.T) {
		t.Parallel()
		err := build(tokenEnv(), []byte(withProfile("image: alpine, runner_image: base")))
		wantErr(t, err, "profiles.p.image: set image or runner_image, not both")
	})
}

func TestProfileInvalidForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, want string }{
		{"cpusZero", "cpus: 0", "profiles.p.cpus: want more than 0 CPUs"},
		{"cpusWord", "cpus: two", "profiles.p.cpus: want a CPU count"},
		{"cpusFourDecimals", "cpus: 0.0005", "profiles.p.cpus"},
		{"memoryBare", "memory: 4096", "profiles.p.memory: want a whole number with a unit"},
		{"memoryDockerUnit", "memory: 4g", "profiles.p.memory"},
		{"memoryTiny", "memory: 1MiB", "profiles.p.memory: want at least 6MiB"},
		{"memoryOverflow", "memory: 999999999999999TiB", "profiles.p.memory: size is too large"},
		{"swapNegative", "swap: -1GiB", "profiles.p.swap"},
		{"pidsZero", "pids: 0", "profiles.p.pids: want a whole number from 1 to 4194304"},
		{"pidsHuge", "pids: 4194305", "profiles.p.pids"},
		{"diskZero", "disk: 0", "profiles.p.disk: want more than 0"},
		{"shmWord", "shm_size: big", "profiles.p.shm_size"},
		{"lifetimeBare", "max_lifetime: 30", "profiles.p.max_lifetime: want a positive duration"},
		{"idleNegative", "idle_timeout: -1m", "profiles.p.idle_timeout"},
		{"maxRunnersZero", "max_runners: 0", "profiles.p.max_runners"},
		{"userName", "user: runner", "profiles.p.user: want a numeric non-root uid:gid"},
		{"userUIDOnly", "user: \"1001\"", "profiles.p.user"},
		{"userRootUID", "user: \"0:1001\"", "profiles.p.user"},
		{"userRootGID", "user: \"1001:0\"", "profiles.p.user"},
		{"runnerImageUnknown", "runner_image: ubuntu", "profiles.p.runner_image: want one of base, build, browser, docker"},
		{"imageUppercase", "image: Alpine", "profiles.p.image: want an image reference"},
		{"imageShortDigest", "image: alpine@sha256:abc", "profiles.p.image"},
		{"imageIPv6Unbracketed", `image: "fd00::5:5000/runner"`, "profiles.p.image: want an image reference"},
		{"imageIPv6Unclosed", `image: "[fd00::5:5000/runner"`, "profiles.p.image: want an image reference"},
		{"imageIPv6PortOutOfRange", `image: "[fd00::5]:70000/runner"`, "profiles.p.image: want a registry port from 1 to 65535"},
		{"commandEmpty", "command: []", "profiles.p.command: want the runner's path"},
		{"commandRelative", "command: [run.sh]", "profiles.p.command[0]: want an absolute, clean path"},
		{"commandDotDot", "command: [/home/runner/../run.sh]", "profiles.p.command[0]"},
		{"dockerUnknown", "docker: socket", "profiles.p.docker: want one of none, host-socket"},
		{"runtimeUnknown", "runtime: kata", "profiles.p.runtime: want one of runc"},
		{"privilegedYes", "privileged: yes", "profiles.p.privileged: want true or false"},
		{"allowRootTitle", "allow_root: True", "profiles.p.allow_root: want true or false"},
		{"capUnknown", "capabilities: [NET_ADMIN, CAP_SYS_ADMIN], scopes: [acme/app]", "profiles.p.capabilities[1]: want a Linux capability name"},
		{"capAll", "capabilities: [ALL], scopes: [acme/app]", "profiles.p.capabilities[0]"},
		{"capWord", "capabilities: all, scopes: [acme/app]", "profiles.p.capabilities: want docker-default or a list"},
		{"capNumber", "capabilities: [7], scopes: [acme/app]", "profiles.p.capabilities[0]"},
		{"tmpfsNoSize", "tmpfs: [{path: /scratch}]", "profiles.p.tmpfs[0].size"},
		{"tmpfsRelative", "tmpfs: [{path: scratch, size: 1GiB}]", "profiles.p.tmpfs[0].path"},
		{"unknownKey", "allow_public: true", "allow_public"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := build(tokenEnv(), []byte(withProfile(tc.body)))
			wantErr(t, err, tc.want)
		})
	}
	t.Run("name", func(t *testing.T) {
		t.Parallel()
		err := build(tokenEnv(), []byte(header+"profiles:\n  Large: {}\n"))
		wantErr(t, err, `profiles."Large": want a name of 1 to 32`)
	})
	t.Run("swapOverflow", func(t *testing.T) {
		t.Parallel()
		doc := strings.Replace(withProfile("memory: 8388607TiB, swap: 1TiB"), "memory: 64GiB", "memory: 8388607TiB", 1)
		wantErr(t, build(tokenEnv(), []byte(doc)), "profiles.p.swap: memory plus swap is too large")
	})
}

func TestProfileElevatedNeedsScopes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		grant, body string
		doc         func(string) string
	}{
		{"allow_privilege_escalation", "allow_privilege_escalation: true, privileged: true", withOpenProfile},
		{"capabilities", "capabilities: [NET_ADMIN]", withProfile},
		{"allow_root", "allow_root: true", withProfile},
		{"docker", "docker: host-socket", withOpenProfile},
		{"privileged", "privileged: true", withOpenProfile},
		{"mounts", "mounts: [{volume: cache, target: /cache}]", withProfile},
	} {
		t.Run(tc.grant, func(t *testing.T) {
			t.Parallel()
			err := build(tokenEnv(), []byte(tc.doc(tc.body)))
			wantErr(t, err, "profiles.p.scopes: an elevated profile must list its scopes", tc.grant)
			p := profile(t, mustBuild(t, tokenEnv(), tc.doc(tc.body+", scopes: [acme/app]")), "p")
			if !slices.Contains(p.Grants(), tc.grant) {
				t.Errorf("grants = %q, want %s", p.Grants(), tc.grant)
			}
		})
	}
}

func TestProfilePrivilegeEscalation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, want string }{
		{"noCapabilities", "allow_privilege_escalation: true", "needs capabilities holding SETUID and SETGID"},
		{"setuidOnly", "allow_privilege_escalation: true, capabilities: [SETUID]", "needs capabilities holding SETUID and SETGID"},
		{"both", "allow_privilege_escalation: true, capabilities: [SETGID, SETUID]", ""},
		{"dockerDefault", "allow_privilege_escalation: true, capabilities: docker-default", ""},
		{"privileged", "allow_privilege_escalation: true, privileged: true", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := build(tokenEnv(), []byte(withOpenProfile(tc.body+", scopes: [acme/app]")))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			wantErr(t, err, "profiles.p.allow_privilege_escalation: "+tc.want)
		})
	}
	p := profile(t, mustBuild(t, tokenEnv(), withProfile("capabilities: docker-default, scopes: [acme/app]")), "p")
	if !p.DockerDefaultCaps || len(p.Capabilities) != 14 || !slices.IsSorted(p.Capabilities) {
		t.Errorf("docker-default = %v %q, want Docker's 14 defaults, sorted", p.DockerDefaultCaps, p.Capabilities)
	}
}

func TestProfileShippedDockerImage(t *testing.T) {
	t.Parallel()
	const privileged = "runner_image: docker, allow_privilege_escalation: true, privileged: true, scopes: [acme/app]"
	p := profile(t, mustBuild(t, tokenEnv(), withOpenProfile(privileged)), "p")
	if !slices.Equal(p.Command, []string{"/usr/local/bin/manade-dockerd-run"}) {
		t.Errorf("command = %q, want the wrapper", p.Command)
	}
	for _, tc := range []struct{ name, body, want string }{
		{"noEscalation", "runner_image: docker, privileged: true", "profiles.p.allow_privilege_escalation: runner_image: docker needs"},
		{"unprivileged", "runner_image: docker, allow_privilege_escalation: true, capabilities: docker-default", "profiles.p.privileged: runner_image: docker needs privileged: true"},
		{"hostSocket", "runner_image: docker, allow_privilege_escalation: true, privileged: true, docker: host-socket", "profiles.p.docker: runner_image: docker cannot use host-socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := build(tokenEnv(), []byte(withOpenProfile(tc.body+", scopes: [acme/app]")))
			wantErr(t, err, tc.want)
		})
	}
}

func TestProfileMounts(t *testing.T) {
	t.Parallel()
	ok := withProfile("mounts: [{volume: cache, target: /cache}, {bind: /srv/data, target: /data, read_only: true}], scopes: [acme/app]")
	p := profile(t, mustBuild(t, tokenEnv(), ok), "p")
	want := []Mount{{Source: "cache", Target: "/cache"}, {Source: "/srv/data", Target: "/data", Bind: true, ReadOnly: true}}
	if !slices.Equal(p.Mounts, want) {
		t.Errorf("mounts = %+v", p.Mounts)
	}
	for _, tc := range []struct{ name, mount, want string }{
		{"unclean", "{bind: /srv//data, target: /s}", "mounts[0].bind: want an absolute, clean path"},
		{"relativeBind", "{bind: srv, target: /s}", "mounts[0].bind"},
		{"relativeTarget", "{volume: cache, target: cache}", "mounts[0].target"},
		{"both", "{bind: /srv, volume: v1, target: /s}", "set exactly one of volume or bind"},
		{"neither", "{target: /s}", "set exactly one of volume or bind"},
		{"volumeName", "{volume: /abs, target: /s}", "mounts[0].volume: want a volume name"},
		{"readOnlyWord", "{volume: cache, target: /s, read_only: ro}", "mounts[0].read_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := build(tokenEnv(), []byte(withProfile("mounts: ["+tc.mount+"], scopes: [acme/app]")))
			wantErr(t, err, tc.want)
		})
	}
}
