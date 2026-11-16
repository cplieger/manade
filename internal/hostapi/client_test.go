package hostapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// h2cConn answers a client over one prior-knowledge HTTP/2 connection to
// addr, the shape of a host's link.
func h2cConn(t *testing.T, addr string) *Client {
	t.Helper()
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &p}
	cc, err := tr.NewClientConn(t.Context(), "http", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return NewClient(cc)
}

// linkClient serves b over HTTP/2 and answers a client for it.
func linkClient(t *testing.T, b *fakeBackend) *Client {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: NewServer(b, "inst-1", nil).Handler(), Protocols: &p, ReadHeaderTimeout: quickTimeout}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return h2cConn(t, ln.Addr().String())
}

func TestClientRoundTrips(t *testing.T) {
	b := &fakeBackend{
		status:  Status{Version: "v1.0.0-dev.11", ProbeDeadline: time.Minute},
		runners: []Runner{{Name: "r1", State: StateFailed, Reason: "create refused", Orphans: []string{"c9"}}},
		logs:    []byte("one\ntwo\n"),
		diag:    []byte("tar"),
		changes: make(chan Runner),
	}
	c := linkClient(t, b)

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
		c := linkClient(t, &fakeBackend{err: &NotReadyError{Profile: "small", Reason: ReasonRefused, Detail: "203.0.113.9 outside allow"}})
		_, err := c.CreateRunner(t.Context(), &req)
		nr, ok := errors.AsType[*NotReadyError](err)
		if !ok || nr.Profile != "small" || nr.Reason != ReasonRefused || nr.Detail != "203.0.113.9 outside allow" {
			t.Errorf("err = %#v", err)
		}
	})
	t.Run("not ready on one line", func(t *testing.T) {
		c := linkClient(t, &fakeBackend{err: &NotReadyError{Profile: "small\nlevel=ERROR", Reason: "pulling\r\nforged", Detail: "x"}})
		_, err := c.CreateRunner(t.Context(), &req)
		nr, ok := errors.AsType[*NotReadyError](err)
		if !ok || strings.ContainsAny(err.Error(), "\r\n") || strings.ContainsAny(nr.Profile+string(nr.Reason), "\r\n") || !strings.Contains(nr.Profile, "small") {
			t.Errorf("err = %q (%#v), want one line", err, err)
		}
	})
	for name, sentinel := range map[string]error{"unknown profile": ErrUnknownProfile, "refused profile": ErrProfileRefused} {
		t.Run(name, func(t *testing.T) {
			c := linkClient(t, &fakeBackend{err: sentinel})
			if _, err := c.CreateRunner(t.Context(), &req); !errors.Is(err, sentinel) {
				t.Errorf("err = %v, want %v", err, sentinel)
			}
		})
	}
	t.Run("unknown runner", func(t *testing.T) {
		c := linkClient(t, &fakeBackend{err: ErrUnknownRunner})
		if _, err := c.Logs(t.Context(), "r1", 5); !errors.Is(err, ErrUnknownRunner) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("other failures", func(t *testing.T) {
		c := linkClient(t, &fakeBackend{err: errors.New("daemon gone")})
		_, err := c.Runners(t.Context())
		e, ok := errors.AsType[*APIError](err)
		if !ok || e.Status != http.StatusInternalServerError || e.Code != codeInternal || !strings.Contains(e.Message, "daemon gone") {
			t.Errorf("err = %#v", err)
		}
	})
	t.Run("probe past its deadline", func(t *testing.T) {
		c := linkClient(t, &fakeBackend{deadline: 50 * time.Millisecond, probe: func(ctx context.Context) (ProbeReport, error) {
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
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
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
		return h2cConn(t, ln.Addr().String())
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
		c := linkClient(t, &fakeBackend{})
		if _, err := c.Probe(t.Context(), 0); err == nil {
			t.Error("a probe with no deadline was sent")
		}
	})
}
