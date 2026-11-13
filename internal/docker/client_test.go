package docker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/httpclient"
)

func TestDial(t *testing.T) {
	t.Run("negotiates the daemon's lower version", func(t *testing.T) {
		d := newDaemon(t, "1.43")
		d.handle("GET /images/x/json", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusNotFound, "no such image") })
		c := d.dial(t)
		if got := c.APIVersion(); got != "1.43" {
			t.Errorf("APIVersion = %q, want 1.43", got)
		}
		if _, _, err := c.InspectImage(t.Context(), "x"); err != nil {
			t.Fatalf("InspectImage: %v", err)
		}
		if v := d.seen(http.MethodGet, "/version"); len(v) != 1 || v[0].version != "" {
			t.Errorf("/version requests = %+v, want one unversioned", v)
		}
		if p := d.seen(http.MethodGet, "/images/x/json"); len(p) != 1 || p[0].version != "1.43" {
			t.Errorf("/images/x/json requests = %+v, want one under /v1.43/", p)
		}
	})
	t.Run("caps a newer daemon at the client's version", func(t *testing.T) {
		if got := newDaemon(t, "1.56").dial(t).APIVersion(); got != "1.51" {
			t.Errorf("APIVersion = %q, want 1.51", got)
		}
	})
	t.Run("accepts the floor", func(t *testing.T) {
		if got := newDaemon(t, "1.40").dial(t).APIVersion(); got != "1.40" {
			t.Errorf("APIVersion = %q, want 1.40", got)
		}
	})
	t.Run("refuses a daemon below the floor", func(t *testing.T) {
		d := newDaemon(t, "1.39")
		_, err := dialWith(t.Context(), d.ln.dial, nil)
		if !errors.Is(err, errAPIVersion) || !strings.Contains(err.Error(), "1.39") {
			t.Errorf("Dial = %v, want errAPIVersion naming 1.39", err)
		}
	})
	t.Run("refuses a daemon whose minimum is above the client", func(t *testing.T) {
		d := newDaemonRange(t, "1.60", "1.55")
		_, err := dialWith(t.Context(), d.ln.dial, nil)
		if !errors.Is(err, errAPIVersion) || !strings.Contains(err.Error(), "1.55") {
			t.Errorf("Dial = %v, want errAPIVersion naming 1.55", err)
		}
	})
	t.Run("refuses a malformed version", func(t *testing.T) {
		d := newDaemon(t, "one.forty\n")
		if _, err := dialWith(t.Context(), d.ln.dial, nil); !errors.Is(err, errAPIVersion) || strings.Contains(err.Error(), "\n") {
			t.Errorf("Dial = %v, want errAPIVersion on one line", err)
		}
	})
}

func TestDialUnixSocket(t *testing.T) {
	// A short directory keeps the socket path under the 108-byte limit.
	dir, err := os.MkdirTemp("", "mdk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ApiVersion": "1.47", "MinAPIVersion": "1.24"})
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := Dial(t.Context(), socket, nil)
	if err != nil || c.APIVersion() != "1.47" {
		t.Fatalf("Dial = %v", err)
	}
	c.Close()
	if _, err := Dial(t.Context(), filepath.Join(dir, "absent.sock"), nil); err == nil {
		t.Error("Dial on a missing socket succeeded")
	}
}

func TestCallTimeout(t *testing.T) {
	answer := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ServerVersion": "29.0.0", "Driver": "overlay2"})
	}
	t.Run("a slow answer inside the call timeout arrives", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("GET /info", after(CallTimeout-time.Second, answer))
			info, err := d.dial(t).Info(t.Context())
			if err != nil || info.ServerVersion != "29.0.0" || info.Driver != "overlay2" {
				t.Errorf("Info = %+v, %v", info, err)
			}
		})
	})
	t.Run("an answer past it is cut at it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("GET /info", hold)
			c := d.dial(t)
			start := time.Now()
			_, err := c.Info(t.Context())
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != CallTimeout {
				t.Errorf("Info = %v after %s, want a deadline at %s", err, time.Since(start), CallTimeout)
			}
		})
	})
	t.Run("a pull is bounded by its own timeout, not the call's", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /images/create", after(10*time.Minute, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"Status: Downloaded newer image"}` + "\n"))
			}))
			if err := d.dial(t).PullImage(t.Context(), "slow:1", ""); err != nil {
				t.Errorf("PullImage answered after 10 minutes = %v, want success", err)
			}
		})
	})
}

