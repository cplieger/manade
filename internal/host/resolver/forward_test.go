package resolver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/egress"
)

// clientWait bounds a test client's wait for an answer; it is no part of
// what a test asserts.
const clientWait = 10 * time.Second

// upstream is a fake resolver on one port for UDP and TCP: handle answers
// the datagrams or the one TCP message to send for a query, none for
// silence.
func upstream(t *testing.T, handle func(q []byte) [][]byte) netip.AddrPort {
	t.Helper()
	ap, udp, tcp := listenPair(t)
	t.Cleanup(func() { _ = udp.Close(); _ = tcp.Close() })
	go func() {
		buf := make([]byte, maxMessage)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			for _, d := range handle(slices.Clone(buf[:n])) {
				_, _ = udp.WriteTo(d, from)
			}
		}
	}()
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				q, err := readTCP(c)
				if err != nil {
					return
				}
				for _, d := range handle(q) {
					_ = writeTCP(c, d)
				}
			}()
		}
	}()
	return ap
}

// listenPair listens on one loopback port for UDP and TCP, trying ports
// until the TCP side of a free UDP port is free too.
func listenPair(t *testing.T) (netip.AddrPort, net.PacketConn, net.Listener) {
	t.Helper()
	for range 50 {
		udp, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ap := netip.MustParseAddrPort(udp.LocalAddr().String())
		tcp, err := net.Listen("tcp", ap.String())
		if err == nil {
			return ap, udp, tcp
		}
		_ = udp.Close()
	}
	t.Fatal("no loopback port free for both UDP and TCP")
	return netip.AddrPort{}, nil, nil
}

// answering answers every query with recs.
func answering(rcode byte, recs ...rr) func([]byte) [][]byte {
	return func(q []byte) [][]byte { return [][]byte{response(q, rcode, recs...)} }
}

func silent([]byte) [][]byte { return nil }

// closedPort answers a UDP port nothing listens on.
func closedPort(t *testing.T) netip.AddrPort {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ap := netip.MustParseAddrPort(c.LocalAddr().String())
	_ = c.Close()
	return ap
}

// serve runs a Forwarder for meta over ups and answers its address.
func serve(t *testing.T, f *Forwarder) netip.AddrPort {
	t.Helper()
	at, udp, tcp := listenPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.Serve(ctx, udp, tcp) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve = %v, want nil once its context ended", err)
		}
	})
	return at
}

func newForwarder(ups []netip.AddrPort, k *kernelSets) *Forwarder {
	l := NewLearner(k.write, learnable(), slog.New(slog.DiscardHandler))
	m := newMatcher(meta)
	return NewForwarder(func() []netip.AddrPort { return ups }, learns(m, l), slog.New(slog.DiscardHandler))
}

func learns(m *Matcher, l *Learner) func() *Learning {
	return func() *Learning { return &Learning{Match: m, Learner: l} }
}

func ask(t *testing.T, network string, at netip.AddrPort, q []byte) []byte {
	t.Helper()
	c, err := net.Dial(network, at.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(clientWait))
	if network == "tcp" {
		if err := writeTCP(c, q); err != nil {
			t.Fatal(err)
		}
		resp, err := readTCP(c)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, maxMessage)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func TestForwardLearnsAListedName(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			k := &kernelSets{}
			up := upstream(t, answering(0, aRR("140.82.121.10", 60), aRR("140.82.121.11", 60)))
			at := serve(t, newForwarder([]netip.AddrPort{up}, k))
			q := query(42, "CodeLoad.GitHub.com.", typeA)
			resp := ask(t, network, at, q)
			if !slices.Equal(resp, response(q, 0, aRR("140.82.121.10", 60), aRR("140.82.121.11", 60))) {
				t.Errorf("answer = %x, want the upstream's answer relayed", resp)
			}
			want := []egress.Learned{
				{Addr: addr("140.82.121.10"), Left: time.Minute + learnPad},
				{Addr: addr("140.82.121.11"), Left: time.Minute + learnPad},
			}
			if w := k.all(); len(w) != 1 || !slices.Equal(w[0], want) {
				t.Errorf("writes = %v, want %v written before the answer", w, want)
			}
		})
	}
}

