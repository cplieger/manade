package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
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

func TestControlPlaneHoldsTheStoreUntilStopped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, store.File)
	t.Setenv("DATA_DIR", dir)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out syncBuffer
	code := make(chan int, 1)
	go func() { code <- serveControlPlane(ctx, &out) }()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "The control plane is running"); {
		if time.Now().After(deadline) {
			t.Fatalf("the control plane did not start; log:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := store.Open(path, store.Options{}); !errors.Is(err, bolttable.ErrLocked) {
		t.Errorf("Open while the control plane runs = %v, want ErrLocked", err)
	}
	cancel()
	if c := <-code; c != 0 {
		t.Fatalf("serveControlPlane = %d, want 0; log:\n%s", c, out.String())
	}
	s, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatalf("the store stayed held after the stop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
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
			var out bytes.Buffer
			if code := serveControlPlane(t.Context(), &out); code != 1 {
				t.Errorf("serveControlPlane = %d, want 1; log:\n%s", code, out.String())
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
