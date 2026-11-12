package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/obs"
	"github.com/cplieger/manade/internal/pool"
)

const configDoc = `capacity: {cpus: 16, memory: 64GiB}
scopes:
  - repo: acme/app
profiles:
  p: {}
`

// fakeDaemon answers /version and /info on a Unix socket and counts the
// requests it gets.
type fakeDaemon struct {
	socket string
	info   map[string]any
	calls  atomic.Int32
}

func newDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	// A short directory keeps the socket path under the 108-byte limit.
	dir, err := os.MkdirTemp("", "mcm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := &fakeDaemon{socket: filepath.Join(dir, "d.sock"), info: map[string]any{
		"DockerRootDir": "/var/lib/docker", "Driver": "overlay2", "Runtimes": map[string]any{"runc": map[string]any{}},
		"MemoryLimit": true, "SwapLimit": true, "PidsLimit": true, "CpuCfsQuota": true,
	}}
	ln, err := net.Listen("unix", d.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.calls.Add(1)
		var body any = map[string]string{"ApiVersion": "1.47", "MinAPIVersion": "1.24"}
		if strings.HasSuffix(r.URL.Path, "/info") {
			body = d.info
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return d
}

// setEnv points the config at a written file, a PAT and d's socket, and
// clears every other setting start reads.
func setEnv(t *testing.T, d *fakeDaemon) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(configDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"CONFIG_FILE": file, "GITHUB_TOKEN": "test-token", "DOCKER_SOCKET": d.socket,
		"GITHUB_TOKEN_FILE": "", "GITHUB_APP_CLIENT_ID": "", "GITHUB_APP_PRIVATE_KEY": "", "GITHUB_APP_PRIVATE_KEY_FILE": "",
		"GITHUB_PROXY": "", "DIAG_DIR": "", "DIAG_RETENTION": "", "POOL_NAME": "", "METRICS_LISTEN_ADDR": "", "LOG_LEVEL": "",
	} {
		t.Setenv(k, v)
	}
}

func TestRunRefusesUsage(t *testing.T) {
	for _, args := range [][]string{{"serve"}, {"health", "now"}, {"--help"}} {
		var out bytes.Buffer
		if code := run(args, &out); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if !strings.Contains(out.String(), "usage: manade [health]") {
			t.Errorf("run(%q) wrote %q, want the usage line", args, out.String())
		}
	}
}

func TestStableTagsPickTheStableImages(t *testing.T) {
	for v, stable := range map[string]bool{
		"v1.2.3": true, "v10.0.12": true, "v1.2.3-dev.4": false, "dev": false, "": false, "v1.2": false, "1.2.3": false,
	} {
		if got := stableRe.MatchString(v); got != stable {
			t.Errorf("stable(%q) = %v, want %v", v, got, stable)
		}
	}
}

// TestStartRefusesBeforeServing covers the boot checks start owns. Each
// row's error must name its cause, and the rows that fail before Docker
// must not reach it: the metrics bind precedes any work.
func TestStartRefusesBeforeServing(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		env    map[string]string
		info   map[string]any
		target error
		name   string
		want   string
		docker bool
	}{
		{name: "metricsBindFails", env: map[string]string{"METRICS_LISTEN_ADDR": held.Addr().String()}, want: "METRICS_LISTEN_ADDR"},
		{name: "diagNotADirectory", env: map[string]string{"DIAG_DIR": notDir}, want: "DIAG_DIR"},
		{name: "configInvalid", env: map[string]string{"GITHUB_TOKEN": ""}, want: "GITHUB_TOKEN"},
		{name: "dockerUnreachable", env: map[string]string{"DOCKER_SOCKET": "/nonexistent/docker.sock"}, want: "DOCKER_SOCKET"},
		{name: "kernelLimits", info: map[string]any{"PidsLimit": false}, target: docker.ErrKernelLimits, want: "PidsLimit", docker: true},
		{name: "runtimeMissing", info: map[string]any{"Runtimes": map[string]any{}}, want: `profiles.p.runtime`, docker: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDaemon(t)
			maps.Copy(d.info, tc.info)
			setEnv(t, d)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := start(ctx, slog.New(slog.DiscardHandler), new(slog.LevelVar))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("start = %v, want an error naming %q", err, tc.want)
			}
			if tc.target != nil && !errors.Is(err, tc.target) {
				t.Errorf("start = %v, want errors.Is %v", err, tc.target)
			}
			if got := d.calls.Load() > 0; got != tc.docker {
				t.Errorf("docker reached = %v, want %v", got, tc.docker)
			}
		})
	}
}

