package hostlink

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

// answer is the control plane's answer to one upgrade request.
type answer struct {
	header http.Header
	body   refusal
	status int
}

// upgradeAnswer sends one /connect request with headers to the rig.
func upgradeAnswer(t *testing.T, r *rig, headers map[string]string) answer {
	t.Helper()
	conn, err := r.dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://manade/connect", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	var body refusal
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &body)
	}
	_ = resp.Body.Close()
	return answer{header: resp.Header, body: body, status: resp.StatusCode}
}

func TestUpgradeAnswers(t *testing.T) {
	r := newRig(t, nil)
	good := map[string]string{"Connection": "Upgrade", "Upgrade": protocol, headerName: "nas", headerBuild: "v1", headerInstance: "i1"}
	with := func(k, v string) map[string]string {
		h := maps.Clone(good)
		if v == "" {
			delete(h, k)
		} else {
			h[k] = v
		}
		return h
	}
	t.Run("101 for a host speaking the protocol", func(t *testing.T) {
		a := upgradeAnswer(t, r, good)
		if a.status != http.StatusSwitchingProtocols || a.header.Get("Upgrade") != protocol {
			t.Errorf("answer = %d, Upgrade %q", a.status, a.header.Get("Upgrade"))
		}
		if l := r.link(t, "nas", "i1"); l.Build != "v1" {
			t.Errorf("link = %+v", l.Hello)
		}
	})
	t.Run("426 for another protocol, naming both", func(t *testing.T) {
		a := upgradeAnswer(t, r, with("Upgrade", "manade-host/2"))
		if a.status != http.StatusUpgradeRequired || a.body.Code != codeUpgradeRequired || a.body.Want != protocol || a.body.Got != "manade-host/2" || a.header.Get("Upgrade") != protocol {
			t.Errorf("answer = %d %+v, Upgrade %q", a.status, a.body, a.header.Get("Upgrade"))
		}
	})
	t.Run("426 for no upgrade at all", func(t *testing.T) {
		a := upgradeAnswer(t, r, with("Upgrade", ""))
		if a.status != http.StatusUpgradeRequired || a.body.Want != protocol || a.body.Got != "" {
			t.Errorf("answer = %d %+v", a.status, a.body)
		}
	})
	t.Run("426 without Connection: upgrade", func(t *testing.T) {
		a := upgradeAnswer(t, r, with("Connection", "keep-alive"))
		if a.status != http.StatusUpgradeRequired {
			t.Errorf("answer = %d", a.status)
		}
	})
	t.Run("400 for a name no host may have", func(t *testing.T) {
		for _, name := range []string{"", "NAS", "../x", "10.0.0.1"} {
			a := upgradeAnswer(t, r, with(headerName, name))
			if a.status != http.StatusBadRequest || a.body.Code != codeBadRequest {
				t.Errorf("name %q: answer = %d %+v", name, a.status, a.body)
			}
		}
		if _, ok := r.srv.Link("NAS"); ok {
			t.Error("a refused name holds a link")
		}
	})
}