func TestForwardWritesBeforeItAnswers(t *testing.T) {
	inWrite, release := make(chan struct{}), make(chan struct{})
	l := NewLearner(func(context.Context, []egress.Learned) error {
		close(inWrite)
		<-release
		return nil
	}, learnable(), slog.New(slog.DiscardHandler))
	m := newMatcher(meta)
	up := upstream(t, answering(0, aRR("140.82.121.10", 60)))
	at := serve(t, NewForwarder(func() []netip.AddrPort { return []netip.AddrPort{up} }, learns(m, l), slog.New(slog.DiscardHandler)))
	got := make(chan []byte, 1)
	go func() {
		c, err := net.Dial("udp", at.String())
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(clientWait))
		_, _ = c.Write(query(1, "codeload.github.com", typeA))
		buf := make([]byte, maxMessage)
		if n, err := c.Read(buf); err == nil {
			got <- buf[:n]
		}
	}()
	<-inWrite
	select {
	case <-got:
		t.Error("the answer reached the client before its addresses were written")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-got:
	case <-time.After(clientWait):
		t.Error("no answer after the write")
	}
}

// written polls k until it holds n writes, failing past clientWait.
func written(t *testing.T, k *kernelSets, n int) {
	t.Helper()
	for end := time.Now().Add(clientWait); len(k.all()) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("writes = %v, want %d", k.all(), n)
		}
	}
}

func TestForwardAnswersInsideTheEmbeddedWaitWhileItLearns(t *testing.T) {
	t.Run("an upstream at 3s and a write taking 2s", func(t *testing.T) {
		k := &kernelSets{}
		l := NewLearner(func(ctx context.Context, ls []egress.Learned) error {
			time.Sleep(2 * time.Second)
			return k.write(ctx, ls)
		}, learnable(), slog.New(slog.DiscardHandler))
		up := upstream(t, func(q []byte) [][]byte {
			time.Sleep(3 * time.Second)
			return answering(0, aRR("140.82.121.10", 60))(q)
		})
		at := serve(t, NewForwarder(func() []netip.AddrPort { return []netip.AddrPort{up} }, learns(newMatcher(meta), l), slog.New(slog.DiscardHandler)))
		start := time.Now()
		resp := ask(t, "udp", at, query(3, "codeload.github.com", typeA))
		if d := time.Since(start); d >= 4*time.Second {
			t.Errorf("answered after %v, past Docker's embedded resolver's 4s wait", d)
		}
		if rcodeOf(resp) != rcodeOK {
			t.Errorf("rcode = %d, want the upstream's answer", rcodeOf(resp))
		}
		written(t, k, 1)
	})
	t.Run("a rule load holding the lock", func(t *testing.T) {
		k := &kernelSets{}
		l := NewLearner(k.write, learnable(), slog.New(slog.DiscardHandler))
		f := NewForwarder(func() []netip.AddrPort { return []netip.AddrPort{upstream(t, answering(0, aRR("140.82.121.10", 60)))} },
			learns(newMatcher(meta), l), slog.New(slog.DiscardHandler))
		f.budget = 300 * time.Millisecond
		at := serve(t, f)
		l.Loading().Lock()
		resp := ask(t, "udp", at, query(4, "codeload.github.com", typeA))
		if rcodeOf(resp) != rcodeOK || len(k.all()) != 0 {
			t.Errorf("rcode %d, writes %v, want the answer while the load runs, nothing written", rcodeOf(resp), k.all())
		}
		l.Loading().Unlock()
		written(t, k, 1)
	})
}

