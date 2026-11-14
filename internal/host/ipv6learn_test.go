package host

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/platform"
)

// trace is a cdn-cgi/trace body in Cloudflare's shape with addr as ip=.
func trace(addr string) []byte {
	return []byte("fl=123f45\nh=cloudflare.com\nip=" + addr + "\nts=1763100000.123\nvisit_scheme=https\nloc=FR\n")
}

// echoes answers its replies in turn, the last one again once they run out.
type echoes struct {
	urls    []string
	replies []func() ([]byte, error)
	mu      sync.Mutex
}

// echo answers the next reply, or ctx's error once ctx ends, as an HTTP
// client does.
func (e *echoes) echo(ctx context.Context, url string) ([]byte, error) {
	e.mu.Lock()
	e.urls = append(e.urls, url)
	r := e.replies[min(len(e.urls), len(e.replies))-1]
	e.mu.Unlock()
	type got struct {
		err  error
		body []byte
	}
	ch := make(chan got, 1)
	go func() {
		b, err := r()
		ch <- got{body: b, err: err}
	}()
	select {
	case g := <-ch:
		return g.body, g.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func answer(addr string) func() ([]byte, error) {
	return func() ([]byte, error) { return trace(addr), nil }
}

func TestLearn64(t *testing.T) {
	const url = "https://cloudflare.com/cdn-cgi/trace"
	for _, tc := range []struct {
		name  string
		reply func() ([]byte, error)
		want  string
		err   string
	}{
		{name: "an IPv6 answer", reply: answer("2a01:e0a:1259:2710:1c29:a0ff:fed1:7008"), want: "2a01:e0a:1259:2710::/64"},
		{name: "an IPv4 answer", reply: answer("82.65.12.34"), err: "IPv4 address 82.65.12.34"},
		{name: "a mapped IPv4 answer", reply: answer("::ffff:82.65.12.34"), err: "IPv4 address 82.65.12.34"},
		{name: "a ULA answer", reply: answer("fd00::1"), err: "not a global IPv6 address"},
		{name: "no ip line", reply: func() ([]byte, error) { return []byte("h=cloudflare.com\n"), nil }, err: "no ip= line"},
		{name: "a bad ip line", reply: func() ([]byte, error) { return []byte("ip=nope\n"), nil }, err: `ip="nope"`},
		{name: "a failure", reply: func() ([]byte, error) { return nil, errors.New("dial tcp6: network is unreachable") }, err: "network is unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &echoes{replies: []func() ([]byte, error){tc.reply}}
			p, err := ask64(t.Context(), e.echo, url)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), url) {
					t.Errorf("Learn64 = %v, %v, want an error naming %q and the echo", p, err, tc.err)
				}
				return
			}
			if err != nil || p.String() != tc.want {
				t.Errorf("Learn64 = %v, %v, want %s", p, err, tc.want)
			}
		})
	}
}

func TestEchoOverIPv6(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(trace(r.RemoteAddr))
	}))
	defer srv.Close()
	echo := echoOverIPv6()
	if _, err := echo(t.Context(), srv.URL); err == nil {
		t.Errorf("an IPv4 server at %s answered an IPv6-only echo", srv.URL)
	}
}

// v6Fixture is an IPv6 host with a global address on its own network, in
// another /64 than the machine's.
func v6Fixture(t *testing.T, e *echoes) *upFixture {
	t.Helper()
	f := newUpFixture(t)
	f.kernel = newFakeIPT(true)
	f.echo = e.echo
	writeAt(t, f.root, "/proc/net/if_inet6", "2a010e0a125927ff0000000000000005 07 40 00 80     eth0\n")
	writeAt(t, f.root, "/proc/net/ipv6_route", "00000000000000000000000000000000 00 00000000000000000000000000000000 00 2a010e0a125927100000000000000001 00000400 00000001 00000000 00000003     eth0\n")
	writeAt(t, f.root, "/proc/sys/net/ipv6/conf/all/forwarding", "0\n")
	return f
}

func v6Section() *config.Host {
	h := section()
	h.Egress.IPv6 = true
	h.Egress.IPv6Echo = "https://echo.example/trace"
	return h
}

// fwd6 answers the loaded ip6tables MANADE-FWD.
func fwd6(f *upFixture) []string { return f.kernel.chain(true, "filter", "MANADE-FWD") }

