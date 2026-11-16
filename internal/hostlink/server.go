package hostlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

// Link is one host's open connection, over which the control plane calls
// the host's API.
type Link struct {
	Since time.Time
	// API calls the host over this connection only, and fails once it
	// closes.
	API  *hostapi.Client
	cc   *http.ClientConn
	conn *watchedConn
	Hello
}

// Done is closed once the connection has closed.
func (l *Link) Done() <-chan struct{} { return l.conn.done }

// Close closes the connection; the host redials.
func (l *Link) Close() error {
	return errors.Join(l.cc.Close(), l.conn.Close())
}

// replaceWarnEvery limits the warning for a name whose connection keeps
// being replaced, which two hosts holding one identity cause.
const replaceWarnEvery = time.Minute

// Server is the control plane's side of the host channel: it answers the
// hosts' upgrades and holds one Link per connected host.
type Server struct {
	log    *slog.Logger
	links  map[string]*Link
	warned map[string]time.Time
	wg     sync.WaitGroup
	// ping and handshake are pingTimeout and handshakeTimeout, test seams.
	ping      time.Duration
	handshake time.Duration
	mu        sync.Mutex
}

// NewServer answers hosts' upgrades. log may be nil for slog.Default.
func NewServer(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log, links: map[string]*Link{}, warned: map[string]time.Time{}, ping: pingTimeout, handshake: handshakeTimeout}
}

// Link answers the open connection of the host named name.
func (s *Server) Link(name string) (*Link, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[name]
	return l, ok
}

// Serve answers upgrades on ln until ctx ends, then closes ln and every
// host's connection and waits for them.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+connectPath, s.connect)
	var http1 http.Protocols
	http1.SetHTTP1(true)
	srv := &http.Server{
		Handler:           mux,
		Protocols:         &http1,
		ReadHeaderTimeout: s.handshake,
		ReadTimeout:       s.handshake,
		WriteTimeout:      s.handshake,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	var err error
	select {
	case serr := <-done:
		err = fmt.Errorf("hosts listener: %w", serr)
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.handshake)
		err = srv.Shutdown(sctx)
		cancel()
		if serr := <-done; !errors.Is(serr, http.ErrServerClosed) {
			err = errors.Join(err, serr)
		}
	}
	s.closeAll()
	s.wg.Wait()
	return err
}

func (s *Server) closeAll() {
	s.mu.Lock()
	links := make([]*Link, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()
	for _, l := range links {
		_ = l.Close()
	}
}

// connect answers a host's upgrade with 101 and opens its Link, or refuses
// it with a refusal body.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get("Upgrade")
	if got != Protocol || !hasToken(r.Header.Values("Connection"), "upgrade") {
		writeRefusal(w, http.StatusUpgradeRequired, &refusal{
			Code: CodeUpgradeRequired, Want: Protocol, Got: got,
			Message: "this control plane speaks " + Protocol + "; upgrade the host or the control plane so both speak it",
		})
		return
	}
	hello, err := helloFrom(r.Header)
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, &refusal{Code: CodeBadRequest, Message: err.Error()})
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.log.Warn("A host's upgrade could not take its connection", "host", hello.Name, "error", err)
		return
	}
	link, err := s.open(r.Context(), conn, brw.Reader.Buffered(), &hello)
	if err != nil {
		_ = conn.Close()
		s.log.Warn("A host's connection failed during its upgrade", "host", hello.Name, "error", err)
		return
	}
	s.register(link)
}

// open writes the 101 on conn and makes the client the control plane calls
// the host with. buffered counts bytes the host sent past its request.
func (s *Server) open(ctx context.Context, conn net.Conn, buffered int, hello *Hello) (*Link, error) {
	if buffered > 0 {
		return nil, errors.New("the host sent data before the upgrade's answer")
	}
	if err := conn.SetDeadline(time.Now().Add(s.handshake)); err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + Protocol + "\r\n\r\n")); err != nil {
		return nil, err
	}
	// A hijacked connection keeps its deadlines, which would end the link in
	// the middle of a call.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	wc := &watchedConn{Conn: conn, done: make(chan struct{})}
	var h2c http.Protocols
	h2c.SetUnencryptedHTTP2(true)
	var dialed atomic.Bool
	tr := &http.Transport{
		Protocols: &h2c,
		HTTP2:     &http.HTTP2Config{SendPingTimeout: s.ping, PingTimeout: s.ping},
		// The Transport hands out the one connection once; the ClientConn
		// never dials again.
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if dialed.Swap(true) {
				return nil, errors.New("a host's link has one connection")
			}
			return wc, nil
		},
	}
	cc, err := tr.NewClientConn(context.WithoutCancel(ctx), "http", net.JoinHostPort(hello.Name, "80"))
	if err != nil {
		return nil, err
	}
	return &Link{Hello: *hello, Since: time.Now(), API: hostapi.NewClient(cc), cc: cc, conn: wc}, nil
}

// register makes l its host's Link, closing the one it replaces.
func (s *Server) register(l *Link) {
	s.mu.Lock()
	old := s.links[l.Name]
	s.links[l.Name] = l
	warn := old != nil && time.Since(s.warned[l.Name]) >= replaceWarnEvery
	if warn {
		s.warned[l.Name] = time.Now()
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go s.watch(l)
	if old == nil {
		s.log.Info("A host connected", "host", l.Name, "build", l.Build, "instance", l.Instance, "section", l.Section)
		return
	}
	_ = old.Close()
	if warn {
		s.log.Warn("A host connected under a name that was already connected, and the newer connection replaces the older; two hosts holding one identity make it flap",
			"host", l.Name, "instance", l.Instance, "replaced_instance", old.Instance)
	} else {
		s.log.Debug("A host's newer connection replaced its older one", "host", l.Name, "instance", l.Instance)
	}
}

// watch drops l once its connection closes, unless a newer one replaced it.
func (s *Server) watch(l *Link) {
	defer s.wg.Done()
	<-l.Done()
	_ = l.cc.Close()
	s.mu.Lock()
	current := s.links[l.Name] == l
	if current {
		delete(s.links, l.Name)
	}
	s.mu.Unlock()
	if current {
		s.log.Info("A host disconnected", "host", l.Name, "instance", l.Instance, "connected_for", time.Since(l.Since).Round(time.Second))
	}
}

// watchedConn closes done on its first Close, which the HTTP/2 client calls
// when the connection fails or a ping goes unanswered.
type watchedConn struct {
	net.Conn
	err  error
	done chan struct{}
	once sync.Once
}

func (c *watchedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		close(c.done)
	})
	return c.err
}

// hasToken reports whether a comma-separated header holds token, in any case.
func hasToken(values []string, token string) bool {
	for _, v := range values {
		for f := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), token) {
				return true
			}
		}
	}
	return false
}
