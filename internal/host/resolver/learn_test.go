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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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

func newTestLearner(log *bytes.Buffer) (*Learner, *kernelSets) {
	k := &kernelSets{}
	h := slog.New(slog.NewTextHandler(log, nil))
	return NewLearner(k.write, h), k
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// learn adds recs through l, running the drain when the add starts one,
// and waits for their write.
func learn(ctx context.Context, l *Learner, recs []Record) {
	written, drain := l.Learn(recs)
	if drain != nil {
		drain(ctx)
	}
	<-written
}

func TestLearnAddsWithTheTTLPlusTenMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, k := newTestLearner(&bytes.Buffer{})
		learn(t.Context(), l, []Record{
			{Addr: addr("140.82.121.10"), TTL: time.Minute},
			{Addr: addr("140.82.121.11"), TTL: 5 * time.Minute},
		})
		want := []egress.Learned{
			{Addr: addr("140.82.121.10"), Left: 11 * time.Minute},
			{Addr: addr("140.82.121.11"), Left: 15 * time.Minute},
		}
		if w := k.all(); len(w) != 1 || !slices.Equal(w[0], want) {
			t.Errorf("writes = %v, want one write of %v", w, want)
		}
		time.Sleep(2 * time.Minute)
		held := l.Held()
		slices.SortFunc(held, func(a, b egress.Learned) int { return a.Addr.Compare(b.Addr) })
		wantHeld := []egress.Learned{
			{Addr: addr("140.82.121.10"), Left: 9 * time.Minute},
			{Addr: addr("140.82.121.11"), Left: 13 * time.Minute},
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
		learn(t.Context(), l, []Record{{Addr: a, TTL: time.Minute}})
		time.Sleep(10 * time.Minute)
		learn(t.Context(), l, []Record{{Addr: a, TTL: time.Minute}})
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
	learn(t.Context(), l, []Record{
		{Addr: addr("192.168.1.10"), TTL: time.Minute},
		{Addr: addr("100.64.0.1"), TTL: time.Minute},
		{Addr: addr("127.0.0.1"), TTL: time.Minute},
		{Addr: addr("fd00::1"), TTL: time.Minute},
		{Addr: addr("2001:db8::1"), TTL: time.Minute},
		{Addr: addr("2606:50c0:8000::154"), TTL: time.Minute},
	})
	if len(k.all()) != 0 || len(l.Held()) != 0 {
		t.Errorf("writes %v, held %v, want nothing learned: no private address and no IPv6 one", k.all(), l.Held())
	}
}

// A retired learner waits for the write in flight, then writes nothing:
// the rule set it learned into is being replaced.
func TestRetire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var writes atomic.Int32
		l := NewLearner(func(context.Context, []egress.Learned) error {
			if writes.Add(1) == 1 {
				<-release
			}
			return nil
		}, slog.New(slog.DiscardHandler))
		first, drain := l.Learn([]Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
		go drain(t.Context())
		synctest.Wait()
		queued, more := l.Learn([]Record{{Addr: addr("140.82.121.11"), TTL: time.Minute}})
		if more != nil {
			t.Fatal("a second drain started while one runs")
		}
		retired := make(chan struct{})
		go func() { l.Retire(); close(retired) }()
		synctest.Wait()
		select {
		case <-retired:
			t.Error("Retire returned while a write was in flight")
		default:
		}
		close(release)
		<-retired
		<-first
		<-queued
		late, drain := l.Learn([]Record{{Addr: addr("140.82.121.12"), TTL: time.Minute}})
		if drain != nil {
			drain(t.Context())
		}
		<-late
		if n := writes.Load(); n != 1 {
			t.Errorf("writes = %d, want only the one in flight at the retire", n)
		}
	})
}

