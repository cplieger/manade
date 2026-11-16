package hostlink

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The host's redial backoff: minBackoff doubling to maxBackoff, back to
// minBackoff after a connection that lasted resetAfter.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	resetAfter = time.Minute
)

// backoff is the wait before each redial.
type backoff struct {
	min, max, reset time.Duration
	next            time.Duration
}

// after answers the wait before the next dial, given how long the last
// connection lasted (zero for none).
func (b *backoff) after(lasted time.Duration) time.Duration {
	if b.next == 0 || lasted >= b.reset {
		b.next = b.min
	}
	d := b.next
	b.next = min(b.next*2, b.max)
	return d
}

// UplinkOptions are what a host's side of the channel runs with.
type UplinkOptions struct {
	// Dial opens a connection to the control plane.
	Dial func(ctx context.Context) (net.Conn, error)
	// Hello is called before each upgrade, so each names the section the
	// host holds at that moment.
	Hello func() Hello
	// Handler is the host API, served on each connection.
	Handler http.Handler
	Log     *slog.Logger
}

// UplinkState is the host's channel as its status reports it.
type UplinkState struct {
	// Since is when the channel last connected or last failed.
	Since time.Time
	// Err is the last dial failure, refusal or close; a *RefusedError for a
	// refusal.
	Err       error
	Connected bool
}

// Uplink is a host's side of the channel: it dials the control plane, keeps
// one connection open and redials after any close.
type Uplink struct {
	opts  *UplinkOptions
	state atomic.Pointer[UplinkState]
	// ping, handshake and wait are pingTimeout, handshakeTimeout and the
	// redial backoff, test seams.
	ping      time.Duration
	handshake time.Duration
	wait      backoff
}

// NewUplink makes a host's side of the channel; Run runs it.
func NewUplink(o *UplinkOptions) *Uplink {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	u := &Uplink{opts: o, ping: pingTimeout, handshake: handshakeTimeout, wait: backoff{min: minBackoff, max: maxBackoff, reset: resetAfter}}
	u.state.Store(&UplinkState{})
	return u
}

// State answers the channel's state now.
func (u *Uplink) State() UplinkState { return *u.state.Load() }

// Run keeps the channel open until ctx ends, then closes it and returns once
// its server has stopped.
func (u *Uplink) Run(ctx context.Context) {
	for {
		lasted, err := u.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		u.state.Store(&UplinkState{Since: time.Now(), Err: err})
		wait := u.wait.after(lasted)
		if refused, ok := errors.AsType[*RefusedError](err); ok {
			u.opts.Log.Warn("The control plane refused the host's connection; the host dials again", "code", refused.Code, "error", err, "wait", wait)
		} else if lasted > 0 {
			u.opts.Log.Info("The host's connection to the control plane closed; the host dials again", "error", err, "connected_for", lasted.Round(time.Second), "wait", wait)
		} else {
			u.opts.Log.Warn("The host could not reach the control plane; it dials again", "error", err, "wait", wait)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// errClosed is a connection that ended: the control plane closed it, or a
// ping went unanswered.
var errClosed = errors.New("the connection to the control plane closed")

// connect dials, upgrades and serves one connection until it closes,
// answering how long it was connected.
func (u *Uplink) connect(ctx context.Context) (time.Duration, error) {
	dctx, cancel := context.WithTimeout(ctx, u.handshake)
	conn, err := u.opts.Dial(dctx)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("dialing the control plane: %w", err)
	}
	hello := u.opts.Hello()
	br, err := u.upgrade(ctx, conn, &hello)
	if err != nil {
		_ = conn.Close()
		return 0, err
	}
	since := time.Now()
	u.state.Store(&UplinkState{Since: since, Connected: true})
	u.opts.Log.Info("The host is connected to the control plane", "host", hello.Name, "section", hello.Section)
	u.serve(ctx, &bufferedConn{Conn: conn, r: br})
	return time.Since(since), errClosed
}

// upgrade sends the upgrade request and reads its answer, answering the
// reader past the 101, which may already hold the control plane's first
// HTTP/2 frames.
func (u *Uplink) upgrade(ctx context.Context, conn net.Conn, hello *Hello) (*bufio.Reader, error) {
	if err := conn.SetDeadline(time.Now().Add(u.handshake)); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://manade"+connectPath, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", Protocol)
	hello.header(req.Header)
	if werr := req.Write(conn); werr != nil {
		return nil, fmt.Errorf("the upgrade request: %w", werr)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("the upgrade's answer: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, refusalLimit))
		_ = resp.Body.Close()
		return nil, refusedFrom(resp.StatusCode, body)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), Protocol) {
		return nil, fmt.Errorf("the control plane switched to %q, not %s", resp.Header.Get("Upgrade"), Protocol)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return br, nil
}

// serve answers the host API on conn until it closes or ctx ends.
func (u *Uplink) serve(ctx context.Context, conn *bufferedConn) {
	ln := newOneConn(conn)
	var h2c http.Protocols
	h2c.SetUnencryptedHTTP2(true)
	// No ReadTimeout or IdleTimeout: an HTTP/2 server's idle timeout falls
	// back to ReadTimeout and would close the channel between calls. The
	// host API bounds each request itself; HTTP/2 ignores ReadHeaderTimeout.
	srv := &http.Server{
		Handler:           u.opts.Handler,
		Protocols:         &h2c,
		HTTP2:             &http.HTTP2Config{SendPingTimeout: u.ping, PingTimeout: u.ping},
		ReadHeaderTimeout: u.handshake,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(u.opts.Log.Handler(), slog.LevelWarn),
	}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	select {
	case <-ln.closed:
	case <-ctx.Done():
	}
	_ = srv.Close()
	_ = conn.Close()
	<-done
}

// bufferedConn reads first what the upgrade's reader holds.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConn is a listener that accepts one connection and closes once that
// connection closes, which ends the server's Serve.
type oneConn struct {
	conn   net.Conn
	closed chan struct{}
	mu     sync.Mutex
	once   sync.Once
}

func newOneConn(c net.Conn) *oneConn {
	l := &oneConn{closed: make(chan struct{})}
	l.conn = &closingConn{Conn: c, l: l}
	return l
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return linkAddr{} }

// closingConn closes its listener when the server closes it.
type closingConn struct {
	net.Conn
	l *oneConn
}

func (c *closingConn) Close() error {
	err := c.Conn.Close()
	_ = c.l.Close()
	return err
}

type linkAddr struct{}

func (linkAddr) Network() string { return "hostlink" }
func (linkAddr) String() string  { return "control plane" }