func dropsPrefix(rules []string, p string) bool {
	return slices.ContainsFunc(rules, func(r string) bool { return strings.Contains(r, "-d "+p) && strings.HasSuffix(r, "-j DROP") })
}

// passes6 reports whether jobs' global IPv6 passes: only what lies outside
// 2000::/3 drops, not all of it.
func passes6(rules []string) bool {
	return slices.Contains(rules, "! -d 2000::/3 -i manade+ -j DROP")
}

func restores(f *upFixture) int {
	n := 0
	for _, c := range f.kernel.commands() {
		if strings.Contains(c, "-restore") {
			n++
		}
	}
	return n
}

func TestHoldLearns64(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := &echoes{replies: []func() ([]byte, error){
			func() ([]byte, error) { return nil, errors.New("timeout") },
			answer("2a01:e0a:1259:2710::7008"),
			answer("2a01:e0a:1259:2710::7009"),
			answer("2a01:e0a:aaaa:bbbb::1"),
			func() ([]byte, error) { return nil, errors.New("timeout") },
		}}
		f := v6Fixture(t, e)
		n := upNet(t, f, v6Section())
		if st := n.iPv6Status(); st == nil || st.Reason == "" || st.Prefix.IsValid() {
			t.Errorf("IPv6Status before any ask = %+v, want a reason", st)
		}
		if passes6(fwd6(f)) {
			t.Error("job IPv6 passes before the /64 is learned")
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- n.hold(ctx) }()
		synctest.Wait()
		if st := n.iPv6Status(); st.Prefix.IsValid() || !strings.Contains(st.Reason, "timeout") {
			t.Errorf("IPv6Status after a failed ask = %+v, want the failure", st)
		}
		time.Sleep(echoInterval)
		synctest.Wait()
		if st := n.iPv6Status(); st.Prefix != pfx("2a01:e0a:1259:2710::/64") {
			t.Errorf("IPv6Status = %+v, want the learned /64", st)
		}
		if r := fwd6(f); !passes6(r) || !dropsPrefix(r, "2a01:e0a:1259:2710::/64") {
			t.Errorf("MANADE-FWD6 = %q, want IPv6 passing with the learned /64 dropped", r)
		}
		loads := restores(f)
		time.Sleep(echoInterval)
		synctest.Wait()
		if got := restores(f); got != loads {
			t.Errorf("the same /64 loaded the rules again (%d restores, then %d)", loads, got)
		}
		time.Sleep(echoInterval)
		synctest.Wait()
		if r := fwd6(f); !dropsPrefix(r, "2a01:e0a:aaaa:bbbb::/64") || dropsPrefix(r, "2a01:e0a:1259:2710::/64") {
			t.Errorf("MANADE-FWD6 = %q, want the rotated /64 followed", r)
		}
		time.Sleep(echoInterval)
		synctest.Wait()
		if st := n.iPv6Status(); st.Prefix != pfx("2a01:e0a:aaaa:bbbb::/64") {
			t.Errorf("IPv6Status after a failed ask = %+v, want the last /64 kept", st)
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Hold = %v", err)
		}
		if !slices.Equal(slices.Compact(slices.Clone(e.urls)), []string{"https://echo.example/trace"}) {
			t.Errorf("asked %q, want the section's ipv6_echo", e.urls)
		}
	})
}

func TestHoldEndsWhenA64CannotLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := v6Fixture(t, &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::7008")}})
		n := upNet(t, f, v6Section())
		f.kernel.fail[platform.LegacyDir+"/ip6tables-restore"] = errors.New("refused")
		if err := n.hold(t.Context()); err == nil || !strings.Contains(err.Error(), "loading the egress rules again") || f.proc.stopped != 1 {
			t.Errorf("Hold = %v, stopped %d, want the failure with the daemon stopped", err, f.proc.stopped)
		}
	})
}

func TestNoEchoWithoutIPv6AndInternet(t *testing.T) {
	f := newUpFixture(t)
	f.echo = func(context.Context, string) ([]byte, error) {
		t.Error("an IPv4 host asked its ipv6_echo")
		return nil, errors.New("no")
	}
	n := upNet(t, f, section())
	if n.iPv6Status() != nil {
		t.Error("an IPv4 host reports a /64")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := n.hold(ctx); err != nil {
		t.Errorf("Hold = %v", err)
	}
}

func (e *echoes) asked() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.urls)
}

