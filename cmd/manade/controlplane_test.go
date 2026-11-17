package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/manade/internal/hostlink"
	"github.com/cplieger/manade/internal/store"
)

// syncBuffer is a log the test reads while the control plane writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// useSocket points the control plane's hosts socket at a fresh path.
func useSocket(t *testing.T) string {
	t.Helper()
	old := hostsSocket
	hostsSocket = filepath.Join(t.TempDir(), "hosts.sock")
	t.Cleanup(func() { hostsSocket = old })
	return hostsSocket
}

// startControlPlane runs the control plane, its ops listener on a free
// loopback port, until the test ends, waiting for its running line; stop
// ends it and answers its exit code.
func startControlPlane(t *testing.T) (out *syncBuffer, stop func() int) {
	t.Helper()
	t.Setenv("OPS_LISTEN_ADDR", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(t.Context())
	out = &syncBuffer{}
	code := make(chan int, 1)
	go func() { code <- serveControlPlane(ctx, out) }()
	t.Cleanup(cancel)
	waitFor(t, "the control plane to start", out, "The control plane is running")
	return out, func() int {
		cancel()
		return <-code
	}
}

var opsRe = regexp.MustCompile(`msg="The control plane is running" .*\bops=(\S+)`)

// opsURL answers the ops listener's URL for path, read from the running
// line.
func opsURL(t *testing.T, out *syncBuffer, path string) string {
	t.Helper()
	m := opsRe.FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("the running line names no ops address; log:\n%s", out.String())
	}
	return "http://" + m[1] + path
}

// opsGet sends method to the ops listener's path with Host host, the URL's
// when empty, answering the status and body.
func opsGet(t *testing.T, out *syncBuffer, method, path, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, opsURL(t, out, path), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// awaitHealthy waits for /healthz to answer 200, the first generation
// booted.
func awaitHealthy(t *testing.T, out *syncBuffer) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		code, body := opsGet(t, out, http.MethodGet, "/healthz", "")
		if code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz = %d %s, want 200; log:\n%s", code, body, out.String())
		}
	}
}

