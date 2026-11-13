package jobspec

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
)

// testHost is host nas of a config file in root/config, its container's one
// mount at root/data. Every host protects /tmp, where t.TempDir puts root,
// so the test drops that one entry; TestNewHostProtects covers the list.
func testHost(t *testing.T, open bool, plat Platform) (h *Host, root string) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"config", "data", "tls"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{File: filepath.Join(root, "config", "config.yaml"), Self: &config.Self{Name: "nas", TLSDir: filepath.Join(root, "tls")}}
	hc := &config.Host{Name: "nas", Egress: config.Egress{Internet: true, Open: open}}
	if plat.Network == "" {
		plat.Network = "manade"
	}
	plat.Mounts = append(plat.Mounts, filepath.Join(root, "data"))
	h = NewHost(cfg, hc, &plat)
	h.protected = slices.DeleteFunc(h.protected, func(p string) bool { return p == "/tmp" })
	return h, root
}

func profile() *config.Profile {
	return &config.Profile{
		Name: "small", RunnerImage: "base", Command: []string{"/home/runner/run.sh"},
		MilliCPUs: 2000, Memory: 4 << 30, PIDs: 4096, Capabilities: []string{"NET_ADMIN"},
	}
}

func runner() *Runner {
	return &Runner{Scope: "acme/app", Name: "manade-small-0123456789ab", ImageID: "sha256:abc", JITConfig: "jit-secret", ID: 42}
}

func TestRunnerQuotaAndPinning(t *testing.T) {
	t.Run("a quota host sets a CFS quota", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{PIDs: true, Swap: true})
		req, err := h.Runner(profile(), runner())
		if err != nil {
			t.Fatal(err)
		}
		hc := req.HostConfig
		if hc.NanoCPUs != 2_000_000_000 || hc.CpusetCpus != "" || hc.Memory != 4<<30 || hc.MemorySwap != 4<<30 || hc.PidsLimit == nil || *hc.PidsLimit != 4096 {
			t.Errorf("limits = %+v", hc)
		}
	})
	t.Run("a pinning host pins whole CPUs and sets no quota", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{Pinning: true, CPUs: 2})
		r := runner()
		r.CPUs = []int{0, 1}
		req, err := h.Runner(profile(), r)
		if err != nil {
			t.Fatal(err)
		}
		if hc := req.HostConfig; hc.CpusetCpus != "0,1" || hc.NanoCPUs != 0 {
			t.Errorf("limits = %+v", hc)
		}
	})
	t.Run("pinned CPUs that do not match cpus are refused", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{Pinning: true, CPUs: 4})
		r := runner()
		r.CPUs = []int{3}
		if _, err := h.Runner(profile(), r); err == nil {
			t.Error("one pinned CPU built a 2-CPU runner")
		}
	})
	t.Run("pinned CPUs on a quota host are refused", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{})
		r := runner()
		r.CPUs = []int{0, 1}
		if _, err := h.Runner(profile(), r); err == nil {
			t.Error("a quota host took pinned CPUs")
		}
	})
	t.Run("a kernel without pids or swap accounting gets neither", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{})
		req, err := h.Runner(profile(), runner())
		if err != nil {
			t.Fatal(err)
		}
		if hc := req.HostConfig; hc.PidsLimit != nil || hc.MemorySwap != 0 {
			t.Errorf("limits = %+v, want no pids limit and no swap field", hc)
		}
	})
}

func TestCheck(t *testing.T) {
	cases := map[string]struct {
		edit func(*config.Profile)
		want string
		plat Platform
	}{
		"disk off XFS pquota":           {edit: func(p *config.Profile) { p.Disk = 10 << 30 }, want: "XFS"},
		"swap without accounting":       {edit: func(p *config.Profile) { p.Swap = 1 << 30 }, want: "swap"},
		"fractional cpus on a pin host": {edit: func(p *config.Profile) { p.MilliCPUs = 1500 }, plat: Platform{Pinning: true, CPUs: 4}, want: "whole CPUs"},
		"more cpus than the cpuset":     {edit: func(p *config.Profile) { p.MilliCPUs = 3000 }, plat: Platform{Pinning: true, CPUs: 2}, want: "the host's 2 CPUs"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := testHost(t, false, tc.plat)
			p := profile()
			tc.edit(p)
			err := h.Check(p)
			if !errors.Is(err, ErrUnenforceable) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "profile small on host nas") {
				t.Errorf("Check = %v, want %q naming the profile and host", err, tc.want)
			}
		})
	}
	t.Run("what the host enforces passes", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{Disk: true, Swap: true, Pinning: true, CPUs: 2})
		p := profile()
		p.Disk, p.Swap = 10<<30, 1<<30
		if err := h.Check(p); err != nil {
			t.Errorf("Check = %v", err)
		}
		req, err := h.Runner(p, &Runner{ImageID: "sha256:abc", CPUs: []int{0, 1}})
		if err != nil || req.HostConfig.StorageOpt["size"] != "10737418240" || req.HostConfig.MemorySwap != 5<<30 {
			t.Errorf("Runner = %+v, %v", req, err)
		}
	})
}