func TestServeWaitsForALearnItsQueryLeft(t *testing.T) {
	inWrite, release := make(chan struct{}), make(chan struct{})
	l := NewLearner(func(context.Context, []egress.Learned) error {
		close(inWrite)
		<-release
		return nil
	}, learnable(), slog.New(slog.DiscardHandler))
	f := NewForwarder(func() []netip.AddrPort { return []netip.AddrPort{upstream(t, answering(0, aRR("140.82.121.10", 60)))} },
		learns(newMatcher(meta), l), slog.New(slog.DiscardHandler))
	f.budget = 300 * time.Millisecond
	at, udp, tcp := listenPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, udp, tcp) }()
	ask(t, "udp", at, query(4, "codeload.github.com", typeA))
	<-inWrite
	cancel()
	select {
	case <-done:
		t.Error("Serve returned while a learn its query left was still writing")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestForwardLearnsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		qname  string
		handle func([]byte) [][]byte
		rcode  rcode
	}{
		{"for an unlisted name", "registry-1.docker.io", answering(0, aRR("54.198.86.24", 60)), 0},
		{"from a failed upstream", "codeload.github.com", answering(2), 2},
		{"for a name that does not exist", "nope.github.com", answering(3), 3},
		{"for a private answer", "codeload.github.com", answering(0, aRR("192.168.1.20", 60)), 0},
		{"from a refusal carrying an address", "codeload.github.com", answering(5, aRR("140.82.121.10", 60)), 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &kernelSets{}
			at := serve(t, newForwarder([]netip.AddrPort{upstream(t, tc.handle)}, k))
			resp := ask(t, "udp", at, query(5, tc.qname, typeA))
			if rcodeOf(resp) != tc.rcode {
				t.Errorf("rcode = %d, want %d relayed", rcodeOf(resp), tc.rcode)
			}
			if w := k.all(); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}

func TestForwardWithLearningOffRelaysAListedName(t *testing.T) {
	k := &kernelSets{}
	var on atomic.Bool
	on.Store(true)
	l := NewLearner(k.write, learnable(), slog.New(slog.DiscardHandler))
	m := newMatcher(meta)
	up := upstream(t, answering(0, aRR("140.82.121.10", 60)))
	at := serve(t, NewForwarder(func() []netip.AddrPort { return []netip.AddrPort{up} }, func() *Learning {
		if !on.Load() {
			return nil
		}
		return &Learning{Match: m, Learner: l}
	}, slog.New(slog.DiscardHandler)))
	on.Store(false)
	q := query(7, "codeload.github.com", typeA)
	if resp := ask(t, "udp", at, q); !slices.Equal(resp, response(q, 0, aRR("140.82.121.10", 60))) {
		t.Errorf("answer = %x, want the upstream's answer relayed", resp)
	}
	if w := k.all(); len(w) != 0 {
		t.Errorf("writes = %v, want none while learning is off", w)
	}
	on.Store(true)
	ask(t, "udp", at, query(8, "codeload.github.com", typeA))
	if w := k.all(); len(w) != 1 {
		t.Errorf("writes = %v, want the answer learned once learning is on again", w)
	}
}

func TestForwardAsksTheNextUpstream(t *testing.T) {
	t.Run("after its share of the budget when the first is silent", func(t *testing.T) {
		k := &kernelSets{}
		var asked atomic.Int32
		first := upstream(t, func([]byte) [][]byte { asked.Add(1); return nil })
		second := upstream(t, answering(0, aRR("140.82.121.10", 60)))
		at := serve(t, newForwarder([]netip.AddrPort{first, second}, k))
		resp := ask(t, "udp", at, query(5, "github.com", typeA))
		if rcodeOf(resp) != 0 || len(k.all()) != 1 || asked.Load() != 1 {
			t.Errorf("rcode %d, writes %v, first asked %d times, want the second's answer", rcodeOf(resp), k.all(), asked.Load())
		}
	})
	t.Run("at once when the first refuses the connection", func(t *testing.T) {
		f := newForwarder([]netip.AddrPort{closedPort(t), upstream(t, answering(0, aRR("140.82.121.10", 60)))}, &kernelSets{})
		at := serve(t, f)
		start := time.Now()
		resp := ask(t, "udp", at, query(5, "github.com", typeA))
		if rcodeOf(resp) != 0 {
			t.Errorf("rcode = %d", rcodeOf(resp))
		}
		if d := time.Since(start); d >= f.budget/2 {
			t.Errorf("answered after %v, want before the first's share of %v ran out", d, f.budget/2)
		}
	})
	t.Run("after a REFUSED", func(t *testing.T) {
		at := serve(t, newForwarder([]netip.AddrPort{upstream(t, answering(5)), upstream(t, answering(0, aRR("140.82.121.10", 60)))}, &kernelSets{}))
		if rc := rcodeOf(ask(t, "udp", at, query(5, "github.com", typeA))); rc != 0 {
			t.Errorf("rcode = %d, want the second upstream's answer", rc)
		}
	})
	t.Run("after a SERVFAIL, keeping it for want of better", func(t *testing.T) {
		at := serve(t, newForwarder([]netip.AddrPort{upstream(t, answering(2)), upstream(t, answering(5))}, &kernelSets{}))
		if rc := rcodeOf(ask(t, "udp", at, query(5, "github.com", typeA))); rc != 5 {
			t.Errorf("rcode = %d, want the last failure answer relayed", rc)
		}
	})
}

func TestForwardAnswersSERVFAILWithinItsBudget(t *testing.T) {
	f := newForwarder([]netip.AddrPort{upstream(t, silent), upstream(t, silent)}, &kernelSets{})
	f.budget = 500 * time.Millisecond
	at := serve(t, f)
	q := query(77, "github.com", typeA)
	resp := ask(t, "udp", at, q)
	pq, _ := parseQuestion(q)
	if !slices.Equal(resp, failure(q, pq, rcodeFail)) {
		t.Errorf("answer = %x, want SERVFAIL to the query", resp)
	}
	if egress.ForwarderBudget >= 4*time.Second {
		t.Errorf("ForwarderBudget %v is not inside Docker's 4s wait on its upstream", egress.ForwarderBudget)
	}
}

func TestForwardIgnoresAStrayDatagram(t *testing.T) {
	k := &kernelSets{}
	up := upstream(t, func(q []byte) [][]byte {
		other := query(9999, "github.com", typeA)
		return [][]byte{response(other, 0, aRR("140.82.121.66", 60)), response(q, 0, aRR("140.82.121.10", 60))}
	})
	at := serve(t, newForwarder([]netip.AddrPort{up}, k))
	ask(t, "udp", at, query(5, "github.com", typeA))
	if w := k.all(); len(w) != 1 || len(w[0]) != 1 || w[0][0].Addr != addr("140.82.121.10") {
		t.Errorf("writes = %v, want only the answer to the query asked", w)
	}
}

func TestForwardReadsItsUpstreamsAtEachQuery(t *testing.T) {
	var ups atomic.Pointer[[]netip.AddrPort]
	old, next := []netip.AddrPort{upstream(t, answering(5))}, []netip.AddrPort{upstream(t, answering(0, aRR("140.82.121.10", 60)))}
	ups.Store(&old)
	m := newMatcher(meta)
	l := NewLearner((&kernelSets{}).write, learnable(), slog.New(slog.DiscardHandler))
	at := serve(t, NewForwarder(func() []netip.AddrPort { return *ups.Load() }, learns(m, l), slog.New(slog.DiscardHandler)))
	if rc := rcodeOf(ask(t, "udp", at, query(5, "github.com", typeA))); rc != 5 {
		t.Errorf("rcode = %d, want the first upstream's", rc)
	}
	ups.Store(&next)
	if rc := rcodeOf(ask(t, "udp", at, query(6, "github.com", typeA))); rc != 0 {
		t.Errorf("rcode = %d, want the new upstream's answer", rc)
	}
}

func TestServeEndsOnAListenerFailure(t *testing.T) {
	f := newForwarder(nil, &kernelSets{})
	udp, tcp, err := Listen(t.Context(), netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	_ = tcp.Close()
	err = f.Serve(t.Context(), udp, tcp)
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("Serve = %v, want the TCP listener's failure", err)
	}
}
