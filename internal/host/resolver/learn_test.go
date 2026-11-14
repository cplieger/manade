package resolver

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/egress"
)

// kernelSets records each write a Learner makes.
type kernelSets struct {
	err    error
	writes [][]egress.Learned
	mu     sync.Mutex
}

func (k *kernelSets) write(_ context.Context, ls []egress.Learned) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.writes = append(k.writes, slices.Clone(ls))
	return k.err
}

func (k *kernelSets) all() [][]egress.Learned {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.writes)
}

// closedHost is an internet: false host with ipv6 and one block6 prefix.
func closedHost() *config.Host {
	return &config.Host{Name: "closed", DNS: config.DefaultDNS, Egress: config.Egress{
		IPv6: true, Block6: []netip.Prefix{netip.MustParsePrefix("2a01:e0a::/32")},
	}}
}

func learnable() func(netip.Addr) bool {
	return (&egress.Input{Host: closedHost()}).Learnable()
}

func newTestLearner(log *bytes.Buffer) (*Learner, *kernelSets) {
	k := &kernelSets{}
	h := slog.New(slog.NewTextHandler(log, nil))
	return NewLearner(k.write, learnable(), h), k
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestLearnAddsWithTheTTLPlusTenMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, k := newTestLearner(&bytes.Buffer{})
		err := l.Learn(t.Context(), []Record{
			{Addr: addr("140.82.121.10"), TTL: time.Minute},
			{Addr: addr("2606:50c0:8000::154"), TTL: 5 * time.Minute},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []egress.Learned{
			{Addr: addr("140.82.121.10"), Left: 11 * time.Minute},
			{Addr: addr("2606:50c0:8000::154"), Left: 15 * time.Minute},
		}
		if w := k.all(); len(w) != 1 || !slices.Equal(w[0], want) {
			t.Errorf("writes = %v, want one write of %v", w, want)
		}
		time.Sleep(2 * time.Minute)
		held := l.Held()
		slices.SortFunc(held, func(a, b egress.Learned) int { return a.Addr.Compare(b.Addr) })
		wantHeld := []egress.Learned{
			{Addr: addr("140.82.121.10"), Left: 9 * time.Minute},
			{Addr: addr("2606:50c0:8000::154"), Left: 13 * time.Minute},
		}
		if !slices.Equal(held, wantHeld) {
			t.Errorf("Held after 2m = %v, want each with its remaining time %v", held, wantHeld)
		}
	})
}

func TestLaterAnswerRenews(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, k := newTestLearner(&bytes.Buffer{})
		a := addr("140.82.121.10")
		_ = l.Learn(t.Context(), []Record{{Addr: a, TTL: time.Minute}})
		time.Sleep(10 * time.Minute)
		_ = l.Learn(t.Context(), []Record{{Addr: a, TTL: time.Minute}})
		w := k.all()
		if len(w) != 2 || w[1][0].Left != 11*time.Minute {
			t.Errorf("writes = %v, want the renewal written with a full 11m", w)
		}
		time.Sleep(5 * time.Minute)
		if h := l.Held(); len(h) != 1 || h[0].Left != 6*time.Minute {
			t.Errorf("Held = %v, want the renewed expiry, 6m left", h)
		}
		time.Sleep(6 * time.Minute)
		if h := l.Held(); len(h) != 0 {
			t.Errorf("Held = %v, want it expired", h)
		}
	})
}

func TestLearnSkipsWhatTheDropsTake(t *testing.T) {
	l, k := newTestLearner(&bytes.Buffer{})
	err := l.Learn(t.Context(), []Record{
		{Addr: addr("192.168.1.10"), TTL: time.Minute},
		{Addr: addr("100.64.0.1"), TTL: time.Minute},
		{Addr: addr("127.0.0.1"), TTL: time.Minute},
		{Addr: addr("fd00::1"), TTL: time.Minute},
		{Addr: addr("2a01:e0a:1:2::3"), TTL: time.Minute},
		{Addr: addr("2001:db8::1"), TTL: time.Minute},
	})
	if err != nil || len(k.all()) != 0 || len(l.Held()) != 0 {
		t.Errorf("Learn = %v, writes %v, held %v, want nothing learned", err, k.all(), l.Held())
	}
	t.Run("IPv6 on a host without ipv6", func(t *testing.T) {
		h := closedHost()
		h.Egress.IPv6 = false
		k := &kernelSets{}
		l := NewLearner(k.write, (&egress.Input{Host: h}).Learnable(), slog.New(slog.DiscardHandler))
		_ = l.Learn(t.Context(), []Record{{Addr: addr("2606:50c0:8000::154"), TTL: time.Minute}})
		if len(k.all()) != 0 {
			t.Errorf("writes = %v, want none: the host has no IPv6 set", k.all())
		}
	})
}

