package hostapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
)

// unixClient serves b on a Unix socket and answers a client for it.
func unixClient(t *testing.T, b *fakeBackend) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenUnix(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(b, "inst-1", nil).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	c, err := NewClient("nas", config.Address{Socket: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func TestClientRoundTrips(t *testing.T) {
	b := &fakeBackend{
		status:  Status{Version: "v1.0.0-dev.11", ProbeDeadline: time.Minute},
		runners: []Runner{{Name: "r1", State: StateFailed, Reason: "create refused", Orphans: []string{"c9"}}},
		logs:    []byte("one\ntwo\n"),
		diag:    []byte("tar"),
		changes: make(chan Runner),
	}
	c := unixClient(t, b)

	t.Run("status", func(t *testing.T) {
		st, err := c.Status(t.Context())
		if err != nil || st.InstanceID != "inst-1" || st.Version != "v1.0.0-dev.11" || st.ProbeDeadline != time.Minute {
			t.Errorf("status = %+v, %v", st, err)
		}
	})
	t.Run("create", func(t *testing.T) {
		req := validRequest()
		a, err := c.CreateRunner(t.Context(), &req)
		if err != nil || a.InstanceID != "inst-1" || a.Runner.Name != req.RunnerName || a.Runner.RunnerID != req.RunnerID {
			t.Errorf("answer = %+v, %v", a, err)
		}
	})
	t.Run("list", func(t *testing.T) {
		l, err := c.Runners(t.Context())
		if err != nil || l.InstanceID != "inst-1" || len(l.Runners) != 1 || l.Runners[0].Orphans[0] != "c9" {
			t.Errorf("list = %+v, %v", l, err)
		}
	})
	t.Run("delete without diag", func(t *testing.T) {
		got, err := c.DeleteRunner(t.Context(), "r1", false)
		if err != nil || got != nil || b.diagAsk {
			t.Errorf("delete = %q, %v (diag asked %v)", got, err, b.diagAsk)
		}
	})
	t.Run("delete with diag", func(t *testing.T) {
		got, err := c.DeleteRunner(t.Context(), "r1", true)
		if err != nil || string(got) != "tar" || !b.diagAsk {
			t.Errorf("delete = %q, %v (diag asked %v)", got, err, b.diagAsk)
		}
	})
	t.Run("logs", func(t *testing.T) {
		got, err := c.Logs(t.Context(), "r1", 2)
		if err != nil || string(got) != "one\ntwo\n" {
			t.Errorf("logs = %q, %v", got, err)
		}
	})
	t.Run("diag", func(t *testing.T) {
		got, err := c.Diag(t.Context(), "r1")
		if err != nil || string(got) != "tar" {
			t.Errorf("diag = %q, %v", got, err)
		}
	})
	t.Run("probe", func(t *testing.T) {
		rep, err := c.Probe(t.Context(), time.Minute)
		if err != nil || !rep.Proven || rep.InstanceID != "inst-1" {
			t.Errorf("probe = %+v, %v", rep, err)
		}
	})
	t.Run("events", func(t *testing.T) {
		got := make(chan *Event, 8)
		done := make(chan error, 1)
		go func() { done <- c.Events(t.Context(), func(ev *Event) { got <- ev }) }()
		if ev := <-got; ev.Type != EventHeartbeat || ev.InstanceID != "inst-1" {
			t.Errorf("first event = %+v", ev)
		}
		b.changes <- Runner{Name: "r1", State: StateRunning}
		if ev := <-got; ev.Type != EventRunner || ev.Runner == nil || ev.Runner.Name != "r1" {
			t.Errorf("event = %+v", ev)
		}
		close(b.changes)
		if err := <-done; err != nil {
			t.Errorf("Events after the host ended the stream: %v", err)
		}
	})
}

func TestClientTypedErrors(t *testing.T) {
	req := validRequest()
	t.Run("not ready", func(t *testing.T) {
		c := unixClient(t, &fakeBackend{err: &NotReadyError{Profile: "small", Reason: ReasonRefused, Detail: "203.0.113.9 outside allow"}})
		_, err := c.CreateRunner(t.Context(), &req)
		nr, ok := errors.AsType[*NotReadyError](err)
		if !ok || nr.Profile != "small" || nr.Reason != ReasonRefused || nr.Detail != "203.0.113.9 outside allow" {
			t.Errorf("err = %#v", err)
		}
	})
	t.Run("not ready on one line", func(t *testing.T) {
		c := unixClient(t, &fakeBackend{err: &NotReadyError{Profile: "small\nlevel=ERROR", Reason: "pulling\r\nforged", Detail: "x"}})
		_, err := c.CreateRunner(t.Context(), &req)
		nr, ok := errors.AsType[*NotReadyError](err)
		if !ok || strings.ContainsAny(err.Error(), "\r\n") || strings.ContainsAny(nr.Profile+string(nr.Reason), "\r\n") || !strings.Contains(nr.Profile, "small") {
			t.Errorf("err = %q (%#v), want one line", err, err)
		}
	})
	for name, sentinel := range map[string]error{"unknown profile": ErrUnknownProfile, "refused profile": ErrProfileRefused} {
		t.Run(name, func(t *testing.T) {
			c := unixClient(t, &fakeBackend{err: sentinel})
			if _, err := c.CreateRunner(t.Context(), &req); !errors.Is(err, sentinel) {
				t.Errorf("err = %v, want %v", err, sentinel)
			}
		})
	}
	t.Run("unknown runner", func(t *testing.T) {
		c := unixClient(t, &fakeBackend{err: ErrUnknownRunner})
		if _, err := c.Logs(t.Context(), "r1", 5); !errors.Is(err, ErrUnknownRunner) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("other failures", func(t *testing.T) {
		c := unixClient(t, &fakeBackend{err: errors.New("daemon gone")})
		_, err := c.Runners(t.Context())
		e, ok := errors.AsType[*APIError](err)
		if !ok || e.Status != http.StatusInternalServerError || e.Code != codeInternal || !strings.Contains(e.Message, "daemon gone") {
			t.Errorf("err = %#v", err)
		}
	})
	t.Run("probe past its deadline", func(t *testing.T) {
		c := unixClient(t, &fakeBackend{deadline: 50 * time.Millisecond, probe: func(ctx context.Context) (ProbeReport, error) {
			<-ctx.Done()
			return ProbeReport{}, ctx.Err()
		}})
		if _, err := c.Probe(t.Context(), 50*time.Millisecond); !errors.Is(err, ErrProbeTimeout) {
			t.Errorf("err = %v", err)
		}
	})
	// silentHost is a client of a host that accepts and never answers.
	silentHost := func(t *testing.T) *Client {
		t.Helper()
		path := filepath.Join(t.TempDir(), "api.sock")
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "unix", path)
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
				t.Cleanup(func() { _ = conn.Close() })
			}
		}()
		c, err := NewClient("nas", config.Address{Socket: path}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("a host that never answers its probe", func(t *testing.T) {
		saved := clientSlack
		clientSlack = 20 * time.Millisecond
		t.Cleanup(func() { clientSlack = saved })
		if _, err := silentHost(t).Probe(t.Context(), 30*time.Millisecond); !errors.Is(err, ErrProbeTimeout) || !strings.Contains(err.Error(), "30ms") {
			t.Errorf("err = %v, want the probe timeout named", err)
		}
	})
	t.Run("a caller deadline before the probe's is the caller's", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if _, err := silentHost(t).Probe(ctx, time.Minute); errors.Is(err, ErrProbeTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the caller's deadline, not the probe timeout", err)
		}
	})
	t.Run("probe with no deadline", func(t *testing.T) {
		c := unixClient(t, &fakeBackend{})
		if _, err := c.Probe(t.Context(), 0); err == nil {
			t.Error("a probe with no deadline was sent")
		}
	})
}

func TestNewClientNeedsTLSForHTTPS(t *testing.T) {
	if _, err := NewClient("nas", config.Address{HostPort: "nas.example:7464"}, nil); err == nil {
		t.Error("an https address without TLS files made a client")
	}
	if _, err := NewClient("nas", config.Address{}, nil); err == nil {
		t.Error("no address made a client")
	}
}

// tlsPair serves host "nas" from hostDir and answers its port and the
// orchestrator's ClientTLS from orchDir.
func tlsPair(t *testing.T, ca *testCA) (port string, client *tls.Config) {
	t.Helper()
	hostDir, orchDir := t.TempDir(), t.TempDir()
	ca.issue(t, hostDir, "nas", "nas", x509.ExtKeyUsageServerAuth)
	ca.issue(t, orchDir, OrchestratorFiles, OrchestratorName, x509.ExtKeyUsageClientAuth)
	addr := tlsServer(t, hostDir)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	client, err = ClientTLS(orchDir)
	if err != nil {
		t.Fatal(err)
	}
	return port, client
}

func TestClientTLS(t *testing.T) {
	ca := newTestCA(t)
	port, cfg := tlsPair(t, ca)
	status := func(t *testing.T, name, hostPort string, cfg *tls.Config) error {
		t.Helper()
		c, err := NewClient(name, config.Address{HostPort: hostPort}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseIdleConnections()
		_, err = c.Status(t.Context())
		return err
	}
	t.Run("the host's name, whatever the address", func(t *testing.T) {
		for _, h := range []string{"127.0.0.1", "localhost"} {
			if err := status(t, "nas", net.JoinHostPort(h, port), cfg); err != nil {
				t.Errorf("at %s: %v", h, err)
			}
		}
	})
	t.Run("another host's certificate at the right address", func(t *testing.T) {
		if err := status(t, "default", net.JoinHostPort("127.0.0.1", port), cfg); err == nil {
			t.Error("the client accepted nas's certificate as default's")
		}
	})
	t.Run("a host certificate without server usage", func(t *testing.T) {
		dir := t.TempDir()
		ca.issue(t, dir, "nas", "nas")
		cert := loadCert(t, dir, "nas")
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*cert},
			ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
		})
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: NewServer(&fakeBackend{}, "inst-1", nil).Handler(), ReadHeaderTimeout: quickTimeout}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		if err := status(t, "nas", ln.Addr().String(), cfg); err == nil || !strings.Contains(err.Error(), "server authentication") {
			t.Errorf("err = %v, want the usage refused", err)
		}
	})
	t.Run("a host certificate from another CA", func(t *testing.T) {
		otherPort, _ := tlsPair(t, newTestCA(t))
		if err := status(t, "nas", net.JoinHostPort("127.0.0.1", otherPort), cfg); err == nil {
			t.Error("the client accepted another CA's host")
		}
	})
}

func TestClientTLSLoadChecksItsOwnFiles(t *testing.T) {
	ca := newTestCA(t)
	t.Run("a server certificate as the orchestrator's", func(t *testing.T) {
		dir := t.TempDir()
		ca.issue(t, dir, OrchestratorFiles, OrchestratorName, x509.ExtKeyUsageServerAuth)
		if _, err := ClientTLS(dir); err == nil || !strings.Contains(err.Error(), "client authentication") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("another name", func(t *testing.T) {
		dir := t.TempDir()
		ca.issue(t, dir, OrchestratorFiles, "nas", x509.ExtKeyUsageClientAuth)
		if _, err := ClientTLS(dir); err == nil || !strings.Contains(err.Error(), OrchestratorName) {
			t.Errorf("err = %v", err)
		}
	})
}
