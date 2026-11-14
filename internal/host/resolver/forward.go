package resolver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/egress"
)

// Port is the DNS port, the forwarder's and its upstreams'.
const Port = 53

const (
	// maxMessage is the largest DNS message over either transport.
	maxMessage = 65535
	// udpInFlight and tcpConns bound the queries answered at once; a UDP
	// query past the bound is dropped and its client asks again.
	udpInFlight = 256
	tcpConns    = 64
	// tcpIdle closes a client's TCP connection that sends nothing.
	tcpIdle = 10 * time.Second
)

// Upstreams answers the configured resolvers on Port.
func Upstreams(dns []netip.Addr) []netip.AddrPort {
	out := make([]netip.AddrPort, len(dns))
	for i, a := range dns {
		out[i] = netip.AddrPortFrom(a, Port)
	}
	return out
}

// Listen opens the forwarder's UDP and TCP listeners at addr.
func Listen(ctx context.Context, addr netip.AddrPort) (net.PacketConn, net.Listener, error) {
	var lc net.ListenConfig
	udp, err := lc.ListenPacket(ctx, "udp", addr.String())
	if err != nil {
		return nil, nil, fmt.Errorf("listening for DNS on %s: %w", addr, err)
	}
	tcp, err := lc.Listen(ctx, "tcp", addr.String())
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("listening for DNS on %s: %w", addr, err), udp.Close())
	}
	return udp, tcp, nil
}

// Learning is what a forwarder learns: the answers for a name Match lists
// have their addresses added by Learner before they are answered, or after
// once the query's budget has passed.
type Learning struct {
	Match   *Matcher
	Learner *Learner
}

// Forwarder answers each query from its upstreams within its budget and,
// while it learns, has the Learner add the addresses of a listed name's
// answer before it answers, finishing an add the budget cut after it.
type Forwarder struct {
	upstreams func() []netip.AddrPort
	learning  func() *Learning
	log       *slog.Logger
	// adds are the learns in flight, which may outlive their query.
	adds   sync.WaitGroup
	budget time.Duration
}

// NewForwarder answers a Forwarder asking the resolvers upstreams answers,
// in order, and learning what learning answers, nil to learn nothing; both
// are read at each query, so they follow the host's section and list.
func NewForwarder(upstreams func() []netip.AddrPort, learning func() *Learning, log *slog.Logger) *Forwarder {
	return &Forwarder{upstreams: upstreams, learning: learning, log: log, budget: egress.ForwarderBudget}
}

// Serve answers queries on udp and tcp until ctx ends, then closes both
// and answers nil once every query and learn in flight is done; a listener
// that fails first ends it with the failure.
func (f *Forwarder) Serve(ctx context.Context, udp net.PacketConn, tcp net.Listener) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		<-sctx.Done()
		_ = udp.Close()
		_ = tcp.Close()
	})
	errs := make(chan error, 2)
	wg.Go(func() { errs <- f.serveUDP(sctx, udp, &wg) })
	wg.Go(func() { errs <- f.serveTCP(sctx, tcp, &wg) })
	err := <-errs
	cancel()
	wg.Wait()
	f.adds.Wait()
	if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (f *Forwarder) serveUDP(ctx context.Context, conn net.PacketConn, wg *sync.WaitGroup) error {
	slots := make(chan struct{}, udpInFlight)
	buf := make([]byte, maxMessage)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return fmt.Errorf("DNS over UDP: %w", err)
		}
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		query := append([]byte(nil), buf[:n]...)
		wg.Go(func() {
			defer func() { <-slots }()
			if resp := f.answer(ctx, "udp", query); resp != nil {
				_, _ = conn.WriteTo(resp, from)
			}
		})
	}
}

func (f *Forwarder) serveTCP(ctx context.Context, l net.Listener, wg *sync.WaitGroup) error {
	slots := make(chan struct{}, tcpConns)
	for {
		c, err := l.Accept()
		if err != nil {
			return fmt.Errorf("DNS over TCP: %w", err)
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			f.serveConn(ctx, c)
		})
	}
}

// serveConn answers a client's queries in turn until it goes quiet, closes
// or ctx ends.
func (f *Forwarder) serveConn(ctx context.Context, c net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer c.Close()
	for {
		if err := c.SetDeadline(time.Now().Add(tcpIdle)); err != nil {
			return
		}
		query, err := readTCP(c)
		if err != nil {
			return
		}
		resp := f.answer(ctx, "tcp", query)
		if resp == nil || writeTCP(c, resp) != nil {
			return
		}
	}
}

func readTCP(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, msg)
	return msg, err
}

func writeTCP(w io.Writer, msg []byte) error {
	n := len(msg)
	if n > maxMessage {
		return errors.New("a DNS message past 65535 bytes")
	}
	b := binary.BigEndian.AppendUint16(make([]byte, 0, 2+n), uint16(n))
	_, err := w.Write(append(b, msg...))
	return err
}

