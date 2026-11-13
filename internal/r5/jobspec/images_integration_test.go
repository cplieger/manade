//go:build integration

package jobspec

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/docker"
)

// TestImages runs each shipped image's images/<name>/smoke.sh in a container
// created from the request Runner writes for a profile on that image, so its
// user, entrypoint and security options are the pool's. It needs the -dev
// tags in the daemon, which building images/ in order writes:
//
//	docker build -t ghcr.io/cplieger/manade-runner:base-dev -f images/base/Dockerfile .
//	(then build, browser and docker)
//	go test -tags integration -run TestImages ./internal/jobspec/
func TestImages(t *testing.T) {
	c, pool := dial(t)
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	before := volumes(t)
	const elevated = "runner_image: docker, allow_privilege_escalation: true, scopes: [acme/app]"
	tests := []struct {
		name, image, body, arg string
		wantOK, sysbox         bool
	}{
		{"base", "base", "runner_image: base", "", true, false},
		{"build", "build", "runner_image: build", "", true, false},
		{"browser", "browser", "runner_image: browser", "", true, false},
		{"dockerSysbox", "docker", elevated + ", runtime: sysbox-runc, capabilities: docker-default", "any", true, true},
		{"dockerPrivileged", "docker", elevated + ", privileged: true", "copy", true, false},
		{"dockerPrivilegedTmpfs", "docker", elevated + ", privileged: true, tmpfs: [{path: /var/lib/docker, size: 2GiB}]", "overlay", true, false},
		{"dockerDefaultSecurity", "docker", `image: "` + ImageRef(&config.Profile{RunnerImage: "docker"}, true) + `", command: [` + config.DockerWrapper + `]`, "any", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.sysbox && !slices.Contains(info.Runtimes, config.RuntimeSysbox) {
				t.Skip("the daemon has no sysbox-runc runtime")
			}
			img := shippedImage(t, c, tt.image)
			script, err := os.ReadFile(filepath.Join("..", "..", "..", "images", tt.image, "smoke.sh"))
			if err != nil {
				t.Fatal(err)
			}
			h, p := profileOf(t, tt.body)
			r := testRunner
			r.ImageID = img.ID
			req := h.Runner(p, &r)
			argv := []string{"/bin/bash", "-c", string(script), "smoke"}
			if tt.arg != "" {
				argv = append(argv, tt.arg)
			}
			// The docker image's wrapper stays the entrypoint and runs argv
			// in place of the runner; the others' run.sh would register one.
			if tt.image == config.ShippedDocker {
				req.Entrypoint = append(req.Entrypoint, argv...)
			} else {
				req.Entrypoint = argv
			}
			// The scripts download toolchains and images. The isolated
			// network's limits are the host firewall's, not under test.
			req.HostConfig.NetworkMode = "bridge"
			id, err := c.CreateContainer(t.Context(), pool+"-"+tt.name, req)
			if err != nil {
				t.Fatalf("CreateContainer: %v", err)
			}
			removed := false
			t.Cleanup(func() {
				if !removed {
					_ = c.RemoveOwned(context.Background(), id)
				}
			})
			if err := c.StartContainer(t.Context(), id); err != nil {
				t.Fatalf("StartContainer: %v", err)
			}
			code := waitExit(t, c, id, 15*time.Minute)
			logs, err := c.Logs(t.Context(), id, 50)
			if err != nil {
				t.Fatalf("Logs: %v", err)
			}
			has := func(s string) bool {
				return slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, s) })
			}
			if tt.wantOK && (code != 0 || !has("manade-runner "+tt.image+" smoke: ok")) {
				t.Errorf("exit %d, logs:\n%s", code, strings.Join(logs, "\n"))
			}
			if !tt.wantOK && (code == 0 || !has("manade-dockerd-run: dockerd exited before it answered")) {
				t.Errorf("exit %d, want the wrapper's refusal; logs:\n%s", code, strings.Join(logs, "\n"))
			}
			if err := c.RemoveOwned(t.Context(), id); err != nil {
				t.Fatalf("RemoveOwned: %v", err)
			}
			removed = true
			if after := volumes(t); !slices.Equal(after, before) {
				t.Errorf("volumes after removal = %v, want %v", after, before)
			}
		})
	}
}

// shippedImage is the daemon's copy of name's -dev tag, checked for the label
// the pool's image removal keys on. A missing tag fails instead of being
// pulled, because the registry's copy is not the tree's Dockerfile.
func shippedImage(t *testing.T, c *docker.Client, name string) docker.Image {
	t.Helper()
	ref := ImageRef(&config.Profile{RunnerImage: name}, true)
	img, found, err := c.InspectImage(t.Context(), ref)
	if err != nil {
		t.Fatalf("InspectImage %s: %v", ref, err)
	}
	if !found {
		t.Fatalf("image %s is not in the daemon; build images/ first (see TestImages)", ref)
	}
	if got := img.Config.Labels[docker.LabelRunnerImage]; got != name {
		t.Fatalf("image %s label %s = %q, want %q", ref, docker.LabelRunnerImage, got, name)
	}
	return img
}

// volumes lists the daemon's volume names, sorted. The pool's client has no
// volume call, because the pool never makes one.
func volumes(t *testing.T) []string {
	t.Helper()
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath())
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://docker/volumes", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET /volumes: %v", err)
	}
	defer resp.Body.Close()
	var body struct{ Volumes []struct{ Name string } }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /volumes: %s, %v", resp.Status, err)
	}
	names := make([]string, 0, len(body.Volumes))
	for _, v := range body.Volumes {
		names = append(names, v.Name)
	}
	slices.Sort(names)
	return names
}
