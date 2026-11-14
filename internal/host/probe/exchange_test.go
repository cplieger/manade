package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/egress"
)

func TestClassify(t *testing.T) {
	op := func(errno syscall.Errno) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
	}
	live := context.Background()
	cut, cancel := context.WithCancel(live)
	cancel()
	for _, tc := range []struct {
		ctx  context.Context
		err  error
		want egress.Outcome
	}{
		{live, op(syscall.ECONNREFUSED), egress.OutcomeRefused},
		{live, op(syscall.ECONNRESET), egress.OutcomeReset},
		{live, op(syscall.ENETUNREACH), egress.OutcomeNoRoute},
		{live, op(syscall.EHOSTUNREACH), egress.OutcomeNoRoute},
		{live, op(syscall.EADDRNOTAVAIL), egress.OutcomeNoRoute},
		{live, op(syscall.EAFNOSUPPORT), egress.OutcomeNoRoute},
		{live, &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, egress.OutcomeTimeout},
		{cut, op(syscall.ECONNREFUSED), egress.OutcomeTimeout},
		{live, errors.New("tls: bad"), egress.OutcomeError},
	} {
		if got := classify(tc.ctx, tc.err); got != tc.want {
			t.Errorf("classify(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}

func TestDoOnLoopback(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if got := do(t.Context(), egress.KindReach, addr, time.Second); got != egress.OutcomeConnected {
		t.Errorf("a listening port = %s", got)
	}
	_ = ln.Close()
	if got := do(t.Context(), egress.KindReach, addr, time.Second); got != egress.OutcomeRefused {
		t.Errorf("a closed port = %s", got)
	}
}

func TestServe(t *testing.T) {
	old := doCheck
	t.Cleanup(func() { doCheck = old })
	var asked []string
	doCheck = func(_ context.Context, k egress.Kind, addr string, timeout time.Duration) egress.Outcome {
		asked = append(asked, fmt.Sprint(k, addr, timeout))
		return egress.OutcomeTimeout
	}
	t.Run("one outcome line per check line", func(t *testing.T) {
		var out bytes.Buffer
		in := `{"kind":1,"addr":"10.0.0.1:1","timeout_ns":3000000000}` + "\n" + `{"kind":3,"addr":"api.github.com","timeout_ns":5000000000}` + "\n"
		if err := Serve(t.Context(), strings.NewReader(in), &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != `{"outcome":3}`+"\n"+`{"outcome":3}`+"\n" || len(asked) != 2 || asked[1] != "3api.github.com5s" {
			t.Errorf("out %q, asked %q", out.String(), asked)
		}
	})
	t.Run("a line that is no check ends it", func(t *testing.T) {
		if err := Serve(t.Context(), strings.NewReader("hello\n"), &bytes.Buffer{}); err == nil {
			t.Error("Serve took a line that is no check")
		}
	})
}

func TestListen(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Listen(ctx) }()
	addr := fmt.Sprintf("127.0.0.1:%d", listenerPort)
	var got egress.Outcome
	for range 100 {
		if got = do(t.Context(), egress.KindReach, addr, time.Second); got == egress.OutcomeConnected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != egress.OutcomeConnected {
		t.Errorf("the listener = %s", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Listen = %v after its context ended", err)
	}
}