// TestLogProfilesNamesGrantsAndMissingDisk loads profiles through the config,
// so each grant is the field the operator typed.
func TestLogProfilesNamesGrantsAndMissingDisk(t *testing.T) {
	setEnv(t, newDaemon(t))
	file := filepath.Join(t.TempDir(), "config.yaml")
	doc := `capacity: {cpus: 16, memory: 64GiB}
scopes:
  - repo: acme/app
profiles:
  plain: {}
  sized: {disk: 10GiB}
  dind:
    runner_image: docker
    allow_privilege_escalation: true
    runtime: sysbox-runc
    capabilities: docker-default
    scopes: [acme/app]
    disk: 10GiB
  lan: {network: host, scopes: [acme/app]}
`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", file)
	cfg, err := config.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logProfiles(slog.New(slog.NewJSONHandler(&buf, nil)), cfg.Profiles)
	warned := map[string][2]any{}
	var noDisk []any
	for line := range strings.Lines(buf.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		switch rec["level"] {
		case "WARN":
			warned[rec["profile"].(string)] = [2]any{rec["grants"], rec["sysbox_root_holds_all_capabilities"]}
		case "INFO":
			noDisk = append(noDisk, rec["profiles"])
		}
	}
	want := map[string][2]any{
		"dind": {"allow_privilege_escalation,capabilities,runtime", true},
		"lan":  {"network", false},
	}
	if !maps.Equal(warned, want) {
		t.Errorf("warnings = %v, want %v", warned, want)
	}
	if !slices.Equal(noDisk, []any{"lan,plain"}) {
		t.Errorf("profiles without disk = %v, want one line naming lan,plain", noDisk)
	}
}

// TestOpenDiagRefusesAnUnwritableDirectory blocks the write check with a
// directory under the probe's name, which refuses the write for root too.
func TestOpenDiagRefusesAnUnwritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".manade-write-check"), 0o700); err != nil {
		t.Fatal(err)
	}
	if root, err := openDiag(dir); err == nil || !strings.Contains(err.Error(), "DIAG_DIR: the pool cannot write there") {
		t.Errorf("openDiag = %v, %v, want the write refusal", root, err)
	}
}

func TestOpenDiagLeavesNoProbe(t *testing.T) {
	dir := t.TempDir()
	root, err := openDiag(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("DIAG_DIR holds %v after the check, want nothing", entries)
	}
	if root, err := openDiag(""); root != nil || err != nil {
		t.Errorf("openDiag(\"\") = %v, %v, want nil, nil", root, err)
	}
}

// testMetrics builds the registry for configDoc.
func testMetrics(t *testing.T) *obs.Metrics {
	t.Helper()
	setEnv(t, newDaemon(t))
	cfg, err := config.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m, err := obs.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// fakeServer is a pool whose health is fixed and whose Run is the test's.
type fakeServer struct {
	run    func(ctx context.Context) error
	health pool.Health
}

func (f *fakeServer) Run(ctx context.Context) error { return f.run(ctx) }
func (f *fakeServer) Health() pool.Health           { return f.health }
func (f *fakeServer) Healthy() bool {
	return f.health.Started && f.health.Listening && f.health.Reconciling
}

func withMarker(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".healthy")
	old := markerPath
	markerPath = path
	t.Cleanup(func() { markerPath = old })
	return path
}

// TestRunPoolServesHealthThroughTheShutdown stops the pool by its context,
// as a signal does: the marker read fresh while it served, /healthz still
// answers during the shutdown and names it, and nothing is left behind.
func TestRunPoolServesHealthThroughTheShutdown(t *testing.T) {
	path := withMarker(t)
	m := testMetrics(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var during struct {
		reasons []string
		status  int
		fresh   bool
	}
	p := &fakeServer{health: pool.Health{Started: true, Listening: true, Reconciling: true}, run: func(ctx context.Context) error {
		for deadline := time.Now().Add(5 * time.Second); !during.fresh && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			during.fresh = health.Inspect(path, health.WithMaxAge(markerLease.Duration())).State == health.MarkerFresh
		}
		cancel()
		<-ctx.Done()
		resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var body struct{ Reasons []string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		during.status, during.reasons = resp.StatusCode, body.Reasons
		return nil
	}}
	if err := runPool(ctx, p, ln, m, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("runPool = %v, want nil after a stop by its context", err)
	}
	if !during.fresh {
		t.Error("the marker never read fresh while the pool was healthy")
	}
	if during.status != http.StatusServiceUnavailable || !slices.Equal(during.reasons, []string{"the pool is shutting down"}) {
		t.Errorf("/healthz during the shutdown = %d %q, want 503 naming the shutdown", during.status, during.reasons)
	}
	if got := health.Inspect(path).State; got != health.MarkerAbsent {
		t.Errorf("marker after runPool = %v, want absent", got)
	}
}

func TestRunPoolStopsWhenTheListenerFails(t *testing.T) {
	withMarker(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	p := &fakeServer{run: func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = runPool(ctx, p, ln, testMetrics(t), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "METRICS_LISTEN_ADDR") || ctx.Err() != nil {
		t.Errorf("runPool = %v (test deadline hit: %v), want the listener's failure", err, ctx.Err() != nil)
	}
}

func TestRunPoolReturnsTheBootFailure(t *testing.T) {
	withMarker(t)
	boot := errors.New("pool: boot refused")
	p := &fakeServer{run: func(context.Context) error { return boot }}
	if err := runPool(t.Context(), p, nil, nil, slog.New(slog.DiscardHandler)); !errors.Is(err, boot) {
		t.Errorf("runPool = %v, want %v", err, boot)
	}
}
