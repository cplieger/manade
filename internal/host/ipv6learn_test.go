package host

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

func (e *echoes) echo(_ context.Context, url string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.urls = append(e.urls, url)
	r := e.replies[min(len(e.urls), len(e.replies))-1]
	return r()
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
			p, err := Learn64(t.Context(), e.echo, url)
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
	echo, err := EchoOverIPv6()
	if err != nil {
		t.Fatal(err)
	}
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
		n, err := Up(t.Context(), v6Section(), f.platform(), Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		if st := n.IPv6Status(); st == nil || st.Reason == "" || st.Prefix.IsValid() {
			t.Errorf("IPv6Status before any ask = %+v, want a reason", st)
		}
		if passes6(fwd6(f)) {
			t.Error("job IPv6 passes before the /64 is learned")
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- n.Hold(ctx) }()
		synctest.Wait()
		if st := n.IPv6Status(); st.Prefix.IsValid() || !strings.Contains(st.Reason, "timeout") {
			t.Errorf("IPv6Status after a failed ask = %+v, want the failure", st)
		}
		time.Sleep(EchoInterval)
		synctest.Wait()
		if st := n.IPv6Status(); st.Prefix != pfx("2a01:e0a:1259:2710::/64") {
			t.Errorf("IPv6Status = %+v, want the learned /64", st)
		}
		if r := fwd6(f); !passes6(r) || !dropsPrefix(r, "2a01:e0a:1259:2710::/64") {
			t.Errorf("MANADE-FWD6 = %q, want IPv6 passing with the learned /64 dropped", r)
		}
		loads := restores(f)
		time.Sleep(EchoInterval)
		synctest.Wait()
		if got := restores(f); got != loads {
			t.Errorf("the same /64 loaded the rules again (%d restores, then %d)", loads, got)
		}
		time.Sleep(EchoInterval)
		synctest.Wait()
		if r := fwd6(f); !dropsPrefix(r, "2a01:e0a:aaaa:bbbb::/64") || dropsPrefix(r, "2a01:e0a:1259:2710::/64") {
			t.Errorf("MANADE-FWD6 = %q, want the rotated /64 followed", r)
		}
		time.Sleep(EchoInterval)
		synctest.Wait()
		if st := n.IPv6Status(); st.Prefix != pfx("2a01:e0a:aaaa:bbbb::/64") {
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
		n, err := Up(t.Context(), v6Section(), f.platform(), Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		f.kernel.fail[platform.LegacyDir+"/ip6tables-restore"] = errors.New("refused")
		if err := n.Hold(t.Context()); err == nil || !strings.Contains(err.Error(), "loading the egress rules again") || f.proc.stopped != 1 {
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
	n, err := Up(t.Context(), section(), f.platform(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if n.IPv6Status() != nil {
		t.Error("an IPv4 host reports a /64")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := n.Hold(ctx); err != nil {
		t.Errorf("Hold = %v", err)
	}
}
