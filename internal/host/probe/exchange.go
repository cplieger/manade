// Package probe runs a runner host's egress probe: the job leg in a probe
// container on the job network, stepped one check per line over its
// attached standard input and output, and the output leg from the host's
// own stack, each block check counted by the rule set's probe counters.
package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/netsetup"
)

// listenerPort is the port the listener container accepts on, standing in
// for a job's own server.
const listenerPort = 8471

// lineLimit caps one exchange line.
const lineLimit = 4 << 10

// request is one line the host writes to the probe container.
type request struct {
	Addr    string        `json:"addr"`
	Timeout time.Duration `json:"timeout_ns"`
	Kind    egress.Kind   `json:"kind"`
}

// answer is the probe container's line back.
type answer struct {
	Outcome egress.Outcome `json:"outcome"`
}

// doCheck and outputCheck make a check from this process's own stack; test
// seams for a network the test does not have.
var (
	doCheck     = do
	outputCheck = output
)

// output runs output-leg check c from the host's own stack. Its lookup asks
// each of dns once, never resolv.conf's timeouts and attempts.
func output(ctx context.Context, c *egress.Check, dns []netip.Addr) egress.Outcome {
	if c.Kind != egress.KindResolve {
		return doCheck(ctx, c.Kind, c.Addr, c.Timeout)
	}
	for _, d := range dns {
		if addrs, err := netsetup.LookupVia(ctx, d, c.Addr); err == nil && len(addrs) > 0 {
			return egress.OutcomeResolved
		}
	}
	return egress.OutcomeLookupFailed
}

// do makes one check from this process's own stack: a connection to addr,
// or a lookup of it through the system's resolver, within timeout.
func do(ctx context.Context, kind egress.Kind, addr string, timeout time.Duration) egress.Outcome {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if kind == egress.KindResolve {
		r := &net.Resolver{PreferGo: true}
		if addrs, err := r.LookupHost(ctx, addr); err != nil || len(addrs) == 0 {
			return egress.OutcomeLookupFailed
		}
		return egress.OutcomeResolved
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return egress.OutcomeConnected
	}
	return classify(ctx, err)
}

// classify maps a failed dial to its outcome. A dial the context cut is a
// timeout: a dropped connection never answers.
func classify(ctx context.Context, err error) egress.Outcome {
	var ne net.Error
	switch {
	case ctx.Err() != nil, errors.As(err, &ne) && ne.Timeout():
		return egress.OutcomeTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return egress.OutcomeRefused
	case errors.Is(err, syscall.ECONNRESET):
		return egress.OutcomeReset
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.EADDRNOTAVAIL),
		errors.Is(err, syscall.EAFNOSUPPORT):
		return egress.OutcomeNoRoute
	}
	return egress.OutcomeError
}

// Serve is the probe container's side: it answers each request line read
// from in with one outcome line on out, until in ends.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, lineLimit), lineLimit)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			return fmt.Errorf("probe: reading a check: %w", err)
		}
		if err := enc.Encode(answer{Outcome: doCheck(ctx, req.Kind, req.Addr, req.Timeout)}); err != nil {
			return fmt.Errorf("probe: answering a check: %w", err)
		}
	}
	return sc.Err()
}

// Listen is the listener container's side: it accepts and closes every
// connection on listenerPort, IPv4 and IPv6, until ctx ends.
func Listen(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", fmt.Sprintf(":%d", listenerPort))
	if err != nil {
		return fmt.Errorf("probe listener: %w", err)
	}
	context.AfterFunc(ctx, func() { _ = ln.Close() })
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("probe listener: %w", err)
		}
		_ = conn.Close()
	}
}

// exchanger is the host's side of the attached stream.
type exchanger struct {
	stream deadlineRW
	sc     *bufio.Scanner
}

// deadlineRW is an attached stream: docker.Stream in production.
type deadlineRW interface {
	io.ReadWriter
	SetDeadline(t time.Time) error
}

func newExchanger(s deadlineRW) *exchanger {
	sc := bufio.NewScanner(s)
	sc.Buffer(make([]byte, 0, lineLimit), lineLimit)
	return &exchanger{stream: s, sc: sc}
}

// errNoOutcome is an exchange with no outcome line within its bound.
var errNoOutcome = errors.New("no outcome line")

// exchange asks the probe container for check c and reads its outcome,
// all within c.Exchange().
func (x *exchanger) exchange(c *egress.Check) (egress.Outcome, error) {
	if err := x.stream.SetDeadline(time.Now().Add(c.Exchange())); err != nil {
		return 0, err
	}
	b, err := json.Marshal(request{Kind: c.Kind, Addr: c.Addr, Timeout: c.Timeout})
	if err != nil {
		return 0, err
	}
	if _, err := x.stream.Write(append(b, '\n')); err != nil {
		return 0, fmt.Errorf("%w: writing the check: %w", errNoOutcome, err)
	}
	if !x.sc.Scan() {
		return 0, fmt.Errorf("%w within %s: %w", errNoOutcome, c.Exchange(), cmpErr(x.sc.Err()))
	}
	var a answer
	if err := json.Unmarshal(x.sc.Bytes(), &a); err != nil {
		return 0, fmt.Errorf("%w: %w", errNoOutcome, err)
	}
	return a.Outcome, nil
}

func cmpErr(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}
