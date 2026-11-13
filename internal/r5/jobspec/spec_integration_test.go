//go:build integration

package jobspec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/docker"
)

// The suite runs GitHub's runner image against a real daemon at
// DOCKER_SOCKET, or Docker's default socket:
// go test -tags integration ./internal/jobspec/.
const runnerImage = "ghcr.io/actions/actions-runner:latest"

func realDaemon(t *testing.T) (*docker.Client, string) {
	t.Helper()
	c, pool := dial(t)
	if err := c.PullImage(t.Context(), runnerImage); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	return c, pool
}

func socketPath() string {
	if s := os.Getenv("DOCKER_SOCKET"); s != "" {
		return s
	}
	return "/var/run/docker.sock"
}

// dial connects as a fresh pool, so RemoveOwned touches only this run's
// containers.
func dial(t *testing.T) (*docker.Client, string) {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	pool := "mt" + hex.EncodeToString(b)
	c, err := docker.Dial(t.Context(), socketPath(), pool)
	if err != nil {
		t.Fatalf("Dial %s: %v", socketPath(), err)
	}
	return c, pool
}

func imageID(t *testing.T, c *docker.Client) docker.Image {
	t.Helper()
	img, found, err := c.InspectImage(t.Context(), runnerImage)
	if err != nil || !found {
		t.Fatalf("InspectImage = %v, %v", found, err)
	}
	return img
}

func TestIntegrationUserCheckOnGitHubImage(t *testing.T) {
	c, pool := realDaemon(t)
	img := imageID(t, c)
	h, p := profileOf(t, "")
	id, err := c.CreateContainer(t.Context(), pool+"-probe", h.Probe(p, ProbeUser, img.ID))
	if err != nil {
		t.Fatalf("CreateContainer probe from an image with no entrypoint: %v", err)
	}
	t.Cleanup(func() { _ = c.RemoveOwned(context.Background(), id) })
	var f Files
	for path, dst := range map[string]*[]byte{PasswdPath: &f.Passwd, GroupPath: &f.Group} {
		data, _, err := c.ReadFile(t.Context(), id, path, UserFileLimit)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		*dst = data
	}
	if err := CheckUser(img.Config.User, f); err != nil {
		t.Errorf("CheckUser(%q) on GitHub's image = %v, want nil", img.Config.User, err)
	}
	if u, err := resolveUser(img.Config.User, f); err != nil || u.uid != 1001 || u.gid != 1001 {
		t.Errorf("resolveUser = %+v, %v, want 1001:1001", u, err)
	}
}

// run starts r's spec with entrypoint argv on no network, which these checks
// do not need, and returns its exit code.
func run(t *testing.T, c *docker.Client, req *docker.CreateRequest, name string, argv ...string) int {
	t.Helper()
	req.Entrypoint, req.HostConfig.NetworkMode = argv, "none"
	id, err := c.CreateContainer(t.Context(), name, req)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	t.Cleanup(func() { _ = c.RemoveOwned(context.Background(), id) })
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	return waitExit(t, c, id, 2*time.Minute)
}

func waitExit(t *testing.T, c *docker.Client, id string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		ctr, _, err := c.InspectContainer(t.Context(), id)
		if err != nil {
			t.Fatalf("InspectContainer: %v", err)
		}
		if !ctr.State.FinishedAt.IsZero() {
			return ctr.State.ExitCode
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still running after %s", id, within)
		}
	}
	return -1
}

func TestIntegrationSudo(t *testing.T) {
	c, pool := realDaemon(t)
	img := imageID(t, c)
	tests := []struct {
		name, body string
		wantOK     bool
	}{
		{"defaultProfileRefusesSudo", "", false},
		{"dockerDefaultCaps", "allow_privilege_escalation: true, capabilities: docker-default, scopes: [acme/app]", true},
		{"setuidSetgidOnly", "allow_privilege_escalation: true, capabilities: [SETGID, SETUID], scopes: [acme/app]", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, p := profileOf(t, tt.body)
			r := testRunner
			r.ImageID = img.ID
			code := run(t, c, h.Runner(p, &r), pool+"-"+tt.name, "sudo", "-n", "true")
			if (code == 0) != tt.wantOK {
				t.Errorf("sudo -n true exited %d, want success %v", code, tt.wantOK)
			}
		})
	}
}

func TestIntegrationDiskLimit(t *testing.T) {
	c, pool := realDaemon(t)
	img := imageID(t, c)
	cfg := load(t, "profiles:\n  p: {disk: 64MiB}\n")
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	// The startup's own check: a daemon it refuses disk on is no test of
	// the limit, and the containerd image store accepts the size and
	// ignores it.
	if err := cfg.CheckDaemon(&config.Daemon{
		DataRoot: info.DataRoot, ContainerdAddress: info.ContainerdAddress,
		Driver: info.Driver, DriverStatus: info.DriverStatus, Runtimes: info.Runtimes,
	}); err != nil {
		t.Skipf("the startup refuses disk on this daemon: %v", err)
	}
	h, p := NewHost(cfg, 999), &cfg.Profiles[0]
	r := testRunner
	r.ImageID = img.ID
	req := h.Runner(p, &r)
	req.Entrypoint, req.HostConfig.NetworkMode = []string{"dd", "if=/dev/zero", "of=/home/runner/fill", "bs=1M", "count=128"}, "none"
	id, err := c.CreateContainer(t.Context(), pool+"-disk", req)
	if _, ok := errors.AsType[*docker.APIError](err); ok {
		t.Skipf("the daemon's storage driver does not take a size, which startup refuses disk on: %v", err)
	}
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	t.Cleanup(func() { _ = c.RemoveOwned(context.Background(), id) })
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if code := waitExit(t, c, id, 2*time.Minute); code == 0 {
		t.Errorf("a 128 MiB write under a 64 MiB disk limit exited 0")
	}
}
