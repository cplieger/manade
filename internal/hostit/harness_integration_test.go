//go:build integration

// Package hostit runs runner hosts from the built host image on the
// machine's Docker and drives them as the control plane does: each host
// dials the hosts socket the test listens on, and the test calls its API
// over that connection. It needs the docker CLI, a daemon that runs
// privileged containers, and the images CI's docker job bakes (or a local
// build of images/ in order, each runner image tagged <name>-dev, and the
// root Dockerfile tagged as MANADE_APP_IMAGE names, default ci-smoke:latest):
//
//	docker build -t ghcr.io/cplieger/manade-runner:base-dev -f images/base/Dockerfile .
//	(then build, browser and host)
//	docker build -t ci-smoke:latest .
//	go test -tags integration ./internal/hostit/
package hostit

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/hostlink"
	"github.com/cplieger/manade/internal/jobspec"
)

// baked is the tag CI's docker job and a local build give runner image name.
func baked(name string) string { return jobspec.RunnerRepo + ":" + name + "-dev" }

// appImage is the control plane's image.
func appImage() string { return cmp.Or(os.Getenv("MANADE_APP_IMAGE"), "ci-smoke:latest") }

// innerSocket is the host's own daemon, inside its container.
const innerSocket = "unix:///run/docker.sock"

// controlPlane is the test's side of the host channel: the hosts socket in
// dir, which each host mounts at /run/manade.
type controlPlane struct {
	srv *hostlink.Server
	dir string
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	dir := t.TempDir()
	ln, err := hostlink.ListenSocket(filepath.Join(dir, filepath.Base(hostlink.SocketPath)))
	if err != nil {
		t.Fatal(err)
	}
	cp := &controlPlane{srv: hostlink.NewServer(slog.New(slog.NewTextHandler(os.Stderr, nil))), dir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cp.srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the hosts listener: %v", err)
		}
	})
	return cp
}

// host is one runner host container connected to cp.
type host struct {
	t    *testing.T
	cp   *controlPlane
	name string
	ctr  string
}

// startHost runs host default from the baked host image with env and the
// docker run flags extra, and waits for its connection. ghcr.io resolves to
// loopback in the host, so its daemon never replaces a loaded image with
// the registry's copy, which is not this tree's.
func startHost(t *testing.T, cp *controlPlane, env []string, extra ...string) *host {
	t.Helper()
	needImage(t, baked("host"))
	h := &host{t: t, cp: cp, name: "default", ctr: "manadetest-hostit-" + randHex(t)}
	args := []string{
		"run", "-d", "--name", h.ctr, "--privileged", cgroupns(t),
		"--memory=8g", "--add-host=ghcr.io:127.0.0.1", "-e", "LOG_LEVEL=debug",
		"-v", cp.dir + ":/run/manade", "-v", "/var/lib/docker",
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(append(args, extra...), baked("host"))
	docker(t, args...)
	t.Cleanup(h.stop)
	h.link("")
	return h
}

// stop prints the host's log tail when the test failed, then removes the
// container and its data volume.
func (h *host) stop() {
	if h.t.Failed() {
		out, _ := exec.Command("docker", "logs", "--tail", "200", h.ctr).CombinedOutput()
		h.t.Logf("host %s log:\n%s", h.ctr, out)
	}
	if out, err := exec.Command("docker", "rm", "-f", "-v", h.ctr).CombinedOutput(); err != nil {
		h.t.Errorf("removing host %s: %v: %s", h.ctr, err, out)
	}
}

// link waits for the host's connection from an instance other than not.
func (h *host) link(not string) *hostlink.Link {
	h.t.Helper()
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); {
		if l, ok := h.cp.srv.Link(h.name); ok && l.Instance != not {
			return l
		}
		time.Sleep(time.Second)
	}
	h.t.Fatalf("host %s did not connect", h.ctr)
	return nil
}

// api is the host's API over its current connection.
func (h *host) api() *hostapi.Client { return h.link("").API }

// deliver puts the section settings at revision rev, again while the host
// answers that it is starting or applying, as the control plane does.
func (h *host) deliver(rev uint64, settings string) (hostapi.SectionAnswer, error) {
	h.t.Helper()
	sec := &hostapi.Section{Revision: rev, Settings: jsontext.Value(settings)}
	for deadline := time.Now().Add(5 * time.Minute); ; {
		ctx, cancel := context.WithTimeout(h.t.Context(), 2*time.Minute)
		ans, err := h.api().PutSection(ctx, sec)
		cancel()
		if (!errors.Is(err, hostapi.ErrStarting) && !errors.Is(err, hostapi.ErrApplying)) || time.Now().After(deadline) {
			return ans, err
		}
		time.Sleep(2 * time.Second)
	}
}

