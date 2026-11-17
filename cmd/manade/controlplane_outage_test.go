package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

// writeStoreFile leaves at path a store file whose meta holds the stamp
// raw, as the build that stamped it wrote it, with a namespace this build
// does not declare when undeclared is set.
func writeStoreFile(t *testing.T, path, raw string, undeclared bool) {
	t.Helper()
	s := bolttable.NewSchema("manade")
	meta := bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("meta")), 0x01, "meta",
		bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	if undeclared {
		bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("later")), 0xef, "rows",
			bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	}
	db, err := bolttable.Open(path, s, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := json.RawMessage(raw)
	if err := db.Update(t.Context(), func(tx *bolttable.Tx) error { return meta.Put(tx, []byte("store"), &v) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

var outageOpsRe = regexp.MustCompile(`msg="The control plane's store cannot open.*\bops=(\S+)`)

// A store file that cannot open holds the control plane in its outage:
// /healthz answers 503 with the class's reason alone, and with the
// shutdown's beside it through a stop, the log names the file, its class
// and its error, the file is left as it was, and a stop exits 0 with the
// lock released.
func TestControlPlaneHoldsItsStoresOutage(t *testing.T) {
	for _, tc := range []struct {
		name, class, reason string
		write               func(t *testing.T, path string)
	}{
		{"newer", "newer", "written by a newer version of manade", func(t *testing.T, path string) {
			writeStoreFile(t, path, `{"format":2,"build":"v9.0.0"}`, false)
		}},
		{"newerWithANamespaceItAdded", "newer", "written by a newer version of manade", func(t *testing.T, path string) {
			writeStoreFile(t, path, `{"format":2,"build":"v9.0.0"}`, true)
		}},
		{"format", "format", "written by another version of manade, or is not manade's file", func(t *testing.T, path string) {
			writeStoreFile(t, path, `{"format":1,"build":"v1.4.0"}`, true)
		}},
		{"damaged", "damaged", "its file is damaged", func(t *testing.T, path string) {
			if err := os.WriteFile(path, bytes.Repeat([]byte{0x5a}, 16<<10), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", "unreadable", "its file cannot be read", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "manade.db")
			tc.write(t, path)
			before := snapshotPath(t, path)
			t.Setenv("DATA_DIR", dir)
			t.Setenv("OPS_LISTEN_ADDR", "127.0.0.1:0")
			useSocket(t)
			draining, release := make(chan struct{}), make(chan struct{})
			old := drainHook
			drainHook = func() {
				close(draining)
				<-release
			}
			t.Cleanup(func() { drainHook = old })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			out := &syncBuffer{}
			code := make(chan int, 1)
			go func() { code <- serveControlPlane(ctx, out) }()
			waitFor(t, "the store's outage", out, "The control plane's store cannot open")
			m := outageOpsRe.FindStringSubmatch(out.String())
			if m == nil {
				t.Fatalf("the outage line names no ops address; log:\n%s", out.String())
			}
			for _, attr := range []string{"file=" + path, "class=" + tc.class, "error="} {
				if !strings.Contains(out.String(), attr) {
					t.Errorf("the outage line lacks %q; log:\n%s", attr, out.String())
				}
			}
			status, body := get(t, "http://"+m[1]+"/healthz")
			if status != http.StatusServiceUnavailable || !strings.Contains(body, tc.reason) || strings.Contains(body, "is starting") {
				t.Errorf("/healthz = %d %s, want 503 with %q alone", status, body, tc.reason)
			}
			if _, body := get(t, "http://"+m[1]+"/metrics"); !strings.Contains(body, "manade_booting 1\n") {
				t.Errorf("/metrics does not read booting:\n%s", body)
			}
			cancel()
			select {
			case <-draining:
			case <-time.After(10 * time.Second):
				t.Fatalf("the stop never read draining; log:\n%s", out.String())
			}
			status, body = get(t, "http://"+m[1]+"/healthz")
			close(release)
			if status != http.StatusServiceUnavailable || !strings.Contains(body, "the control plane is shutting down") || !strings.Contains(body, tc.reason) {
				t.Errorf("/healthz through the stop = %d %s, want 503 naming the shutdown and %q", status, body, tc.reason)
			}
			if c := <-code; c != 0 {
				t.Errorf("serveControlPlane = %d, want 0; log:\n%s", c, out.String())
			}
			if after := snapshotPath(t, path); !bytes.Equal(after, before) {
				t.Errorf("the outage changed the file: %d bytes, was %d", len(after), len(before))
			}
			lock, err := bolttable.LockPath(path, 0)
			if err != nil {
				t.Fatalf("the stop left the store's lock held: %v", err)
			}
			if err := lock.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// snapshotPath answers the file's bytes, or nil for a directory.
func snapshotPath(t *testing.T, path string) []byte {
	t.Helper()
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b.String()
}