func waitFor(t *testing.T, what string, out *syncBuffer, line string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), line); {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s; log:\n%s", what, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestControlPlaneHoldsTheStoreUntilStopped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, store.File)
	t.Setenv("DATA_DIR", dir)
	sock := useSocket(t)
	out, stop := startControlPlane(t)
	if _, err := openStoreAt(path); !errors.Is(err, bolttable.ErrLocked) {
		t.Errorf("Open while the control plane runs = %v, want ErrLocked", err)
	}
	if fi, err := os.Lstat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("hosts socket = %v, %v; want a socket with mode 0600", fi, err)
	}
	if c := stop(); c != 0 {
		t.Fatalf("serveControlPlane = %d, want 0; log:\n%s", c, out.String())
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hosts socket after the stop: %v, want it removed", err)
	}
	s, err := openStoreAt(path)
	if err != nil {
		t.Fatalf("the store stayed held after the stop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// A host dialing the socket is answered with the upgrade and registered.
func TestControlPlaneAnswersItsHosts(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	sock := useSocket(t)
	out, stop := startControlPlane(t)
	ctx, cancel := context.WithCancel(t.Context())
	up := hostlink.NewUplink(&hostlink.UplinkOptions{
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		Hello:   func() hostlink.Hello { return hostlink.Hello{Name: "default", Build: "test", Instance: "i1"} },
		Handler: http.NotFoundHandler(),
		Log:     slog.New(slog.DiscardHandler),
	})
	done := make(chan struct{})
	go func() {
		up.Run(ctx)
		close(done)
	}()
	waitFor(t, "the host's connection", out, `msg="A host connected" host=default`)
	if st := up.State(); !st.Connected {
		t.Errorf("the host's state = %+v, want connected", st)
	}
	cancel()
	<-done
	if c := stop(); c != 0 {
		t.Fatalf("serveControlPlane = %d, want 0; log:\n%s", c, out.String())
	}
}

func TestControlPlaneRefusesToStart(t *testing.T) {
	held := t.TempDir()
	s, err := openStoreAt(filepath.Join(held, store.File))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, tc := range []struct{ name, dataDir, logs string }{
		{"relative DATA_DIR", "data", "DATA_DIR"},
		{"store held by another process", held, "file=" + filepath.Join(held, store.File)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATA_DIR", tc.dataDir)
			t.Setenv("OPS_LISTEN_ADDR", "127.0.0.1:0")
			useSocket(t)
			var out bytes.Buffer
			if code := serveControlPlane(t.Context(), &out); code != 1 || !strings.Contains(out.String(), tc.logs) {
				t.Errorf("serveControlPlane = %d, want 1 naming %q; log:\n%s", code, tc.logs, out.String())
			}
		})
	}
	t.Run("a hosts socket that cannot be bound", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("DATA_DIR", dir)
		t.Setenv("OPS_LISTEN_ADDR", "127.0.0.1:0")
		useSocket(t)
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		hostsSocket = filepath.Join(file, "hosts.sock")
		var out bytes.Buffer
		if code := serveControlPlane(t.Context(), &out); code != 1 || !strings.Contains(out.String(), "could not listen for its hosts") {
			t.Errorf("serveControlPlane = %d, want 1 naming the socket; log:\n%s", code, out.String())
		}
		st, err := openStoreAt(filepath.Join(dir, store.File))
		if err != nil {
			t.Fatalf("the store stayed held after the refused start: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

// The ops listener answers /healthz and /metrics alone, with no sign-in,
// to the machine itself and to an ALLOWED_HOSTS name, and refuses a page
// that rebinds a name of its own to the address.
func TestControlPlaneOpsListener(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("ALLOWED_HOSTS", "scrape.lan")
	useSocket(t)
	out, stop := startControlPlane(t)
	awaitHealthy(t, out)
	t.Run("metrics", func(t *testing.T) {
		code, body := opsGet(t, out, http.MethodGet, "/metrics", "")
		for _, line := range []string{`manade_build_info{version="` + version + `"} 1`, "manade_booting 0"} {
			if code != http.StatusOK || !strings.Contains(body, line+"\n") {
				t.Errorf("/metrics = %d, want 200 holding %q:\n%s", code, line, body)
			}
		}
	})
	for _, tc := range []struct {
		name, method, path, host, code string
		want                           int
	}{
		{"anAllowedName", http.MethodGet, "/healthz", "scrape.lan:7465", "", http.StatusOK},
		{"localhostFromTheMachine", http.MethodGet, "/metrics", "localhost", "", http.StatusOK},
		{"head", http.MethodHead, "/healthz", "", "", http.StatusOK},
		{"aRebindingName", http.MethodGet, "/metrics", "attacker.example", "host_not_allowed", http.StatusForbidden},
		{"anotherPath", http.MethodGet, "/api/runs", "", "not_found", http.StatusNotFound},
		{"theProductPage", http.MethodGet, "/", "", "not_found", http.StatusNotFound},
		{"anotherMethod", http.MethodPost, "/healthz", "", "method_not_allowed", http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := opsGet(t, out, tc.method, tc.path, tc.host)
			if code != tc.want || !strings.Contains(body, tc.code) {
				t.Errorf("%s %s Host %q = %d %s, want %d %s", tc.method, tc.path, tc.host, code, body, tc.want, tc.code)
			}
		})
	}
	if c := stop(); c != 0 {
		t.Fatalf("serveControlPlane = %d, want 0; log:\n%s", c, out.String())
	}
}

// /healthz answers 503 with the drain's reason while the pool shuts down.
func TestControlPlaneHealthzThroughTheDrain(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	useSocket(t)
	draining, release := make(chan struct{}), make(chan struct{})
	old := drainHook
	drainHook = func() {
		close(draining)
		<-release
	}
	t.Cleanup(func() { drainHook = old })
	out, stop := startControlPlane(t)
	awaitHealthy(t, out)
	code := make(chan int, 1)
	go func() { code <- stop() }()
	<-draining
	got, body := opsGet(t, out, http.MethodGet, "/healthz", "")
	close(release)
	if got != http.StatusServiceUnavailable || !strings.Contains(body, "the control plane is shutting down") {
		t.Errorf("/healthz through the drain = %d %s, want 503 naming the shutdown", got, body)
	}
	if c := <-code; c != 0 {
		t.Fatalf("serveControlPlane = %d, want 0; log:\n%s", c, out.String())
	}
}

// connectHost dials the control plane's socket as the host default, its
// API served by h, until the test ends.
func connectHost(t *testing.T, sock string, out *syncBuffer, h http.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	up := hostlink.NewUplink(&hostlink.UplinkOptions{
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		Hello:   func() hostlink.Hello { return hostlink.Hello{Name: "default", Build: "test", Instance: "i1"} },
		Handler: h,
		Log:     slog.New(slog.DiscardHandler),
	})
	done := make(chan struct{})
	go func() {
		up.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "the host's connection", out, `msg="A host connected" host=default`)
}

// The pool's shutdown reads its hosts over their links, so the control
// plane holds them open until that is done.
func TestControlPlaneKeepsItsHostsThroughThePoolsShutdown(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	sock := useSocket(t)
	draining := make(chan struct{})
	old := drainHook
	drainHook = func() { close(draining) }
	t.Cleanup(func() { drainHook = old })
	called, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	out, stop := startControlPlane(t)
	connectHost(t, sock, out, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-draining:
			once.Do(func() { close(called) })
			<-release
		default:
		}
		http.Error(w, "test: no host API", http.StatusServiceUnavailable)
	}))
	code := make(chan int, 1)
	go func() { code <- stop() }()
	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatalf("the pool's shutdown called no host; log:\n%s", out.String())
	}
	select {
	case c := <-code:
		t.Errorf("serveControlPlane = %d while the pool's shutdown still read its host", c)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if c := <-code; c != 0 {
		t.Fatalf("serveControlPlane = %d, want 0; log:\n%s", c, out.String())
	}
}

// failingListener fails its Accept once fail is closed.
type failingListener struct {
	net.Listener
	fail chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	accepted := make(chan struct{})
	defer close(accepted)
	go func() {
		select {
		case <-l.fail:
			_ = l.Close()
		case <-accepted:
		}
	}()
	return l.Listener.Accept()
}

// A listener that stops on a failure stops the control plane with exit 1,
// so the restart policy restarts it.
func TestControlPlaneStopsWhenAListenerFails(t *testing.T) {
	t.Run("hosts", func(t *testing.T) {
		t.Setenv("DATA_DIR", t.TempDir())
		useSocket(t)
		fail := make(chan struct{})
		old := listenHosts
		listenHosts = func(path string) (net.Listener, error) {
			ln, err := old(path)
			return &failingListener{Listener: ln, fail: fail}, err
		}
		t.Cleanup(func() { listenHosts = old })
		out, stop := startControlPlane(t)
		close(fail)
		waitFor(t, "the hosts listener's failure", out, "The control plane's hosts listener stopped on a failure")
		if c := stop(); c != 1 {
			t.Errorf("serveControlPlane = %d, want 1; log:\n%s", c, out.String())
		}
	})
	t.Run("ops", func(t *testing.T) {
		t.Setenv("DATA_DIR", t.TempDir())
		useSocket(t)
		fail := make(chan struct{})
		old := runOps
		runOps = func(ctx context.Context, ops *httpserver.Ops, served ...httpserver.Served) error {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() {
				<-fail
				cancel()
			}()
			_ = old(ctx, ops, served...)
			return errors.New("test: the ops listener failed")
		}
		t.Cleanup(func() { runOps = old })
		t.Setenv("OPS_LISTEN_ADDR", "127.0.0.1:0")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		out := &syncBuffer{}
		code := make(chan int, 1)
		go func() { code <- serveControlPlane(ctx, out) }()
		waitFor(t, "the control plane to start", out, "The control plane is running")
		close(fail)
		select {
		case c := <-code:
			if c != 1 || !strings.Contains(out.String(), "test: the ops listener failed") {
				t.Errorf("serveControlPlane = %d, want 1 naming the failure; log:\n%s", c, out.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the control plane kept running with its ops listener stopped; log:\n%s", out.String())
		}
	})
}

func TestControlPlaneRefusesItsOpsSettings(t *testing.T) {
	for _, tc := range []struct{ name, key, value, want string }{
		{"anOpsAddressThatCannotBind", "OPS_LISTEN_ADDR", "127.0.0.1:99999", "OPS_LISTEN_ADDR"},
		{"anUnusableAllowedHost", "ALLOWED_HOSTS", "http://scrape.lan", "ALLOWED_HOSTS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DATA_DIR", dir)
			t.Setenv("OPS_LISTEN_ADDR", "127.0.0.1:0")
			t.Setenv(tc.key, tc.value)
			useSocket(t)
			var out bytes.Buffer
			if code := serveControlPlane(t.Context(), &out); code != 1 || !strings.Contains(out.String(), tc.want) {
				t.Errorf("serveControlPlane = %d, want 1 naming %s; log:\n%s", code, tc.want, out.String())
			}
			if _, err := os.Stat(filepath.Join(dir, store.File)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the refused start made the store: %v", err)
			}
		})
	}
}

func TestControlPlaneUsage(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"control-plane", "now"}, &out); code != 2 || !strings.Contains(out.String(), controlPlaneUsage) {
		t.Errorf("run(control-plane now) = %d, %q, want 2 and the usage line", code, out.String())
	}
}
