package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s is not PEM", path)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIssueUsagesAndNames(t *testing.T) {
	root := t.TempDir()
	made, err := Issue(root, []string{"nas", "default"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "ca"), filepath.Join(root, "orchestrator"), filepath.Join(root, "nas"), filepath.Join(root, "default")}
	if !slices.Equal(made, want) {
		t.Errorf("made = %v, want %v", made, want)
	}
	ca := readCert(t, filepath.Join(root, "ca", "ca.pem"))
	if !ca.IsCA || !ca.MaxPathLenZero || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("CA = IsCA %v, MaxPathLenZero %v, usage %v", ca.IsCA, ca.MaxPathLenZero, ca.KeyUsage)
	}
	for _, tc := range []struct {
		dir, file, name string
		usage           x509.ExtKeyUsage
	}{
		{"orchestrator", "orchestrator", hostapi.OrchestratorName, x509.ExtKeyUsageClientAuth},
		{"nas", "nas", "nas", x509.ExtKeyUsageServerAuth},
		{"default", "default", "default", x509.ExtKeyUsageServerAuth},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			c := readCert(t, filepath.Join(root, tc.dir, tc.file+".pem"))
			if !slices.Equal(c.ExtKeyUsage, []x509.ExtKeyUsage{tc.usage}) || !slices.Equal(c.DNSNames, []string{tc.name}) || c.IsCA {
				t.Errorf("usages %v names %v IsCA %v", c.ExtKeyUsage, c.DNSNames, c.IsCA)
			}
			if err := c.CheckSignatureFrom(ca); err != nil {
				t.Errorf("not signed by the CA: %v", err)
			}
			if c.NotAfter.After(ca.NotAfter) {
				t.Errorf("outlives the CA: %v after %v", c.NotAfter, ca.NotAfter)
			}
			if got := readCert(t, filepath.Join(root, tc.dir, "ca.pem")); !got.Equal(ca) {
				t.Error("the directory's ca.pem is not the pool's CA")
			}
			fi, err := os.Stat(filepath.Join(root, tc.dir, tc.file+"-key.pem"))
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("key mode %v, %v; want 0600", fi.Mode(), err)
			}
		})
	}
	fi, err := os.Stat(filepath.Join(root, "ca", "ca-key.pem"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("CA key mode %v, %v; want 0600", fi.Mode(), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 4 {
		t.Errorf("root holds %v: %v, want the four directories and no temporary one", entries, err)
	}
}

func TestIssueAddsAHostToAPool(t *testing.T) {
	root := t.TempDir()
	if _, err := Issue(root, []string{"nas"}, now); err != nil {
		t.Fatal(err)
	}
	orch, err := os.ReadFile(filepath.Join(root, "orchestrator", "orchestrator.pem"))
	if err != nil {
		t.Fatal(err)
	}
	made, err := Issue(root, []string{"edge"}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(made, []string{filepath.Join(root, "edge")}) {
		t.Errorf("made = %v, want only the new host", made)
	}
	again, err := os.ReadFile(filepath.Join(root, "orchestrator", "orchestrator.pem"))
	if err != nil || string(again) != string(orch) {
		t.Error("the orchestrator's certificate was issued again")
	}
	ca := readCert(t, filepath.Join(root, "ca", "ca.pem"))
	if err := readCert(t, filepath.Join(root, "edge", "edge.pem")).CheckSignatureFrom(ca); err != nil {
		t.Errorf("the new host is not signed by the pool's CA: %v", err)
	}
}

func TestIssueCapsALeafAtTheCA(t *testing.T) {
	root := t.TempDir()
	if _, err := Issue(root, nil, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(Validity - 24*time.Hour)
	if _, err := Issue(root, []string{"nas"}, later); err != nil {
		t.Fatal(err)
	}
	ca := readCert(t, filepath.Join(root, "ca", "ca.pem"))
	if leaf := readCert(t, filepath.Join(root, "nas", "nas.pem")); !leaf.NotAfter.Equal(ca.NotAfter) {
		t.Errorf("leaf until %v, CA until %v", leaf.NotAfter, ca.NotAfter)
	}
	if _, err := Issue(root, []string{"late"}, now.Add(Validity+time.Hour)); err == nil || !strings.Contains(err.Error(), "valid now") {
		t.Errorf("an expired CA issued: %v", err)
	}
}

func TestIssueRefusals(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, root string)
		want  string
		hosts []string
	}{
		"not a DNS label":  {hosts: []string{"Nas"}, want: "lowercase DNS label"},
		"an IP literal":    {hosts: []string{"10.0.0.1"}, want: "lowercase DNS label"},
		"a path":           {hosts: []string{"../x"}, want: "lowercase DNS label"},
		"the CA's name":    {hosts: []string{CADir}, want: "other than ca and orchestrator"},
		"the orchestrator": {hosts: []string{OrchestratorDir}, want: "other than ca and orchestrator"},
		"named twice":      {hosts: []string{"nas", "nas"}, want: "named twice"},
		"an existing host": {hosts: []string{"nas"}, want: "remove it to issue", setup: func(t *testing.T, root string) { mkdir(t, filepath.Join(root, "nas")) }},
		"a CA without key": {hosts: []string{"nas"}, want: "without its key", setup: func(t *testing.T, root string) {
			if _, err := Issue(root, nil, now); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "ca", "ca-key.pem")); err != nil {
				t.Fatal(err)
			}
		}},
		"an orchestrator without its CA": {hosts: []string{"nas"}, want: "restore ca", setup: func(t *testing.T, root string) {
			if _, err := Issue(root, nil, now); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(root, CADir)); err != nil {
				t.Fatal(err)
			}
		}},
		"an orchestrator from another CA": {hosts: []string{"nas"}, want: "is not from the CA in ca", setup: func(t *testing.T, root string) {
			other := t.TempDir()
			for _, r := range []string{root, other} {
				if _, err := Issue(r, nil, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(filepath.Join(root, CADir)); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(other, CADir), filepath.Join(root, CADir)); err != nil {
				t.Fatal(err)
			}
		}},
		"a ca directory without ca.pem": {hosts: []string{"nas"}, want: "holds no ca.pem", setup: func(t *testing.T, root string) { mkdir(t, filepath.Join(root, "ca")) }},
		"a mismatched CA key": {hosts: []string{"nas"}, want: "does not match", setup: func(t *testing.T, root string) {
			other := t.TempDir()
			for _, r := range []string{root, other} {
				if _, err := Issue(r, nil, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(filepath.Join(other, "ca", "ca-key.pem"), filepath.Join(root, "ca", "ca-key.pem")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, root)
			}
			before, _ := os.ReadDir(root)
			_, err := Issue(root, tc.hosts, now)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			after, _ := os.ReadDir(root)
			if len(after) != len(before) {
				t.Errorf("the refusal wrote %v (before %v)", after, before)
			}
		})
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// serveHost serves a host API with root's files for host name.
func serveHost(t *testing.T, root, name string) string {
	t.Helper()
	cfg, err := hostapi.ServerTLS(filepath.Join(root, name), name)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := hostapi.ListenTLS(t.Context(), "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// handshake dials addr with cfg and completes the TLS handshake.
func handshake(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	d := tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	// TLS 1.3 sends the server's verdict on the client certificate after the
	// client's handshake ends, so a read is what surfaces it.
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = conn.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return nil
	}
	return err
}

func TestIssuedFilesWorkOnlyInTheirRole(t *testing.T) {
	root := t.TempDir()
	if _, err := Issue(root, []string{"nas", "edge"}, now); err != nil {
		t.Fatal(err)
	}
	addr := serveHost(t, root, "nas")
	orch, err := hostapi.ClientTLS(filepath.Join(root, "orchestrator"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the orchestrator reaches its host", func(t *testing.T) {
		c, err := hostapi.NewClient("nas", config.Address{HostPort: addr}, orch)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseIdleConnections()
		_, err = c.Status(t.Context())
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("status = %v, want the handler's 404 after a good handshake", err)
		}
	})
	t.Run("a host certificate is refused as a client", func(t *testing.T) {
		edge, err := tls.LoadX509KeyPair(filepath.Join(root, "edge", "edge.pem"), filepath.Join(root, "edge", "edge-key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := orch.Clone()
		cfg.ServerName, cfg.Certificates = "nas", []tls.Certificate{edge}
		if err := handshake(t, addr, cfg); err == nil {
			t.Error("the host accepted another host's certificate as the orchestrator")
		}
	})
	t.Run("the orchestrator's certificate is refused as a server", func(t *testing.T) {
		pair, err := tls.LoadX509KeyPair(filepath.Join(root, "orchestrator", "orchestrator.pem"), filepath.Join(root, "orchestrator", "orchestrator-key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					if tc, ok := conn.(*tls.Conn); ok {
						_ = tc.Handshake()
					}
					_ = conn.Close()
				}()
			}
		}()
		cfg2 := orch.Clone()
		cfg2.ServerName = hostapi.OrchestratorName
		if err := handshake(t, ln.Addr().String(), cfg2); err == nil {
			t.Errorf("the client accepted the orchestrator's certificate as a host: %v", err)
		}
	})
}

func TestWriteDirLeavesNothingOnFailure(t *testing.T) {
	root := t.TempDir()
	taken := filepath.Join(root, "nas")
	mkdir(t, taken)
	if err := os.WriteFile(filepath.Join(taken, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDir(taken, 0o755, map[string]file{"a.pem": {data: []byte("x"), mode: 0o644}}); err == nil {
		t.Fatal("writeDir replaced a directory that holds files")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "nas" {
		t.Errorf("root holds %v: %v, want only the directory that was there", entries, err)
	}
}