func TestAPIError(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "conflict":
			writeError(w, http.StatusConflict, "removal in progress")
		case "server":
			writeError(w, http.StatusInternalServerError, "line one\nline two\x1b[31m")
		case "plain":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>"))
		}
	})
	c := d.dial(t)
	t.Run("a conflict matches ErrConflict", func(t *testing.T) {
		_, _, err := c.InspectContainer(t.Context(), "conflict")
		ae, ok := errors.AsType[*APIError](err)
		if !errors.Is(err, ErrConflict) || errors.Is(err, errNotFound) || !ok || ae.Code != 409 || ae.Op != "inspect" {
			t.Errorf("err = %v, want an inspect APIError 409 matching ErrConflict only", err)
		}
	})
	t.Run("the message is one sanitized line", func(t *testing.T) {
		_, _, err := c.InspectContainer(t.Context(), "server")
		if err == nil || strings.ContainsAny(err.Error(), "\n\x1b") || !strings.Contains(err.Error(), "line one") {
			t.Errorf("err = %q, want the daemon's text on one line without controls", err)
		}
		if !errors.Is(err, httpclient.ErrServerError) {
			t.Errorf("err = %v, want httpclient.ErrServerError", err)
		}
	})
	t.Run("a body that is not JSON falls back to the status text", func(t *testing.T) {
		_, _, err := c.InspectContainer(t.Context(), "plain")
		if err == nil || !strings.HasSuffix(err.Error(), "Bad Gateway") {
			t.Errorf("err = %v, want the status text", err)
		}
	})
	t.Run("a missing image is told apart from another 404", func(t *testing.T) {
		missing := &APIError{Code: http.StatusNotFound, Message: "No such image: ghcr.io/x@sha256:1"}
		other := &APIError{Code: http.StatusNotFound, Message: "No such container: r1"}
		fault := &APIError{Code: http.StatusInternalServerError, Message: "pull: no such image in the mirror"}
		if !errors.Is(missing, ErrImageMissing) || errors.Is(other, ErrImageMissing) || !errors.Is(other, errNotFound) || errors.Is(fault, ErrImageMissing) {
			t.Errorf("missing image %v, other 404 %v, a 500 %v", errors.Is(missing, ErrImageMissing), errors.Is(other, ErrImageMissing), errors.Is(fault, ErrImageMissing))
		}
	})
}

func TestCallCapsTheBody(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Driver":"` + strings.Repeat("x", jsonLimit) + `"}`))
	})
	if _, err := d.dial(t).Info(t.Context()); !errors.As(err, new(*httpclient.ResponseTooLargeError)) {
		t.Errorf("Info = %v, want ResponseTooLargeError", err)
	}
}

func TestParseAPIVersion(t *testing.T) {
	for _, s := range []string{"", "1", "1.", ".40", "1.x", "-1.40", "1.-4", "+1.40", "1.40.1", "32768.0", "1.65536"} {
		if _, err := parseAPIVersion(s); err == nil {
			t.Errorf("parseAPIVersion(%q) succeeded", s)
		}
	}
	v, err := parseAPIVersion("1.9")
	w, _ := parseAPIVersion("1.10")
	if err != nil || v >= w || v.String() != "1.9" {
		t.Errorf("parseAPIVersion(1.9) = %v, %v; want 1.9, ordered below 1.10", v, err)
	}
}