// TestPeerTextIsLoggedClean sends a Hello whose fields hold the unsafe runes
// a header can carry; a CR or LF it cannot, since the server refuses one in
// a header value (RFC 9110 §5.5).
func TestPeerTextIsLoggedClean(t *testing.T) {
	var logs syncBuffer
	r := &rig{srv: NewServer(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))), path: filepath.Join(t.TempDir(), "hosts.sock")}
	ln, err := ListenSocket(r.path)
	if err != nil {
		t.Fatal(err)
	}
	r.serve(t, ln)
	const build, instance, section = "v1\u202egnp.exe", "i1\u0085forged", "7\u2028x"
	a := upgradeAnswer(t, r, map[string]string{"Connection": "Upgrade", "Upgrade": protocol, headerName: "nas", headerBuild: build, headerInstance: instance, headerSection: section})
	if a.status != http.StatusSwitchingProtocols {
		t.Fatalf("answer = %d %+v", a.status, a.body)
	}
	r.link(t, "nas", instance)
	const replacing = "i2\u2029x"
	if a := upgradeAnswer(t, r, map[string]string{"Connection": "Upgrade", "Upgrade": protocol, headerName: "nas", headerBuild: build, headerInstance: replacing}); a.status != http.StatusSwitchingProtocols {
		t.Fatalf("replacing answer = %d %+v", a.status, a.body)
	}
	_ = r.link(t, "nas", replacing).Close()
	want := map[string]map[string]string{
		"A host connected": {"host": "nas", "build": "v1 gnp.exe", "instance": "i1 forged", "section": "7 x"},
		"A host connected under a name that was already connected, and the newer connection replaces the older; two hosts holding one identity make it flap": {
			"host": "nas", "instance": "i2 x", "replaced_instance": "i1 forged",
		},
		"A host disconnected": {"host": "nas", "instance": "i2 x"},
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(want) > 0 && time.Now().Before(deadline) {
		for line := range strings.Lines(logs.String()) {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			msg, _ := rec["msg"].(string)
			fields, ok := want[msg]
			if !ok {
				continue
			}
			for k, v := range fields {
				if rec[k] != v {
					t.Errorf("%s: %s = %q, want %q", msg, k, rec[k], v)
				}
			}
			delete(want, msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(want) > 0 {
		t.Errorf("no log line for %v in:\n%s", slices.Collect(maps.Keys(want)), logs.String())
	}
}

func TestUplinkReportsARefusal(t *testing.T) {
	r := newRig(t, nil)
	u, _ := r.host(t, "Bad Name", "i1", newBackend(), nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if refused, ok := errors.AsType[*refusedError](u.State().Err); ok {
			if refused.Status != http.StatusBadRequest || refused.Code != codeBadRequest || u.State().Connected {
				t.Errorf("state = %+v", u.State())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state = %+v, want the refusal", u.State())
}

func TestCallsOverOneLink(t *testing.T) {
	r := newRig(t, nil)
	b := newBackend()
	r.host(t, "nas", "i1", b, nil)
	l := r.link(t, "nas", "i1")

	t.Run("a held stream and a long probe leave other calls answered", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		events := make(chan *hostapi.Event, 4)
		go func() { _ = l.API.Events(ctx, func(ev *hostapi.Event) { events <- ev }) }()
		<-events
		probed := make(chan error, 1)
		go func() {
			_, err := l.API.Probe(ctx, 10*time.Minute)
			probed <- err
		}()
		for b.probing.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		for range 5 {
			if st, err := l.API.Status(t.Context()); err != nil || st.InstanceID != "i1" {
				t.Fatalf("status during the held calls = %+v, %v", st, err)
			}
		}
		b.changes <- hostapi.Runner{Name: "r1"}
		if ev := <-events; ev.Runner == nil || ev.Runner.Name != "r1" {
			t.Errorf("event = %+v", ev)
		}
		b.release <- struct{}{}
		if err := <-probed; err != nil {
			t.Errorf("probe = %v", err)
		}
	})
	t.Run("300 concurrent calls on the one connection", func(t *testing.T) {
		var wg sync.WaitGroup
		var failed atomic.Int32
		for range 300 {
			wg.Go(func() {
				if _, err := l.API.Status(t.Context()); err != nil {
					failed.Add(1)
				}
			})
		}
		wg.Wait()
		if n := failed.Load(); n != 0 {
			t.Errorf("%d of 300 calls failed", n)
		}
		if cur, _ := r.srv.Link("nas"); cur != l {
			t.Error("the calls replaced the link")
		}
	})
}

func TestDeadlinesClearedOnTheUpgradedConnection(t *testing.T) {
	r := &rig{srv: NewServer(nil), path: filepath.Join(t.TempDir(), "hosts.sock")}
	r.srv.handshake = 100 * time.Millisecond
	ln, err := ListenSocket(r.path)
	if err != nil {
		t.Fatal(err)
	}
	r.serve(t, ln)
	r.host(t, "nas", "i1", newBackend(), nil)
	l := r.link(t, "nas", "i1")
	time.Sleep(3 * r.srv.handshake)
	if _, err := l.API.Status(t.Context()); err != nil {
		t.Errorf("a call past the handshake's deadlines = %v", err)
	}
	select {
	case <-l.Done():
		t.Error("the link closed on the handshake's deadline")
	default:
	}
}

func TestASilentPeerIsDroppedOnEachSide(t *testing.T) {
	const ping = 150 * time.Millisecond
	var hostSide, cpSide freezer
	r := &rig{srv: NewServer(nil), path: filepath.Join(t.TempDir(), "hosts.sock"), ping: ping}
	r.srv.ping = ping
	ln, err := ListenSocket(r.path)
	if err != nil {
		t.Fatal(err)
	}
	r.serve(t, &wrapListener{Listener: ln, wrap: func(c net.Conn) net.Conn { return cpSide.wrap(c) }})
	// No redial reaches the control plane after the freeze, whose
	// replacement would close the link for it.
	var cut atomic.Bool
	dial := func(ctx context.Context) (net.Conn, error) {
		if cut.Load() {
			return nil, errors.New("cut off")
		}
		c, err := r.dial(ctx)
		if err != nil {
			return nil, err
		}
		return hostSide.wrap(c), nil
	}
	u, _ := r.host(t, "nas", "i1", newBackend(), dial)
	l := r.link(t, "nas", "i1")
	for !u.State().Connected {
		time.Sleep(time.Millisecond)
	}
	// Each side's peer goes silent and never closes, so each side learns of
	// it only by its own pings.
	frozen := time.Now()
	cut.Store(true)
	hostSide.last().frozen.Store(true)
	cpSide.last().frozen.Store(true)

	bound := 2*ping + time.Second
	select {
	case <-l.Done():
	case <-time.After(bound):
		t.Fatalf("the control plane kept a silent host's link past %s", bound)
	}
	if took := time.Since(frozen); took < ping {
		t.Errorf("the control plane dropped the link after %s, before a ping could go unanswered", took)
	}
	for u.State().Connected && time.Since(frozen) < bound {
		time.Sleep(time.Millisecond)
	}
	if u.State().Connected {
		t.Fatalf("the host kept a silent control plane's connection past %s", bound)
	}
}

func TestANewerConnectionReplacesTheOlder(t *testing.T) {
	r := newRig(t, nil)
	_, stopOld := r.host(t, "nas", "old", newBackend(), nil)
	old := r.link(t, "nas", "old")
	r.host(t, "nas", "new", newBackend(), nil)
	r.link(t, "nas", "new")
	select {
	case <-old.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced link stayed open")
	}
	stopOld()
	cur := r.link(t, "nas", "new")
	if _, err := cur.API.Status(t.Context()); err != nil {
		t.Errorf("the newer link = %v", err)
	}
}

func TestADroppedLinkLeavesTheRegistry(t *testing.T) {
	r := newRig(t, nil)
	_, stop := r.host(t, "nas", "i1", newBackend(), nil)
	l := r.link(t, "nas", "i1")
	stop()
	<-l.Done()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := r.srv.Link("nas"); !ok {
			if _, err := l.API.Status(t.Context()); err == nil {
				t.Error("a call over a closed link succeeded")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a closed link stayed registered")
}

// An upgrade that registers its link after Serve has closed every link
// is closed at once, so no connection outlives Serve.
func TestALinkRegisteredAfterCloseIsClosed(t *testing.T) {
	s := NewServer(slog.New(slog.DiscardHandler))
	conn, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	l, err := s.open(t.Context(), conn, 0, &Hello{Name: "nas", Instance: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	s.closeAll()
	s.register(l)
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the late link stayed open")
	}
	if _, ok := s.Link("nas"); ok {
		t.Error("the late link was registered")
	}
	waited := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Error("Serve's wait would hold for the late link")
	}
}

func TestTheHostRedialsAfterConnectionRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.sock")
	r := &rig{srv: NewServer(nil), path: path}
	u, _ := r.host(t, "nas", "i1", newBackend(), nil)
	for u.State().Err == nil {
		time.Sleep(time.Millisecond)
	}
	if u.State().Connected {
		t.Fatal("connected with no control plane")
	}
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	r.serve(t, ln)
	r.link(t, "nas", "i1")
}

func TestBackoff(t *testing.T) {
	b := backoff{min: time.Second, max: 30 * time.Second, reset: time.Minute}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := b.after(time.Second); got != w {
			t.Errorf("wait %d = %s, want %s", i, got, w)
		}
	}
	if got := b.after(time.Minute); got != time.Second {
		t.Errorf("after a connection that lasted a minute = %s, want 1s", got)
	}
	if got := b.after(0); got != 2*time.Second {
		t.Errorf("the dial after the reset's = %s, want 2s", got)
	}
}

func TestListenSocket(t *testing.T) {
	t.Run("mode 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hosts.sock")
		ln, err := ListenSocket(path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %s", fi.Mode())
		}
	})
	t.Run("a stale socket removed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hosts.sock")
		stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		stale.SetUnlinkOnClose(false)
		_ = stale.Close()
		var d net.Dialer
		if _, err := d.DialContext(t.Context(), "unix", path); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("dial to the stale socket = %v, want connection refused", err)
		}
		ln, err := ListenSocket(path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		c, err := d.DialContext(t.Context(), "unix", path)
		if err != nil {
			t.Fatalf("dial after the listen = %v", err)
		}
		_ = c.Close()
	})
	t.Run("closing removes only its own socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hosts.sock")
		first, err := ListenSocket(path)
		if err != nil {
			t.Fatal(err)
		}
		second, err := ListenSocket(path)
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		_ = first.Close()
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("the newer socket after the older closed: %v", err)
		}
		_ = second.Close()
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the socket after its listener closed: %v", err)
		}
	})
}
