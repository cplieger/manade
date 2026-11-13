package jobspec

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	return hostAt(root, open, plat), root
}

// hostAt is testHost's host in root, serving profiles.
func hostAt(root string, open bool, plat Platform, profiles ...config.Profile) *Host {
	cfg := &config.Config{
		File: filepath.Join(root, "config", "config.yaml"), Self: &config.Self{Name: "nas", TLSDir: filepath.Join(root, "tls")},
		Profiles: profiles,
	}
	hc := &config.Host{Name: "nas", Egress: config.Egress{Internet: true, Open: open}}
	if plat.Network == "" {
		plat.Network = "manade"
	}
	plat.Mounts = append(plat.Mounts, filepath.Join(root, "data"))
	h := NewHost(cfg, hc, &plat)
	h.protected = slices.DeleteFunc(h.protected, func(p string) bool { return p == "/tmp" })
	return h
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
	t.Run("a kernel without pids or swap accounting gets no pids limit and unlimited swap", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{})
		req, err := h.Runner(profile(), runner())
		if err != nil {
			t.Fatal(err)
		}
		if hc := req.HostConfig; hc.PidsLimit != nil || hc.MemorySwap != -1 || hc.Memory != 4<<30 {
			t.Errorf("limits = %+v, want no pids limit, the memory limit and MemorySwap -1", hc)
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

func TestRunnerMountNesting(t *testing.T) {
	// serve builds host nas in root serving profiles small (binding src
	// read-only, or writable) and big (binding other, writable when
	// otherWritable), and answers it with small.
	serve := func(root, src string, writable bool, other string, otherWritable bool) (*Host, *config.Profile) {
		small := profile()
		small.Hosts = []string{"nas"}
		small.Mounts = []config.Mount{{Source: src, Target: "/cache", Bind: true, ReadOnly: !writable}}
		big := profile()
		big.Name, big.Hosts = "big", []string{"nas"}
		big.Mounts = []config.Mount{{Source: other, Target: "/work", Bind: true, ReadOnly: !otherWritable}}
		return hostAt(root, false, Platform{}, *small, *big), small
	}
	mkdirs := func(t *testing.T, dirs ...string) {
		t.Helper()
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	// linkInto makes root/data/link to root/data/work/x and
	// root/data/elsewhere, and answers work and x.
	linkInto := func(t *testing.T, root string) (work, x, link string) {
		t.Helper()
		work = filepath.Join(root, "data", "work")
		x = filepath.Join(work, "x")
		mkdirs(t, x, filepath.Join(root, "data", "elsewhere"))
		link = filepath.Join(root, "data", "link")
		if err := os.Symlink(x, link); err != nil {
			t.Fatal(err)
		}
		return work, x, link
	}
	// swap is a job on work replacing x with a link out of its mount.
	swap := func(t *testing.T, root, x string) {
		t.Helper()
		if err := os.Remove(x); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "data", "elsewhere"), x); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a link into another profile's writable source is refused, after a swap too", func(t *testing.T) {
		_, root := testHost(t, false, Platform{})
		work, x, link := linkInto(t, root)
		h, small := serve(root, link, false, work, true)
		_, err := h.Runner(small, runner())
		if !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), "profile small") || !strings.Contains(err.Error(), "profile big's jobs may write") {
			t.Fatalf("Runner = %v, want the source refused naming both profiles", err)
		}
		swap(t, root, x)
		if _, err := h.Runner(small, runner()); !errors.Is(err, ErrMountRefused) {
			t.Errorf("Runner after the swap = %v, want refused", err)
		}
	})
	t.Run("a link into another profile's writable source swapped before the host was built is refused", func(t *testing.T) {
		_, root := testHost(t, false, Platform{})
		work, x, link := linkInto(t, root)
		swap(t, root, x)
		h, small := serve(root, link, false, work, true)
		if _, err := h.Runner(small, runner()); !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), "profile big's jobs may write") {
			t.Errorf("Runner = %v, want refused", err)
		}
	})
	t.Run("a writable source another's link goes through is refused after a swap", func(t *testing.T) {
		_, root := testHost(t, false, Platform{})
		work, x, link := linkInto(t, root)
		h, small := serve(root, work, true, link, false)
		swap(t, root, x)
		_, err := h.Runner(small, runner())
		if !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), "writable to jobs and an ancestor of "+x) {
			t.Errorf("Runner = %v, want refused for the link it holds", err)
		}
	})
	t.Run("a writable link to an ancestor of another source is refused", func(t *testing.T) {
		_, root := testHost(t, false, Platform{})
		parent := filepath.Join(root, "data", "parent")
		mkdirs(t, filepath.Join(parent, "child"))
		link := filepath.Join(root, "data", "up")
		if err := os.Symlink(parent, link); err != nil {
			t.Fatal(err)
		}
		h, small := serve(root, link, true, filepath.Join(parent, "child"), false)
		_, err := h.Runner(small, runner())
		if !errors.Is(err, ErrMountRefused) || !strings.Contains(err.Error(), "writable to jobs and an ancestor of") || !strings.Contains(err.Error(), "profile big") {
			t.Errorf("Runner = %v, want the source refused naming both profiles", err)
		}
	})
	t.Run("siblings reached through one link pass", func(t *testing.T) {
		_, root := testHost(t, false, Platform{})
		x := filepath.Join(root, "data", "x")
		mkdirs(t, filepath.Join(x, "b"), filepath.Join(x, "c"))
		via := filepath.Join(root, "data", "via")
		if err := os.Symlink(x, via); err != nil {
			t.Fatal(err)
		}
		h, small := serve(root, filepath.Join(via, "b"), true, filepath.Join(via, "c"), false)
		if _, err := h.Runner(small, runner()); err != nil {
			t.Errorf("Runner = %v, want a writable via/b and a via/c apart", err)
		}
	})
	t.Run("sources apart, equal, or nested read-only pass", func(t *testing.T) {
		_, root := testHost(t, false, Platform{})
		a, b := filepath.Join(root, "data", "a"), filepath.Join(root, "data", "b")
		mkdirs(t, filepath.Join(a, "sub"), b)
		for name, tc := range map[string]struct {
			src, other    string
			writable, own bool
		}{
			"apart, both writable":  {src: a, other: b, writable: true, own: true},
			"equal, both writable":  {src: a, other: a, writable: true, own: true},
			"nested, both readonly": {src: filepath.Join(a, "sub"), other: a},
		} {
			h, small := serve(root, tc.src, tc.writable, tc.other, tc.own)
			if _, err := h.Runner(small, runner()); err != nil {
				t.Errorf("%s: Runner = %v", name, err)
			}
		}
	})
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, to := range map[string]string{"abs": real, "rel": "real", "chain": "rel/sub", "loop": "loop", "file": "f"} {
		if err := os.Symlink(to, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"abs", "rel", "chain", "abs/sub/../sub", "real/./sub"} {
		t.Run(src, func(t *testing.T) {
			p := filepath.Join(root, src)
			if strings.Contains(src, "..") {
				p = root + "/" + src
			}
			want, err := filepath.EvalSymlinks(p)
			if err != nil {
				t.Fatal(err)
			}
			w, err := resolve(p)
			if err != nil || w.final != want || !slices.Contains(w.forms, want) || w.forms[0] != p {
				t.Errorf("resolve = %+v, %v, want final %s as EvalSymlinks", w, err, want)
			}
		})
	}
	t.Run("a chain's forms are its spellings and its lookups hold each link", func(t *testing.T) {
		w, err := resolve(filepath.Join(root, "chain"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(root, "chain"), filepath.Join(root, "rel/sub"), filepath.Join(root, "real/sub")}
		if !slices.Equal(w.forms, want) {
			t.Errorf("forms = %v, want %v", w.forms, want)
		}
		for _, l := range []string{filepath.Join(root, "chain"), filepath.Join(root, "rel"), filepath.Join(root, "real"), filepath.Join(root, "real/sub")} {
			if !slices.Contains(w.lookups, l) {
				t.Errorf("lookups %v lack %s", w.lookups, l)
			}
		}
	})
	t.Run("a link inside the path is no form of it", func(t *testing.T) {
		w, err := resolve(filepath.Join(root, "abs", "sub"))
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(w.forms, filepath.Join(root, "abs")) {
			t.Errorf("forms = %v hold the link abs, an ancestor", w.forms)
		}
	})
	// A file followed by .. fails as EvalSymlinks does, though the kernel
	// is never asked to look under it.
	for src, want := range map[string]error{"loop": errTooManyLinks, "file/x": syscall.ENOTDIR, "f/..": syscall.ENOTDIR, "absent": fs.ErrNotExist} {
		t.Run(src+" fails", func(t *testing.T) {
			if _, err := resolve(root + "/" + src); !errors.Is(err, want) {
				t.Errorf("resolve = %v, want %v", err, want)
			}
		})
	}
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
	t.Run("a kernel without swap accounting gets unlimited swap", func(t *testing.T) {
		h, _ := testHost(t, false, Platform{})
		hc := h.Probe(&Probe{Name: "manade-probe-2", Run: "run-2", ImageID: "sha256:probe"}).HostConfig
		if hc.Memory != 32<<20 || hc.MemorySwap != -1 {
			t.Errorf("probe memory = %d, swap = %d, want 32 MiB and -1", hc.Memory, hc.MemorySwap)
		}
	})
}