// await polls the host's status until ok holds, failing the test past
// within.
func (h *host) await(what string, within time.Duration, ok func(*hostapi.Status) bool) *hostapi.Status {
	h.t.Helper()
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
		st, err := h.api().Status(ctx)
		cancel()
		if err == nil && ok(&st) {
			return &st
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("section %+v, profiles %+v", st.Section, st.Profiles)
		}
		time.Sleep(2 * time.Second)
	}
	h.t.Fatalf("host %s: waited %s for %s; last: %v", h.ctr, within, what, last)
	return nil
}

// up waits for the host to run the section of hash, its rules loaded.
func (h *host) up(hash string) *hostapi.Status {
	h.t.Helper()
	return h.await("it to run its section", 5*time.Minute, func(st *hostapi.Status) bool {
		return st.Section.State == hostapi.SectionRunning && st.Section.Hash == hash && st.PolicyHash != ""
	})
}

// load copies baked image name from the machine's daemon into the host's,
// tagged only as the host resolves a profile on it.
func (h *host) load(name string) {
	h.t.Helper()
	needImage(h.t, baked(name))
	save := exec.CommandContext(h.t.Context(), "docker", "save", baked(name))
	load := exec.CommandContext(h.t.Context(), "docker", "exec", "-i", h.ctr, "docker", "-H", innerSocket, "load", "-q")
	pipe, err := save.StdoutPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	var saveErr, loadOut bytes.Buffer
	save.Stderr, load.Stdin, load.Stdout, load.Stderr = &saveErr, pipe, &loadOut, &loadOut
	if err := save.Start(); err != nil {
		h.t.Fatal(err)
	}
	loadErr := load.Run()
	if err := save.Wait(); err != nil || loadErr != nil {
		h.t.Fatalf("loading %s into host %s: save %v: %s; load %v: %s", name, h.ctr, err, saveErr.Bytes(), loadErr, loadOut.Bytes())
	}
	ref := jobspec.ImageRef(&config.Profile{RunnerImage: name}, "dev")
	h.inner("tag", baked(name), ref)
	h.inner("image", "rm", baked(name))
}

// inner runs the docker CLI against the host's own daemon.
func (h *host) inner(args ...string) string {
	h.t.Helper()
	return docker(h.t, append([]string{"exec", h.ctr, "docker", "-H", innerSocket}, args...)...)
}

// run starts a runner of profile, waits for it to exit within, and answers
// it as the host lists it exited, and its log tail; the runner is deleted
// when t ends.
func (h *host) run(t *testing.T, profile string, within time.Duration) (exited hostapi.Runner, logs string) {
	t.Helper()
	name := "manadetest-" + profile + "-" + randHex(t)
	req := &hostapi.RunnerRequest{Profile: profile, RunnerName: name, Scope: "acme/app", JITConfig: "unused", RunnerID: 1}
	if _, err := h.api().CreateRunner(t.Context(), req); err != nil {
		t.Fatalf("POST /v1/runners for %s: %v", profile, err)
	}
	t.Cleanup(func() {
		if _, err := h.api().DeleteRunner(context.Background(), name, false); err != nil {
			t.Errorf("DELETE runner %s: %v", name, err)
		}
	})
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
		l, err := h.api().Runners(t.Context())
		if err != nil {
			t.Fatalf("GET /v1/runners: %v", err)
		}
		for _, r := range l.Runners {
			if r.Name != name {
				continue
			}
			switch r.State {
			case hostapi.StateFailed:
				t.Fatalf("runner %s failed: %s", name, r.Reason)
			case hostapi.StateExited:
				out, err := h.api().Logs(t.Context(), name, 100)
				if err != nil {
					t.Fatalf("logs of %s: %v", name, err)
				}
				return r, string(out)
			}
		}
	}
	t.Fatalf("runner %s did not exit within %s", name, within)
	return hostapi.Runner{}, ""
}

// settings is host default's section: its hosts entry host, serving
// profiles, each a "name": {...} member.
func settings(host string, profiles ...string) string {
	return `{"hosts": {"default": ` + host + `}, "profiles": {` + strings.Join(profiles, ", ") + `}}`
}

// hashOf is the hash a host reports for settings.
func hashOf(t *testing.T, settings string) string {
	t.Helper()
	h, err := (&hostapi.Section{Settings: jsontext.Value(settings)}).Hash()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// cgroupns is the docker run flag a host needs for its cgroup namespace:
// private on cgroup v2, where the host refuses the machine's, and the
// default on cgroup v1, where a 4.4 kernel has no cgroup namespace.
func cgroupns(t *testing.T) string {
	t.Helper()
	if docker(t, "info", "--format", "{{.CgroupVersion}}") == "2" {
		return "--cgroupns=private"
	}
	return "--cgroupns=host"
}

// needImage fails the test when the machine's daemon lacks image ref.
func needImage(t *testing.T, ref string) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", ref).Run(); err != nil {
		t.Fatalf("image %s is not in the daemon; build it first (see the package doc)", ref)
	}
}

// docker runs the docker CLI and answers its trimmed output.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
