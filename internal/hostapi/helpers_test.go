package hostapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackend is a Backend whose answers each test sets.
type fakeBackend struct {
	err, subscribeErr error
	changes           chan Runner
	probe             func(ctx context.Context) (ProbeReport, error)
	// section answers each PUT /v1/section; sections records each one.
	section    func(*Section) (SectionAnswer, error)
	sections   []Section
	restarting chan struct{}
	created    []RunnerRequest
	deleted    []string
	runners    []Runner
	logs       []byte
	diag       []byte
	status     Status
	deadline   time.Duration
	mu         sync.Mutex
	diagAsk    bool
}

var _ Backend = (*fakeBackend)(nil)

func (f *fakeBackend) Status(context.Context) Status { return f.status }

func (f *fakeBackend) CreateRunner(_ context.Context, req *RunnerRequest) (Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Runner{}, f.err
	}
	for i := range f.created {
		if f.created[i].RunnerName == req.RunnerName {
			return Runner{Name: req.RunnerName, Profile: req.Profile, Scope: req.Scope, RunnerID: req.RunnerID, State: StateStarting}, nil
		}
	}
	f.created = append(f.created, *req)
	return Runner{Name: req.RunnerName, Profile: req.Profile, Scope: req.Scope, RunnerID: req.RunnerID, State: StateCreating}, nil
}

func (f *fakeBackend) Runners(context.Context) ([]Runner, error) { return f.runners, f.err }

func (f *fakeBackend) DeleteRunner(_ context.Context, name string, diag bool) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted, f.diagAsk = append(f.deleted, name), diag
	if f.err != nil {
		return nil, f.err
	}
	if diag {
		return f.diag, nil
	}
	return nil, nil
}

func (f *fakeBackend) Logs(context.Context, string, int) ([]byte, error) { return f.logs, f.err }

func (f *fakeBackend) Subscribe(ctx context.Context) (<-chan Runner, error) {
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	out := make(chan Runner)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case r, ok := <-f.changes:
				if !ok {
					return
				}
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (f *fakeBackend) Probe(ctx context.Context) (ProbeReport, error) {
	if f.probe != nil {
		return f.probe(ctx)
	}
	return ProbeReport{Proven: true}, f.err
}

func (f *fakeBackend) ProbeDeadline() time.Duration {
	if f.deadline == 0 {
		return time.Minute
	}
	return f.deadline
}

func (f *fakeBackend) PutSection(_ context.Context, s *Section) (SectionAnswer, error) {
	f.mu.Lock()
	f.sections = append(f.sections, *s)
	f.mu.Unlock()
	if f.section != nil {
		return f.section(s)
	}
	return SectionAnswer{Applies: SectionApplied}, f.err
}

func (f *fakeBackend) Restarting() <-chan struct{} { return f.restarting }

// overH2C serves h over HTTP/2 with prior knowledge on one in-memory
// connection, as the host channel serves the host API, and answers a client
// of it. It runs inside a synctest bubble until the test ends.
func overH2C(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	var h2c http.Protocols
	h2c.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: &h2c}
	ln := &oneListener{conn: srvConn, closed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	var dialed atomic.Bool
	tr := &http.Transport{Protocols: &h2c, DialContext: func(context.Context, string, string) (net.Conn, error) {
		if dialed.Swap(true) {
			return nil, errors.New("the test's connection was dialed already")
		}
		return cliConn, nil
	}}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		_ = srv.Close()
		<-done
	})
	return &http.Client{Transport: tr}
}

// oneListener accepts conn once, then waits for Close.
type oneListener struct {
	conn   net.Conn
	closed chan struct{}
	mu     sync.Mutex
	once   sync.Once
}

func (l *oneListener) Accept() (net.Conn, error) {
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

func (l *oneListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*oneListener) Addr() net.Addr { return &net.UnixAddr{Name: "memory", Net: "unix"} }

func validRequest() RunnerRequest {
	return RunnerRequest{Profile: "small", RunnerName: "manade-small-0123456789ab", Scope: "acme/app", RunnerID: 42, JITConfig: "eyJqaXQiOiJ0ZXN0In0="}
}
