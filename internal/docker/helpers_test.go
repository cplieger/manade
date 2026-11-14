package docker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// memListener is an in-memory listener: each dial is one net.Pipe. Inside a
// synctest bubble its connections block durably, so the bubble's clock can
// run a call out to its CallTimeout.
type memListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newMemListener() *memListener {
	return &memListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (*memListener) Addr() net.Addr { return &net.UnixAddr{Name: "memory", Net: "unix"} }

func (l *memListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// fakeDaemon is an Engine API over a memListener. Routes are registered on
// unversioned paths; every request's version prefix and body are recorded.
type fakeDaemon struct {
	mux      *http.ServeMux
	ln       *memListener
	srv      *http.Server
	mu       sync.Mutex
	requests []seen
	took     []string
}

type seen struct {
	header                http.Header
	query                 map[string][]string
	method, path, version string
	body                  []byte
}

// newDaemon starts a fake daemon whose /version answers apiVersion. Inside a
// synctest bubble, call it inside the bubble.
func newDaemon(t *testing.T, apiVersion string) *fakeDaemon {
	t.Helper()
	return newDaemonRange(t, apiVersion, "1.24")
}

// newDaemonRange starts a fake daemon speaking minVersion to apiVersion.
func newDaemonRange(t *testing.T, apiVersion, minVersion string) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{mux: http.NewServeMux(), ln: newMemListener()}
	d.srv = &http.Server{Handler: http.HandlerFunc(d.serve), ReadHeaderTimeout: time.Hour}
	go func() { _ = d.srv.Serve(d.ln) }()
	t.Cleanup(func() { _ = d.srv.Close() })
	d.handle("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ApiVersion": apiVersion, "MinAPIVersion": minVersion})
	})
	return d
}

func (d *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s := seen{method: r.Method, path: r.URL.Path, query: r.URL.Query(), body: body, header: r.Header.Clone()}
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

// observed are the ops the client reported latencies for.
func (d *fakeDaemon) observed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.took...)
}

// dial connects a client with a fixed clock that records each observed op.
func (d *fakeDaemon) dial(t *testing.T) *Client {
	t.Helper()
	c, err := dialWith(t.Context(), d.ln.dial, func(op string, _ time.Duration) {
		d.mu.Lock()
		d.took = append(d.took, op)
		d.mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(c.Close)
	return c
}

// hold blocks a handler until the request's context ends, standing in for a
// daemon that never answers.
func hold(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

// after answers with h once d has passed, or gives up with the request.
func after(d time.Duration, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
			h(w, r)
		case <-r.Context().Done():
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"message": msg})
}

// managedLabelsAt are the labels the client stamps at its fixed clock.
func managedLabelsAt() map[string]string {
	return map[string]string{LabelManaged: "true", labelCreated: "2026-11-10T12:00:00Z"}
}

// inspectAnswer is an inspect answer for a container in state status.
func inspectAnswer(id, status string) map[string]any {
	return map[string]any{"Id": id, "Name": "/" + id, "Config": map[string]any{"Labels": managedLabelsAt()}, "State": map[string]any{"Status": status}}
}
