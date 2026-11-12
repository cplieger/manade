package docker

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon is an Engine API on a Unix socket. Routes are registered on
// unversioned paths; every request's version prefix and body are recorded.
type fakeDaemon struct {
	mux      *http.ServeMux
	socket   string
	mu       sync.Mutex
	requests []seen
}

type seen struct {
	method, path, version string
	query                 map[string][]string
	body                  []byte
}

// newDaemon starts a fake daemon whose /version answers apiVersion.
func newDaemon(t *testing.T, apiVersion string) *fakeDaemon {
	t.Helper()
	return newDaemonRange(t, apiVersion, "1.24")
}

// newDaemonRange starts a fake daemon speaking minVersion to apiVersion.
func newDaemonRange(t *testing.T, apiVersion, minVersion string) *fakeDaemon {
	t.Helper()
	// A short directory keeps the socket path under the 108-byte limit.
	dir, err := os.MkdirTemp("", "mdk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := &fakeDaemon{mux: http.NewServeMux(), socket: filepath.Join(dir, "d.sock")}
	ln, err := net.Listen("unix", d.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(d.serve))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	d.handle("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ApiVersion": apiVersion, "MinAPIVersion": minVersion})
	})
	return d
}

func (d *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s := seen{method: r.Method, path: r.URL.Path, query: r.URL.Query(), body: body}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/v"); ok {
		if v, p, ok := strings.Cut(rest, "/"); ok {
			s.version, s.path = v, "/"+p
		}
	}
	d.mu.Lock()
	d.requests = append(d.requests, s)
	d.mu.Unlock()
	r2 := r.Clone(r.Context())
	r2.URL.Path = s.path
	r2.Body = io.NopCloser(strings.NewReader(string(body)))
	d.mux.ServeHTTP(w, r2)
}

func (d *fakeDaemon) handle(pattern string, h http.HandlerFunc) { d.mux.HandleFunc(pattern, h) }

// seen returns the recorded requests matching method and path.
func (d *fakeDaemon) seen(method, path string) []seen {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []seen
	for _, s := range d.requests {
		if s.method == method && s.path == path {
			out = append(out, s)
		}
	}
	return out
}

// dial connects a client for pool "manade" with a fixed clock.
func (d *fakeDaemon) dial(t *testing.T) *Client {
	t.Helper()
	c, err := Dial(t.Context(), d.socket, "manade")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"message": msg})
}

// ownedLabels are the pool labels of pool "manade".
func ownedLabels() map[string]string {
	return map[string]string{LabelManaged: "true", labelPool: "manade", LabelCreated: "2026-11-10T12:00:00Z"}
}