// closedV6Section is v6Section with internet off, so with no ipv6_echo.
func closedV6Section() *config.Host {
	h := v6Section()
	h.Egress.Internet = false
	h.Egress.IPv6Echo = ""
	return h
}

// holding runs n's hold until the test ends and answers its result.
func holding(t *testing.T, n *hostNet) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- n.hold(ctx) }()
	return func() error {
		cancel()
		return <-done
	}
}

func TestAChangeOfInternetRebuildsThe64(t *testing.T) {
	t.Run("turning internet on asks at once and lets job IPv6 out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::7008")}}
			f := v6Fixture(t, e)
			n := upNet(t, f, closedV6Section())
			stop := holding(t, n)
			time.Sleep(2 * echoInterval)
			synctest.Wait()
			if got := e.asked(); got != 0 {
				t.Errorf("an internet: false host asked its ipv6_echo %d times", got)
			}
			at := time.Now()
			if failed, fatal := n.change(t.Context(), v6Section()); failed != nil || fatal != nil {
				t.Fatalf("Change = %v, %v", failed, fatal)
			}
			synctest.Wait()
			if got := e.asked(); got != 1 || time.Since(at) != 0 {
				t.Errorf("asked %d times %v after the change, want once at once", got, time.Since(at))
			}
			if st := n.iPv6Status(); st == nil || st.Prefix != pfx("2a01:e0a:1259:2710::/64") {
				t.Errorf("IPv6Status = %+v, want the learned /64", st)
			}
			if r := fwd6(f); !passes6(r) || !dropsPrefix(r, "2a01:e0a:1259:2710::/64") {
				t.Errorf("MANADE-FWD6 = %q, want IPv6 passing with the learned /64 dropped", r)
			}
			if err := stop(); err != nil {
				t.Errorf("Hold = %v", err)
			}
		})
	})
	t.Run("turning internet off stops asking, and on again starts unasked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := &echoes{replies: []func() ([]byte, error){
				answer("2a01:e0a:1259:2710::7008"),
				func() ([]byte, error) { return nil, errors.New("dial tcp6: network is unreachable") },
			}}
			f := v6Fixture(t, e)
			n := upNet(t, f, v6Section())
			stop := holding(t, n)
			synctest.Wait()
			if st := n.iPv6Status(); st == nil || !st.Prefix.IsValid() {
				t.Fatalf("setup: IPv6Status = %+v, want a learned /64", st)
			}
			if failed, fatal := n.change(t.Context(), closedV6Section()); failed != nil || fatal != nil {
				t.Fatalf("Change = %v, %v", failed, fatal)
			}
			if st := n.iPv6Status(); st != nil {
				t.Errorf("IPv6Status = %+v on an internet: false host, want none", st)
			}
			if dropsPrefix(fwd6(f), "2a01:e0a:1259:2710::/64") {
				t.Error("the internet: false rules carry the /64 learned before the change")
			}
			loads := restores(f)
			time.Sleep(2*echoInterval + time.Second)
			synctest.Wait()
			if got := e.asked(); got != 1 {
				t.Errorf("asked %d times, want no ask after internet was turned off", got)
			}
			if got := restores(f); got != loads {
				t.Errorf("the closed rules were restored %d times after the change, want them held as loaded", got-loads)
			}
			if failed, fatal := n.change(t.Context(), v6Section()); failed != nil || fatal != nil {
				t.Fatalf("Change = %v, %v", failed, fatal)
			}
			synctest.Wait()
			if st := n.iPv6Status(); st == nil || st.Prefix.IsValid() || !strings.Contains(st.Reason, "unreachable") {
				t.Errorf("IPv6Status = %+v, want the failed ask, not the /64 learned before internet was off", st)
			}
			if passes6(fwd6(f)) {
				t.Error("job IPv6 passes on a /64 learned before internet was turned off")
			}
			if err := stop(); err != nil {
				t.Errorf("Hold = %v", err)
			}
		})
	})
	t.Run("an ask in flight at the change is dropped", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			e := &echoes{replies: []func() ([]byte, error){
				func() ([]byte, error) {
					<-release
					return trace("2a01:e0a:1259:2710::7008"), nil
				},
				func() ([]byte, error) { return nil, errors.New("dial tcp6: network is unreachable") },
			}}
			f := v6Fixture(t, e)
			n := upNet(t, f, v6Section())
			stop := holding(t, n)
			synctest.Wait()
			if failed, fatal := n.change(t.Context(), closedV6Section()); failed != nil || fatal != nil {
				t.Fatalf("Change = %v, %v", failed, fatal)
			}
			close(release)
			if failed, fatal := n.change(t.Context(), v6Section()); failed != nil || fatal != nil {
				t.Fatalf("Change = %v, %v", failed, fatal)
			}
			synctest.Wait()
			if st := n.iPv6Status(); st == nil || st.Prefix.IsValid() || !strings.Contains(st.Reason, "unreachable") {
				t.Errorf("IPv6Status = %+v, want the failed ask, not the answer that came after the change", st)
			}
			if err := stop(); err != nil {
				t.Errorf("Hold = %v", err)
			}
		})
	})
}

