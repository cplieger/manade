package docker

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// infoAnswer is a /info body as a 1.51 daemon writes it, with every field
// the client reads.
func infoAnswer() map[string]any {
	return map[string]any{
		"DockerRootDir":   "/srv/docker",
		"Driver":          "overlay2",
		"DriverStatus":    [][2]string{{"Backing Filesystem", "xfs"}, {"Supports d_type", "true"}},
		"Runtimes":        map[string]any{"runc": map[string]string{"path": "runc"}, "io.containerd.runc.v2": map[string]any{}, "runsc": map[string]any{}},
		"MemoryLimit":     true,
		"SwapLimit":       true,
		"PidsLimit":       true,
		"CpuCfsQuota":     true,
		"Containerd":      map[string]any{"Address": "/run/containerd/containerd.sock", "Namespaces": map[string]string{"Containers": "moby"}},
		"FirewallBackend": map[string]any{"Driver": "nftables"},
	}
}

func infoDaemon(t *testing.T, version string, answer map[string]any) *Client {
	t.Helper()
	d := newDaemon(t, version)
	d.handle("GET /info", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, answer) })
	return d.dial(t)
}

func TestInfo(t *testing.T) {
	t.Run("readsEveryFieldAtANewVersion", func(t *testing.T) {
		info, err := infoDaemon(t, "1.51", infoAnswer()).Info(t.Context())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.DataRoot != "/srv/docker" || info.Driver != "overlay2" {
			t.Errorf("DataRoot, Driver = %q, %q", info.DataRoot, info.Driver)
		}
		if !slices.Equal(info.DriverStatus, [][2]string{{"Backing Filesystem", "xfs"}, {"Supports d_type", "true"}}) {
			t.Errorf("DriverStatus = %v", info.DriverStatus)
		}
		if !slices.Equal(info.Runtimes, []string{"io.containerd.runc.v2", "runc", "runsc"}) {
			t.Errorf("Runtimes = %v, want sorted names", info.Runtimes)
		}
		if info.ContainerdAddress != "/run/containerd/containerd.sock" {
			t.Errorf("ContainerdAddress = %q", info.ContainerdAddress)
		}
		if info.FirewallBackend != "nftables" {
			t.Errorf("FirewallBackend = %q, want nftables", info.FirewallBackend)
		}
		if err := info.CheckLimits(); err != nil {
			t.Errorf("CheckLimits = %v", err)
		}
	})
	t.Run("ignoresFieldsNewerThanTheNegotiatedVersion", func(t *testing.T) {
		info, err := infoDaemon(t, "1.43", infoAnswer()).Info(t.Context())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.FirewallBackend != "" {
			t.Errorf("FirewallBackend at 1.43 = %q, want unknown", info.FirewallBackend)
		}
		if info.ContainerdAddress != "" {
			t.Errorf("ContainerdAddress at 1.43 = %q, want the default fallback", info.ContainerdAddress)
		}
	})
	t.Run("readsContainerdFrom146AndTheFirewallFrom149", func(t *testing.T) {
		info, err := infoDaemon(t, "1.46", infoAnswer()).Info(t.Context())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.ContainerdAddress == "" || info.FirewallBackend != "" {
			t.Errorf("at 1.46: ContainerdAddress %q, FirewallBackend %q; want the first only", info.ContainerdAddress, info.FirewallBackend)
		}
		info, err = infoDaemon(t, "1.49", infoAnswer()).Info(t.Context())
		if err != nil || info.FirewallBackend != "nftables" {
			t.Errorf("at 1.49: FirewallBackend %q, %v; want nftables", info.FirewallBackend, err)
		}
	})
}

func TestCheckLimits(t *testing.T) {
	for _, name := range []string{"MemoryLimit", "SwapLimit", "PidsLimit", "CpuCfsQuota"} {
		t.Run(name, func(t *testing.T) {
			answer := infoAnswer()
			answer[name] = false
			info, err := infoDaemon(t, "1.51", answer).Info(t.Context())
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			err = info.CheckLimits()
			if !errors.Is(err, ErrKernelLimits) || !strings.Contains(err.Error(), name) {
				t.Errorf("CheckLimits = %v, want ErrKernelLimits naming %s", err, name)
			}
		})
	}
	t.Run("namesEveryMissingLimit", func(t *testing.T) {
		err := (&Info{SwapLimit: true, CPUCFSQuota: true}).CheckLimits()
		if err == nil || !strings.Contains(err.Error(), "MemoryLimit, PidsLimit") {
			t.Errorf("CheckLimits = %v, want both missing named", err)
		}
	})
}

func TestSocketGID(t *testing.T) {
	t.Run("readsTheSocketsGroup", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		gid, err := d.dial(t).SocketGID()
		fi, serr := os.Stat(d.socket)
		if err != nil || serr != nil {
			t.Fatalf("SocketGID: %v, stat: %v", err, serr)
		}
		if want := fi.Sys().(*syscall.Stat_t).Gid; gid != want {
			t.Errorf("SocketGID = %d, want %d", gid, want)
		}
	})
	t.Run("refusesARegularFile", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "docker.sock")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		c := &Client{socket: p}
		if _, err := c.SocketGID(); err == nil || !strings.Contains(err.Error(), "not a Unix socket") {
			t.Errorf("SocketGID = %v, want a refusal", err)
		}
	})
	t.Run("failsOnAMissingPath", func(t *testing.T) {
		c := &Client{socket: filepath.Join(t.TempDir(), "absent.sock")}
		if _, err := c.SocketGID(); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("SocketGID = %v, want ErrNotExist", err)
		}
	})
}

func TestPingFailure(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /_ping", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusInternalServerError, "down") })
	if err := d.dial(t).Ping(t.Context()); err == nil {
		t.Error("Ping on a 500 succeeded")
	}
}