func TestLearnAnswersTheWriteFailure(t *testing.T) {
	l, k := newTestLearner(&bytes.Buffer{})
	k.err = context.DeadlineExceeded
	if err := l.Learn(t.Context(), []Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}}); err != k.err {
		t.Errorf("Learn = %v, want the write's error", err)
	}
	if len(l.Held()) != 1 {
		t.Error("a failed write dropped the address from the record a reload loads")
	}
}

// fill learns egress.LearnedMax IPv4 addresses with ttl.
func fill(t *testing.T, l *Learner, ttl time.Duration) {
	t.Helper()
	recs := make([]Record, egress.LearnedMax)
	for i := range recs {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], 0x0b000000+uint32(i))
		recs[i] = Record{Addr: netip.AddrFrom4(b), TTL: ttl}
	}
	if err := l.Learn(t.Context(), recs); err != nil {
		t.Fatal(err)
	}
}

func TestBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log bytes.Buffer
		l, k := newTestLearner(&log)
		fill(t, l, time.Hour)
		writes := len(k.all())
		_ = l.Learn(t.Context(), []Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
		_ = l.Learn(t.Context(), []Record{{Addr: addr("140.82.121.11"), TTL: time.Minute}})
		c := l.Counts()
		if c.Held[IPv4] != egress.LearnedMax || c.Refused[IPv4] != 2 || c.Refused[IPv6] != 0 {
			t.Errorf("Counts = %+v, want %d held and 2 refused IPv4", c, egress.LearnedMax)
		}
		if len(k.all()) != writes {
			t.Error("a refused address was written to the kernel")
		}
		if n := strings.Count(log.String(), "level=WARN"); n != 1 {
			t.Errorf("%d WARN lines, want 1 within the minute:\n%s", n, log.String())
		}
		_ = l.Learn(t.Context(), []Record{{Addr: addr("2606:50c0:8000::154"), TTL: time.Minute}})
		if c := l.Counts(); c.Held[IPv6] != 1 {
			t.Errorf("Counts = %+v, want the IPv6 family's own bound", c)
		}
		_ = l.Learn(t.Context(), []Record{{Addr: addr("11.0.0.5"), TTL: time.Hour}})
		if w := k.all(); len(w) != writes+2 {
			t.Errorf("a held address was refused at the bound; writes %d, want %d", len(w), writes+2)
		}
		time.Sleep(time.Minute)
		_ = l.Learn(t.Context(), []Record{{Addr: addr("140.82.121.12"), TTL: time.Minute}})
		if n := strings.Count(log.String(), "level=WARN"); n != 2 {
			t.Errorf("%d WARN lines, want a second one a minute later", n)
		}
		time.Sleep(time.Hour + learnPad)
		_ = l.Learn(t.Context(), []Record{{Addr: addr("140.82.121.13"), TTL: time.Minute}})
		if c := l.Counts(); c.Held[IPv4] != 1 || c.Refused[IPv4] != 3 {
			t.Errorf("Counts = %+v, want the expired set emptied and the add taken", c)
		}
	})
}

func TestTheWriteHoldsLoading(t *testing.T) {
	inWrite, release := make(chan struct{}), make(chan struct{})
	l := NewLearner(func(context.Context, []egress.Learned) error {
		close(inWrite)
		<-release
		return nil
	}, learnable(), slog.New(slog.DiscardHandler))
	done := make(chan error)
	go func() {
		done <- l.Learn(context.Background(), []Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
	}()
	<-inWrite
	try, ok := l.Loading().(interface{ TryLock() bool })
	if !ok {
		t.Fatal("Loading is not a mutex")
	}
	if try.TryLock() {
		t.Error("a rule load could start while a learned address was being written")
	}
	if len(l.Held()) != 1 {
		t.Error("the address is not recorded before its write, so a load after it would miss it")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !try.TryLock() {
		t.Error("Loading still held after the write")
	}
}