func TestRunnerRequest(t *testing.T) {
	h, _ := testHost(t, false, Platform{SocketGID: 991})
	p := profile()
	p.Tmpfs = []config.Tmpfs{{Path: "/scratch", Size: 1 << 30}}
	req, err := h.Runner(p, runner())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{LabelScope: "acme/app", LabelProfile: "small", LabelRunner: "manade-small-0123456789ab", LabelRunnerID: "42"}
	for k, v := range want {
		if req.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, req.Labels[k], v)
		}
	}
	hc := req.HostConfig
	if req.Image != "sha256:abc" || !slices.Equal(req.Env, []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit-secret"}) || !slices.Equal(req.Entrypoint, p.Command) {
		t.Errorf("request = %+v", req)
	}
	if hc.NetworkMode != "manade" || !slices.Equal(hc.CapDrop, []string{"ALL"}) || !slices.Equal(hc.CapAdd, []string{"NET_ADMIN"}) ||
		!slices.Equal(hc.SecurityOpt, []string{"no-new-privileges:true"}) || hc.Tmpfs["/scratch"] != "size=1073741824,exec" || hc.Privileged {
		t.Errorf("host config = %+v", hc)
	}
	t.Run("host-socket gets its host's daemon socket and group", func(t *testing.T) {
		h, _ := testHost(t, true, Platform{SocketGID: 991})
		p := profile()
		p.Docker = config.DockerHostSocket
		req, err := h.Runner(p, runner())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(req.HostConfig.Mounts, docker.Mount{Type: "bind", Source: "/run/docker.sock", Target: "/var/run/docker.sock"}) ||
			!slices.Equal(req.HostConfig.GroupAdd, []string{"991"}) {
			t.Errorf("mounts %+v groups %v", req.HostConfig.Mounts, req.HostConfig.GroupAdd)
		}
	})
}

func TestRunnerDockerVolume(t *testing.T) {
	h, _ := testHost(t, true, Platform{})
	dk := profile()
	dk.RunnerImage = config.ShippedDocker
	t.Run("the docker image gets its volume at /var/lib/docker", func(t *testing.T) {
		r := runner()
		r.DockerVolume = "vol1"
		req, err := h.Runner(dk, r)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(req.HostConfig.Mounts, docker.Mount{Type: "volume", Source: "vol1", Target: "/var/lib/docker"}) {
			t.Errorf("mounts = %+v", req.HostConfig.Mounts)
		}
	})
	t.Run("the docker image without a volume is refused", func(t *testing.T) {
		if _, err := h.Runner(dk, runner()); err == nil {
			t.Error("a docker-image runner was built without its volume")
		}
	})
	t.Run("a volume for another image is refused", func(t *testing.T) {
		r := runner()
		r.DockerVolume = "vol1"
		if _, err := h.Runner(profile(), r); err == nil {
			t.Error("a base runner was given a /var/lib/docker volume")
		}
	})
}

