package hostapi

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// tlsServer serves a fake backend as host "nas" with dir's TLS files.
func tlsServer(t *testing.T, dir string) string {
	t.Helper()
	cfg, err := ServerTLS(dir, "nas")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ListenTLS(t.Context(), "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: NewServer(&fakeBackend{}, "inst-1", nil).Handler(), ReadHeaderTimeout: quickTimeout}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// get calls GET /v1/status at addr as a client presenting cert (nil for
// none), trusting roots and expecting the server to be "nas".
func get(t *testing.T, addr string, roots *x509.CertPool, cert *tls.Certificate) error {
	t.Helper()
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "nas"}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/v1/status", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func loadCert(t *testing.T, dir, file string) *tls.Certificate {
	t.Helper()
	c, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile(file)), filepath.Join(dir, KeyFile(file)))
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

func TestServerTLSRefusals(t *testing.T) {
	ca := newTestCA(t)
	hostDir, clients := t.TempDir(), t.TempDir()
	ca.issue(t, hostDir, "nas", "nas", x509.ExtKeyUsageServerAuth)
	ca.issue(t, clients, "good", OrchestratorName, x509.ExtKeyUsageClientAuth)
	ca.issue(t, clients, "host-as-client", "other", x509.ExtKeyUsageServerAuth)
	ca.issue(t, clients, "server-usage", OrchestratorName, x509.ExtKeyUsageServerAuth)
	ca.issue(t, clients, "no-usage", OrchestratorName)
	ca.issue(t, clients, "wrong-name", "someone", x509.ExtKeyUsageClientAuth)
	other := newTestCA(t)
	otherDir := t.TempDir()
	other.issue(t, otherDir, "foreign", OrchestratorName, x509.ExtKeyUsageClientAuth)

	addr := tlsServer(t, hostDir)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)

	if err := get(t, addr, roots, loadCert(t, clients, "good")); err != nil {
		t.Fatalf("the orchestrator's certificate was refused: %v", err)
	}
	refused := map[string]*tls.Certificate{
		"no certificate":                nil,
		"another CA":                    loadCert(t, otherDir, "foreign"),
		"a host certificate as client":  loadCert(t, clients, "host-as-client"),
		"server usage only":             loadCert(t, clients, "server-usage"),
		"no extended key usage":         loadCert(t, clients, "no-usage"),
		"client usage for another name": loadCert(t, clients, "wrong-name"),
	}
	for name, cert := range refused {
		t.Run(name, func(t *testing.T) {
			if err := get(t, addr, roots, cert); err == nil {
				t.Error("the host answered")
			}
		})
	}
}

func TestServerTLSLoadChecksItsOwnFiles(t *testing.T) {
	ca := newTestCA(t)
	t.Run("a client certificate as the host's", func(t *testing.T) {
		dir := t.TempDir()
		ca.issue(t, dir, "nas", "nas", x509.ExtKeyUsageClientAuth)
		if _, err := ServerTLS(dir, "nas"); err == nil || !strings.Contains(err.Error(), "server authentication") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("another host's name", func(t *testing.T) {
		dir := t.TempDir()
		ca.issue(t, dir, "nas", "default", x509.ExtKeyUsageServerAuth)
		if _, err := ServerTLS(dir, "nas"); err == nil || !strings.Contains(err.Error(), `"nas"`) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("signed by another CA than ca.pem", func(t *testing.T) {
		dir := t.TempDir()
		ca.issue(t, dir, "nas", "nas", x509.ExtKeyUsageServerAuth)
		if err := os.WriteFile(filepath.Join(dir, CAFile), newTestCA(t).pem, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ServerTLS(dir, "nas"); err == nil || !strings.Contains(err.Error(), "not signed by") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing files name TLS_DIR", func(t *testing.T) {
		if _, err := ServerTLS(t.TempDir(), "nas"); err == nil || !strings.Contains(err.Error(), "TLS_DIR") {
			t.Errorf("err = %v", err)
		}
	})
	for _, name := range []string{"ca", "orchestrator"} {
		t.Run("a host named "+name, func(t *testing.T) {
			if _, err := ServerTLS(t.TempDir(), name); err == nil || !strings.Contains(err.Error(), "other than ca and orchestrator") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestListenUnix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts", "nas", "api.sock")
	ln, err := ListenUnix(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 || !ok || int(st.Uid) != os.Getuid() {
		t.Errorf("socket mode %v uid %v, want a 0600 socket owned by %d", fi.Mode(), st.Uid, os.Getuid())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Errorf("socket directory holds %v: %v", entries, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket left after Close: %v", err)
	}
}

func TestListenUnixBindsPrivately(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	var bindDir os.FileMode
	orig := listen
	t.Cleanup(func() { listen = orig })
	listen = func(addr string) (*net.UnixListener, error) {
		if fi, err := os.Stat(filepath.Dir(addr)); err == nil {
			bindDir = fi.Mode().Perm()
		}
		return orig(addr)
	}
	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenUnix(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if bindDir != 0o700 {
		t.Errorf("bound in a directory of mode %v, want 0700 under umask 0", bindDir)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket = %v, %v, want mode 0600", fi, err)
	}
}

func TestListenUnixLeavesNothingOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.sock")
	// A non-empty directory at path fails the rename, as root too.
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenUnix(path, os.Getuid()); err == nil {
		t.Fatal("ListenUnix renamed a socket onto a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "api.sock" {
		t.Errorf("socket directory holds %v: %v, want only what was there", entries, err)
	}
}

func TestListenUnixCloseKeepsASuccessor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	first, err := ListenUnix(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	second, err := ListenUnix(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	go func() {
		if c, err := second.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial after the first Close: %v, want the second listener", err)
	}
	_ = c.Close()
}