func TestLearnLogsTheWriteFailure(t *testing.T) {
	var log bytes.Buffer
	l, k := newTestLearner(&log)
	k.err = context.DeadlineExceeded
	learn(t.Context(), l, []Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
	if !strings.Contains(log.String(), "level=WARN") || !strings.Contains(log.String(), k.err.Error()) {
		t.Errorf("log = %q, want a WARN naming the write's error", log.String())
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
	learn(t.Context(), l, recs)
}

func TestBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log bytes.Buffer
		l, k := newTestLearner(&log)
		fill(t, l, time.Hour)
		writes := len(k.all())
		learn(t.Context(), l, []Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
		learn(t.Context(), l, []Record{{Addr: addr("140.82.121.11"), TTL: time.Minute}})
		c := l.Counts()
		if c.Held != egress.LearnedMax || c.Refused != 2 {
			t.Errorf("Counts = %+v, want %d held and 2 refused", c, egress.LearnedMax)
		}
		if len(k.all()) != writes {
			t.Error("a refused address was written to the kernel")
		}
		if n := strings.Count(log.String(), "level=WARN"); n != 1 {
			t.Errorf("%d WARN lines, want 1 within the minute:\n%s", n, log.String())
		}
		learn(t.Context(), l, []Record{{Addr: addr("11.0.0.5"), TTL: time.Hour}})
		if w := k.all(); len(w) != writes+1 {
			t.Errorf("a held address was refused at the bound; writes %d, want %d", len(w), writes+1)
		}
		time.Sleep(time.Minute)
		learn(t.Context(), l, []Record{{Addr: addr("140.82.121.12"), TTL: time.Minute}})
		if n := strings.Count(log.String(), "level=WARN"); n != 2 {
			t.Errorf("%d WARN lines, want a second one a minute later", n)
		}
		time.Sleep(time.Hour + learnPad)
		learn(t.Context(), l, []Record{{Addr: addr("140.82.121.13"), TTL: time.Minute}})
		if c := l.Counts(); c.Held != 1 || c.Refused != 3 {
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
	}, slog.New(slog.DiscardHandler))
	done := make(chan struct{})
	go func() {
		learn(context.Background(), l, []Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
		close(done)
	}()
	<-inWrite
	if tryLock(l.load) {
		t.Error("a rule load could start while a learned address was being written")
	}
	if len(l.Held()) != 1 {
		t.Error("the address is not recorded before its write, so a load after it would miss it")
	}
	close(release)
	<-done
	if !tryLock(l.load) {
		t.Error("Loading still held after the write")
	}
}

// tryLock takes k if it is free, answering whether it did.
func tryLock(k loadLock) bool {
	select {
	case k <- struct{}{}:
		return true
	default:
		return false
	}
}

func TestADrainWaitingOnALoadEndsWithItsContext(t *testing.T) {
	var log bytes.Buffer
	l, k := newTestLearner(&log)
	l.Loading().Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	written, drain := l.Learn([]Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
	drain(ctx)
	select {
	case <-written:
	default:
		t.Error("the batch's waiters were left waiting once its drain ended")
	}
	if len(l.Held()) != 0 || len(k.all()) != 0 {
		t.Errorf("held %v, writes %v, want nothing recorded", l.Held(), k.all())
	}
	if !strings.Contains(log.String(), context.DeadlineExceeded.Error()) {
		t.Errorf("log = %q, want the lost batch logged with its context's end", log.String())
	}
	l.Loading().Unlock()
	written, drain = l.Learn([]Record{{Addr: addr("140.82.121.11"), TTL: time.Minute}})
	if drain == nil {
		t.Fatal("an add after a drain ended on its context started no drain")
	}
	drain(t.Context())
	<-written
	if w := k.all(); len(w) != 1 || w[0][0].Addr != addr("140.82.121.11") {
		t.Errorf("writes = %v, want the later add written", w)
	}
}

func TestAnAddWithNothingToLearnWaitsForNoWrite(t *testing.T) {
	l, k := newTestLearner(&bytes.Buffer{})
	l.Loading().Lock()
	defer l.Loading().Unlock()
	for name, recs := range map[string][]Record{
		"an empty answer":            nil,
		"only a private address":     {{Addr: addr("10.0.0.1"), TTL: time.Minute}},
		"only a blocked IPv6 prefix": {{Addr: addr("2a01:e0a::1"), TTL: time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			written, drain := l.Learn(recs)
			if drain != nil {
				t.Error("an add with nothing to learn started a drain")
			}
			select {
			case <-written:
			default:
				t.Error("an add with nothing to learn waits for a write")
			}
		})
	}
	if _, drain := l.Learn([]Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}}); drain == nil {
		t.Error("an add after the empty ones started no drain")
	}
	if len(k.all()) != 0 {
		t.Errorf("writes = %v, want none while the load is held", k.all())
	}
}

func TestAddsDuringAWriteJoinTheNextOne(t *testing.T) {
	k := &kernelSets{}
	inWrite, release := make(chan struct{}), make(chan struct{})
	l := NewLearner(func(ctx context.Context, ls []egress.Learned) error {
		inWrite <- struct{}{}
		<-release
		return k.write(ctx, ls)
	}, slog.New(slog.DiscardHandler))
	first, drain := l.Learn([]Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
	if drain == nil {
		t.Fatal("the first add started no drain")
	}
	drained := make(chan struct{})
	go func() {
		drain(t.Context())
		close(drained)
	}()
	<-inWrite
	second, d2 := l.Learn([]Record{{Addr: addr("140.82.121.11"), TTL: time.Minute}})
	third, d3 := l.Learn([]Record{{Addr: addr("140.82.121.12"), TTL: time.Minute}})
	if d2 != nil || d3 != nil {
		t.Error("an add during a write started a second drain")
	}
	if second != third {
		t.Error("two adds during one write went to different writes")
	}
	release <- struct{}{}
	<-first
	select {
	case <-second:
		t.Error("an add made during the first write was answered written with it")
	default:
	}
	<-inWrite
	release <- struct{}{}
	<-second
	<-drained
	want := [][]netip.Addr{{addr("140.82.121.10")}, {addr("140.82.121.11"), addr("140.82.121.12")}}
	got := make([][]netip.Addr, 0, 2)
	for _, w := range k.all() {
		var as []netip.Addr
		for _, e := range w {
			as = append(as, e.Addr)
		}
		got = append(got, as)
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	if _, again := l.Learn([]Record{{Addr: addr("140.82.121.13"), TTL: time.Minute}}); again == nil {
		t.Error("an add after the drain ended started no drain")
	} else {
		go func() { <-inWrite; release <- struct{}{} }()
		again(t.Context())
	}
}

func TestAStuckWriterKeepsOneDrainAndLetsALoadIn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var writes int
		got := map[netip.Addr]bool{}
		l := NewLearner(func(ctx context.Context, ls []egress.Learned) error {
			mu.Lock()
			writes++
			for _, e := range ls {
				got[e.Addr] = true
			}
			mu.Unlock()
			<-ctx.Done()
			return ctx.Err()
		}, slog.New(slog.DiscardHandler))
		var running, most int
		var wg sync.WaitGroup
		waited := make(chan time.Duration, 1)
		start := time.Now()
		const adds = 40
		for i := range adds {
			if i == adds/4 {
				go func() {
					at := time.Now()
					l.Loading().Lock()
					waited <- time.Since(at)
					l.Loading().Unlock()
				}()
			}
			_, drain := l.Learn([]Record{{Addr: netip.AddrFrom4([4]byte{140, 82, 121, byte(i)}), TTL: time.Minute}})
			if drain != nil {
				mu.Lock()
				running++
				most = max(most, running)
				mu.Unlock()
				wg.Go(func() {
					drain(t.Context())
					mu.Lock()
					running--
					mu.Unlock()
				})
			}
			time.Sleep(100 * time.Millisecond)
		}
		wg.Wait()
		elapsed := time.Since(start)
		if d := <-waited; d > learnTimeout {
			t.Errorf("a rule load waited %v, want at most one write's %v", d, learnTimeout)
		}
		mu.Lock()
		defer mu.Unlock()
		if most != 1 {
			t.Errorf("%d drains ran at once, want 1", most)
		}
		if limit := int(elapsed/learnTimeout) + 1; writes > limit {
			t.Errorf("%d writes in %v, want at most %d of %v each", writes, elapsed, limit, learnTimeout)
		}
		if len(got) != adds {
			t.Errorf("%d addresses written, want all %d", len(got), adds)
		}
	})
}

func TestLearnBoundsTheWrite(t *testing.T) {
	var left time.Duration
	l := NewLearner(func(ctx context.Context, _ []egress.Learned) error {
		dl, ok := ctx.Deadline()
		if ok {
			left = time.Until(dl)
		}
		return nil
	}, slog.New(slog.DiscardHandler))
	learn(t.Context(), l, []Record{{Addr: addr("140.82.121.10"), TTL: time.Minute}})
	if left <= 0 || left > 2*time.Second {
		t.Errorf("the write's time left = %v, want within the design's 2s", left)
	}
}
