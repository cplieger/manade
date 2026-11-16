package hostlink

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

// backend is a hostapi.Backend whose probe waits for release and whose
// changes come from changes.
type backend struct {
	changes chan hostapi.Runner
	release chan struct{}
	probing atomic.Int32
}

var _ hostapi.Backend = (*backend)(nil)

func newBackend() *backend {
	return &backend{changes: make(chan hostapi.Runner), release: make(chan struct{})}
}

func (*backend) Status(context.Context) hostapi.Status {
	return hostapi.Status{Version: "v1.0.0-dev.1"}
}

func (*backend) CreateRunner(_ context.Context, req *hostapi.RunnerRequest) (hostapi.Runner, error) {
	return hostapi.Runner{Name: req.RunnerName}, nil
}

func (*backend) Runners(context.Context) ([]hostapi.Runner, error) { return nil, nil }

func (*backend) DeleteRunner(context.Context, string, bool) ([]byte, error) { return nil, nil }

func (*backend) Logs(context.Context, string, int) ([]byte, error) { return nil, nil }

func (*backend) Diag(context.Context, string) ([]byte, error) { return nil, nil }

func (*backend) PutSection(context.Context, *hostapi.Section) (hostapi.SectionAnswer, error) {
	return hostapi.SectionAnswer{Applies: hostapi.SectionApplied}, nil
}

func (*backend) Restarting() <-chan struct{} { return nil }

func (b *backend) Subscribe(ctx context.Context) <-chan hostapi.Runner {
	out := make(chan hostapi.Runner)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-b.changes:
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (b *backend) Probe(ctx context.Context) (hostapi.ProbeReport, error) {
	b.probing.Add(1)
	select {
	case <-b.release:
		return hostapi.ProbeReport{Proven: true}, nil
	case <-ctx.Done():
		return hostapi.ProbeReport{}, ctx.Err()
	}
}

func (*backend) ProbeDeadline() time.Duration { return 10 * time.Minute }

// rig is a control plane listening on a socket in a temporary directory.
type rig struct {
	srv  *Server
	path string
	// ping, when set, is each host's ping timeout.
	ping time.Duration
}

// newRig serves a control plane until the test ends; wrap, when set, wraps
// each connection it accepts.
func newRig(t *testing.T, wrap func(net.Conn) net.Conn) *rig {
	t.Helper()
	r := &rig{srv: NewServer(nil), path: filepath.Join(t.TempDir(), "hosts.sock")}
	ln, err := ListenSocket(r.path)
	if err != nil {
		t.Fatal(err)
	}
	l := ln
	if wrap != nil {
		l = &wrapListener{Listener: ln, wrap: wrap}
	}
	r.serve(t, l)
	return r
}

func (r *rig) serve(t *testing.T, ln net.Listener) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
}

// dial reaches the rig's socket.
func (r *rig) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", r.path)
}

// host runs a host named name on the rig until the test ends or stop is
// called, answering its uplink.
func (r *rig) host(t *testing.T, name, instance string, b *backend, dial func(context.Context) (net.Conn, error)) (u *Uplink, stop func()) {
	t.Helper()
	if dial == nil {
		dial = r.dial
	}
	u = NewUplink(&UplinkOptions{
		Dial:    dial,
		Hello:   func() Hello { return Hello{Name: name, Build: "v1.0.0-dev.1", Instance: instance} },
		Handler: hostapi.NewServer(b, instance, nil).Handler(),
	})
	u.wait = backoff{min: 10 * time.Millisecond, max: 40 * time.Millisecond, reset: time.Minute}
	if r.ping != 0 {
		u.ping = r.ping
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		u.Run(ctx)
		close(done)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return u, stop
}

// link waits for the rig to hold a link for name from instance.
func (r *rig) link(t *testing.T, name, instance string) *Link {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l, ok := r.srv.Link(name); ok && l.Instance == instance {
			return l
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no link for %s from instance %s", name, instance)
	return nil
}

type wrapListener struct {
	net.Listener
	wrap func(net.Conn) net.Conn
}

func (l *wrapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.wrap(c), nil
}

// freezable, once frozen, drops every byte it is given and reads nothing
// until it is closed, as a peer that stops answering does.
type freezable struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
	frozen atomic.Bool
}

func newFreezable(c net.Conn) *freezable { return &freezable{Conn: c, closed: make(chan struct{})} }

func (c *freezable) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.frozen.Load() {
		<-c.closed
		return 0, net.ErrClosed
	}
	return n, err
}

func (c *freezable) Write(p []byte) (int, error) {
	if c.frozen.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *freezable) Close() error {
	c.once.Do(func() { close(c.closed) })
	if c.frozen.Load() {
		// The peer must not learn of the close, so only the read waiting on
		// it is released.
		return c.SetReadDeadline(time.Unix(1, 0))
	}
	return c.Conn.Close()
}

// freezer records every connection it wraps.
type freezer struct {
	conns []*freezable
	mu    sync.Mutex
}

func (f *freezer) wrap(c net.Conn) *freezable {
	fc := newFreezable(c)
	f.mu.Lock()
	f.conns = append(f.conns, fc)
	f.mu.Unlock()
	return fc
}

func (f *freezer) last() *freezable {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[len(f.conns)-1]
}

// syncBuffer is a bytes.Buffer a logger writes while the test reads it.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
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