func TestRunnerMounts(t *testing.T) {
	bind := func(src string) *config.Profile {
		p := profile()
		p.Mounts = []config.Mount{{Source: src, Target: "/cache", Bind: true}, {Source: "named", Target: "/named"}}
		return p
	}
	t.Run("a source under a mount is accepted and passed resolved", func(t *testing.T) {
		h, root := testHost(t, false, Platform{})
		real := filepath.Join(root, "data", "cache")
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "data", "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		req, err := h.Runner(bind(link), runner())
		if err != nil {
			t.Fatal(err)
		}
		want := []docker.Mount{{Type: "bind", Source: real, Target: "/cache"}, {Type: "volume", Source: "named", Target: "/named"}}
		if !slices.Equal(req.HostConfig.Mounts, want) {
			t.Errorf("mounts = %+v, want %+v", req.HostConfig.Mounts, want)
		}
	})
	refused := map[string]func(root string) string{
		"a source on the image's own filesystem": func(string) string { return "/usr/local/bin" },
		"/etc/resolv.conf":                       func(string) string { return "/etc/resolv.conf" },
		"the config directory":                   func(root string) string { return filepath.Join(root, "config") },
		"an ancestor of TLS_DIR":                 func(root string) string { return root },
		"a missing source":                       func(root string) string { return filepath.Join(root, "data", "absent") },
	}
	for name, src := range refused {
		t.Run(name+" is refused", func(t *testing.T) {
			h, root := testHost(t, false, Platform{})
			_, err := h.Runner(bind(src(root)), runner())
			if !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), "profile small") {
				t.Errorf("Runner = %v, want the mount refused naming the profile", err)
			}
		})
	}
	t.Run("/tmp is refused on every host", func(t *testing.T) {
		cfg := &config.Config{File: "/config/config.yaml"}
		h := NewHost(cfg, &config.Host{Name: "nas"}, &Platform{Mounts: []string{"/"}})
		if _, err := h.Runner(bind("/tmp"), runner()); !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), "/tmp") {
			t.Errorf("Runner = %v", err)
		}
	})
	t.Run("a source swapped for a link to a protected path refuses the next create", func(t *testing.T) {
		h, root := testHost(t, false, Platform{})
		src := filepath.Join(root, "data", "cache")
		if err := os.Mkdir(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Runner(bind(src), runner()); err != nil {
			t.Fatalf("first create: %v", err)
		}
		if err := os.Remove(src); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "tls"), src); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Runner(bind(src), runner()); !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), filepath.Join(root, "tls")) {
			t.Errorf("second create = %v, want the link refused naming its target", err)
		}
	})
	t.Run("inside a mount that holds a protected path", func(t *testing.T) {
		h, root := testHost(t, false, Platform{})
		h.plat.Mounts = append(h.plat.Mounts, root)
		link := filepath.Join(root, "data", "to-tls")
		if err := os.Symlink(filepath.Join(root, "tls"), link); err != nil {
			t.Fatal(err)
		}
		for name, src := range map[string]string{"an ancestor of TLS_DIR": root, "a link to TLS_DIR": link} {
			if _, err := h.Runner(bind(src), runner()); !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), "which host nas protects") {
				t.Errorf("%s: Runner = %v, want it refused as protected", name, err)
			}
		}
	})
	t.Run("an open host binds a source as written", func(t *testing.T) {
		h, _ := testHost(t, true, Platform{})
		req, err := h.Runner(bind("/usr/local/bin"), runner())
		if err != nil || req.HostConfig.Mounts[0].Source != "/usr/local/bin" {
			t.Errorf("Runner = %+v, %v", req, err)
		}
	})
}

func TestNewHostProtects(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-tls")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "tls")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{File: "/config/config.yaml", Self: &config.Self{Name: "nas", TLSDir: link}}
	h := NewHost(cfg, &config.Host{Name: "nas", RegistryAuth: "/auth/config.json"}, &Platform{})
	for _, want := range []string{link, real, "/config", "/auth/config.json", "/run/docker.sock", "/var/lib/docker", "/run/manade", "/tmp", "/proc", "/sys", "/dev"} {
		if !slices.Contains(h.protected, want) {
			t.Errorf("protected %v lacks %s", h.protected, want)
		}
	}
}

func TestProbe(t *testing.T) {
	h, _ := testHost(t, false, Platform{PIDs: true, Swap: true})
	probe := h.Probe(&Probe{Name: "manade-probe-1", Run: "run-1", ImageID: "sha256:probe", Command: []string{"/manade", "probe"}, Stdin: true})
	hc := probe.HostConfig
	if probe.Image != "sha256:probe" || probe.User != "65534:65534" || probe.Labels[LabelProbe] != "run-1" || !probe.OpenStdin || !probe.StdinOnce || !probe.AttachStdin || !probe.AttachStdout {
		t.Errorf("probe = %+v", probe)
	}
	if hc.Memory != 32<<20 || hc.MemorySwap != 32<<20 || hc.NetworkMode != "manade" || len(hc.Mounts) != 0 || len(hc.CapAdd) != 0 || len(hc.CapDrop) != 0 || hc.Privileged || hc.PidsLimit == nil {
		t.Errorf("probe host config = %+v, want a job's shape with Docker's default capabilities", hc)
	}
	listener := h.Probe(&Probe{Name: "manade-listener-1", Run: "run-1", ImageID: "sha256:probe", Command: []string{"/manade", "probe", "listen"}})
	if listener.OpenStdin || listener.StdinOnce || listener.AttachStdin || listener.Labels[LabelProbe] != "run-1" {
		t.Errorf("listener = %+v", listener)
	}
}