// answer answers query within the budget from its arrival, or nil for a
// message that is not one query.
func (f *Forwarder) answer(ctx context.Context, network string, query []byte) []byte {
	q, err := parseQuestion(query)
	if err != nil {
		return nil
	}
	if isResponse(query) {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, f.budget)
	defer cancel()
	resp := f.ask(qctx, network, query, q)
	if resp == nil {
		return failure(query, q, rcodeFail)
	}
	if l := f.learning(); l != nil && rcodeOf(resp) == rcodeOK && l.Match.Match(q.name) {
		f.learn(ctx, qctx, l.Learner, resp)
	}
	return resp
}

// learn adds resp's addresses under ctx, the forwarder's run, waiting for
// the add until budget, the query's, ends: an add past it still lands
// after the rule load it waits for, and the job's connection retransmits.
func (f *Forwarder) learn(ctx, budget context.Context, l *Learner, resp []byte) {
	rq, err := parseQuestion(resp)
	var recs []Record
	if err == nil {
		recs, err = addresses(resp, rq)
	}
	if err != nil {
		f.log.Warn("An upstream answer for a GitHub name did not parse; nothing learned", "name", rq.name, "err", err)
		return
	}
	done := make(chan struct{})
	f.adds.Go(func() {
		defer close(done)
		if err := l.Learn(ctx, recs); err != nil {
			f.log.Warn("Adding learned GitHub addresses to the rules failed; jobs' connections to them are dropped", "name", rq.name, "err", err)
		}
	})
	select {
	case <-done:
	case <-budget.Done():
	}
}

type exchanged struct {
	err  error
	resp []byte
}

// asking is one query's walk through the upstreams.
type asking struct {
	ctx     context.Context
	res     chan exchanged
	timer   *time.Timer
	network string
	query   []byte
	ups     []netip.AddrPort
	last    []byte
	q       question
	step    time.Duration
	next    int
	pending int
}

// launch asks the next upstream and gives it its share of the budget.
func (a *asking) launch() {
	up := a.ups[a.next]
	a.next++
	a.pending++
	a.timer.Reset(a.step)
	go func() {
		resp, err := exchange(a.ctx, a.network, up, a.query, a.q)
		a.res <- exchanged{resp: resp, err: err}
	}()
}

func (a *asking) more() bool { return a.next < len(a.ups) }

// took takes one upstream's result and answers the response to return, if
// the walk is over: a good answer, or the last failure answer once every
// upstream has answered or failed.
func (a *asking) took(r exchanged) ([]byte, bool) {
	a.pending--
	if r.err == nil && !failed(r.resp) {
		return r.resp, true
	}
	if r.err == nil {
		a.last = r.resp
	}
	if a.more() {
		a.launch()
		return nil, false
	}
	return a.last, a.pending == 0
}

// ask answers the first good answer of the upstreams before ctx ends,
// else the last SERVFAIL or REFUSED one gave, else nil. Each upstream is
// asked in turn, the next one once the one before has failed or its share
// of the budget has passed, while those before may still answer.
func (f *Forwarder) ask(ctx context.Context, network string, query []byte, q question) []byte {
	ups := f.upstreams()
	if len(ups) == 0 {
		return nil
	}
	step := f.budget / time.Duration(len(ups))
	a := &asking{
		ctx: ctx, res: make(chan exchanged, len(ups)), timer: time.NewTimer(step),
		network: network, query: query, ups: ups, q: q, step: step,
	}
	defer a.timer.Stop()
	a.launch()
	for {
		select {
		case <-a.timer.C:
			if a.more() {
				a.launch()
			}
		case r := <-a.res:
			if resp, over := a.took(r); over {
				return resp
			}
		case <-ctx.Done():
			return a.last
		}
	}
}

// failed reports whether resp says its upstream could not answer, so the
// next one is asked.
func failed(resp []byte) bool {
	rc := rcodeOf(resp)
	return rc == rcodeFail || rc == rcodeRefused
}

// exchange asks up for query over network and answers its response to q.
func exchange(ctx context.Context, network string, up netip.AddrPort, query []byte, q question) ([]byte, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network, up.String())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	if network == "tcp" {
		if err := writeTCP(c, query); err != nil {
			return nil, err
		}
		resp, err := readTCP(c)
		if err != nil {
			return nil, err
		}
		if !answers(resp, q) {
			return nil, errors.New("the upstream answered another question")
		}
		return resp, nil
	}
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, maxMessage)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		// A datagram answering another question is stray or spoofed.
		if answers(buf[:n], q) {
			return buf[:n], nil
		}
	}
}

// answers reports whether resp is a response to q.
func answers(resp []byte, q question) bool {
	rq, err := parseQuestion(resp)
	if err != nil {
		return false
	}
	return isResponse(resp) && sameQuestion(q, rq)
}
