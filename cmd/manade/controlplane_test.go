package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
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

// startControlPlane runs the control plane until the test ends, waiting for
// its running line; stop ends it and answers its exit code.
func startControlPlane(t *testing.T) (out *syncBuffer, stop func() int) {
	t.Helper()
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
	if _, err := store.Open(path, store.Options{}); !errors.Is(err, bolttable.ErrLocked) {
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
	s, err := store.Open(path, store.Options{})
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
	s, err := store.Open(filepath.Join(held, store.File), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for name, dataDir := range map[string]string{"relative DATA_DIR": "data", "store held by another process": held} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATA_DIR", dataDir)
			useSocket(t)
			var out bytes.Buffer
			if code := serveControlPlane(t.Context(), &out); code != 1 {
				t.Errorf("serveControlPlane = %d, want 1; log:\n%s", code, out.String())
			}
		})
	}
	t.Run("a hosts socket that cannot be bound", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("DATA_DIR", dir)
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
		st, err := store.Open(filepath.Join(dir, store.File), store.Options{})
		if err != nil {
			t.Fatalf("the store stayed held after the refused start: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestControlPlaneUsage(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"control-plane", "now"}, &out); code != 2 || !strings.Contains(out.String(), controlPlaneUsage) {
		t.Errorf("run(control-plane now) = %d, %q, want 2 and the usage line", code, out.String())
	}
}