// A change or a stop that cancels the learner's rule load ends the posture
// as cleanly as one between two asks.
func TestAnEndedPostureMidLoadIsNoFailure(t *testing.T) {
	// blockedLoad is a platform whose first restore once armed waits for
	// its context, as exec.CommandContext's load does when a change ends
	// the posture.
	blockedLoad := func(t *testing.T) (n *hostNet, f *upFixture, blocked <-chan struct{}) {
		t.Helper()
		f = v6Fixture(t, &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::7008")}})
		p := f.platform()
		var armed atomic.Bool
		ch := make(chan struct{})
		run := p.Run
		p.Run = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
			if strings.HasSuffix(name, "-restore") && armed.CompareAndSwap(true, false) {
				close(ch)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return run(ctx, stdin, name, args...)
		}
		n, err := bringUp(t.Context(), v6Section(), p, guardHooks{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = n.stop() })
		armed.Store(true)
		return n, f, ch
	}
	t.Run("a change rebuilds to the new posture", func(t *testing.T) {
		n, f, blocked := blockedLoad(t)
		stop := holding(t, n)
		<-blocked
		if failed, fatal := n.change(t.Context(), closedV6Section()); failed != nil || fatal != nil {
			t.Fatalf("Change = %v, %v, want the new posture with no failure", failed, fatal)
		}
		if st := n.iPv6Status(); st != nil {
			t.Errorf("IPv6Status = %+v, want the internet: false posture's none", st)
		}
		f.proc.mu.Lock()
		stopped := f.proc.stopped
		f.proc.mu.Unlock()
		if stopped != 0 {
			t.Errorf("the daemon was stopped %d times, want it running", stopped)
		}
		if err := stop(); err != nil {
			t.Errorf("Hold = %v", err)
		}
	})
	t.Run("a stop ends the hold with no error", func(t *testing.T) {
		n, _, blocked := blockedLoad(t)
		stop := holding(t, n)
		<-blocked
		if err := stop(); err != nil {
			t.Errorf("Hold = %v, want nil", err)
		}
	})
}

// The host's own /64 change is loaded under the guard's lock, so a check
// that runs while it is made finds the new rules loaded and restores
// nothing.
func TestALearned64IsNoRestore(t *testing.T) {
	var restored []string
	f := v6Fixture(t, &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::7008")}})
	p := f.platform()
	n, err := bringUp(t.Context(), v6Section(), p, guardHooks{Restored: func(w string) { restored = append(restored, w) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.stop() })
	g := n.rulesGuard()
	checked := make(chan error, 1)
	err = g.refresh(t.Context(), func() {
		n.posture.Load().self64.Store(&learned64{prefix: pfx("2a01:e0a:1259:2710::/64")})
		go func() { checked <- g.check(t.Context()) }()
		// The window a check would need to load the new /64 itself and
		// count it restored, were apply run outside the guard's lock.
		time.Sleep(50 * time.Millisecond)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Errorf("restored %q, want the host's own /64 load counted as none", restored)
	}
	if !dropsPrefix(fwd6(f), "2a01:e0a:1259:2710::/64") {
		t.Error("the learned /64 was not loaded")
	}
}
