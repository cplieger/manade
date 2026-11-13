package docker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/go/httpclient"
)

func TestDial(t *testing.T) {
	t.Run("negotiatesTheDaemonsLowerVersion", func(t *testing.T) {
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
	t.Run("capsANewerDaemonAtTheClientsVersion", func(t *testing.T) {
		c := newDaemon(t, "1.56").dial(t)
		if got := c.APIVersion(); got != "1.51" {
			t.Errorf("APIVersion = %q, want 1.51", got)
		}
	})
	// A pull's headers come only with its first progress line, so only an
	// API call is cut by the header wait.
	t.Run("waitsForAStreamsHeadersOnlyOnItsContext", func(t *testing.T) {
		prev := headerTimeout
		headerTimeout = 50 * time.Millisecond
		t.Cleanup(func() { headerTimeout = prev })
		d := newDaemon(t, "1.43")
		late := func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"status":"Status: Downloaded newer image"}` + "\n"))
		}
		d.handle("POST /images/create", late)
		d.handle("GET /info", late)
		c := d.dial(t)
		if err := c.PullImage(t.Context(), "slow:1"); err != nil {
			t.Errorf("PullImage with headers after the API header wait = %v, want success", err)
		}
		if _, err := c.Info(t.Context()); err == nil {
			t.Error("Info with headers after the header wait succeeded, want it cut")
		}
	})
	// A loaded daemon answers a create or start late, so only those wait
	// past the API header bound, up to their own.
	t.Run("waitsLongerForACreateOrStart", func(t *testing.T) {
		prevAPI, prevStart := headerTimeout, startHeaderTimeout
		headerTimeout, startHeaderTimeout = 50*time.Millisecond, 400*time.Millisecond
		t.Cleanup(func() { headerTimeout, startHeaderTimeout = prevAPI, prevStart })
		d := newDaemon(t, "1.43")
		var delay atomic.Int64
		delay.Store(int64(200 * time.Millisecond))
		d.handle("POST /containers/create", func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Duration(delay.Load()))
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c-late"})
		})
		d.handle("POST /containers/{id}/start", func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Duration(delay.Load()))
			w.WriteHeader(http.StatusNoContent)
		})
		d.handle("GET /containers/{id}/json", func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(time.Duration(delay.Load()))
			writeJSON(w, http.StatusOK, map[string]any{"Id": "c-late"})
		})
		c := d.dial(t)
		if id, err := c.CreateContainer(t.Context(), "late", &CreateRequest{Image: "x"}); err != nil || id != "c-late" {
			t.Errorf("CreateContainer answered after the API header wait = %q, %v; want c-late", id, err)
		}
		if err := c.StartContainer(t.Context(), "c-late"); err != nil {
			t.Errorf("StartContainer answered after the API header wait = %v, want success", err)
		}
		if _, _, err := c.InspectContainer(t.Context(), "c-late"); err == nil {
			t.Error("InspectContainer answered after the API header wait succeeded, want it cut")
		}
		delay.Store(int64(600 * time.Millisecond))
		err := c.StartContainer(t.Context(), "c-late")
		if err == nil || !Unanswered(err) {
			t.Errorf("StartContainer answered after its own header wait = %v, want an unanswered error", err)
		}
	})
	t.Run("acceptsTheFloor", func(t *testing.T) {
		if got := newDaemon(t, "1.40").dial(t).APIVersion(); got != "1.40" {
			t.Errorf("APIVersion = %q, want 1.40", got)
		}
	})
	t.Run("refusesADaemonBelowTheFloor", func(t *testing.T) {
		d := newDaemon(t, "1.39")
		_, err := Dial(t.Context(), d.socket, "manade")
		if !errors.Is(err, errAPIVersion) || !strings.Contains(err.Error(), "1.39") {
			t.Errorf("Dial = %v, want errAPIVersion naming 1.39", err)
		}
	})
	t.Run("refusesADaemonWhoseMinimumIsAboveTheClient", func(t *testing.T) {
		d := newDaemonRange(t, "1.60", "1.55")
		_, err := Dial(t.Context(), d.socket, "manade")
		if !errors.Is(err, errAPIVersion) || !strings.Contains(err.Error(), "1.55") {
			t.Errorf("Dial = %v, want errAPIVersion naming 1.55", err)
		}
	})
	t.Run("refusesAMalformedVersion", func(t *testing.T) {
		d := newDaemon(t, "one.forty\n")
		if _, err := Dial(t.Context(), d.socket, "manade"); !errors.Is(err, errAPIVersion) || strings.Contains(err.Error(), "\n") {
			t.Errorf("Dial = %v, want errAPIVersion on one line", err)
		}
	})
	t.Run("failsWhenTheSocketIsAbsent", func(t *testing.T) {
		if _, err := Dial(t.Context(), "/nonexistent/docker.sock", "manade"); err == nil {
			t.Error("Dial on a missing socket succeeded")
		}
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
	_, _, err := c.InspectContainer(t.Context(), "conflict")
	t.Run("conflictMatchesErrConflict", func(t *testing.T) {
		var ae *APIError
		if !errors.Is(err, ErrConflict) || errors.Is(err, errNotFound) || !errors.As(err, &ae) || ae.Code != 409 || ae.Op != "inspect" {
			t.Errorf("err = %v, want an inspect APIError 409 matching ErrConflict only", err)
		}
	})
	_, _, err = c.InspectContainer(t.Context(), "server")
	t.Run("messageIsOneSanitizedLine", func(t *testing.T) {
		if err == nil || strings.ContainsAny(err.Error(), "\n\x1b") || !strings.Contains(err.Error(), "line one") {
			t.Errorf("err = %q, want the daemon's text on one line without controls", err)
		}
	})
	t.Run("httpclientSentinelsMatch", func(t *testing.T) {
		if !errors.Is(err, httpclient.ErrServerError) {
			t.Errorf("err = %v, want httpclient.ErrServerError", err)
		}
	})
	_, _, err = c.InspectContainer(t.Context(), "plain")
	t.Run("nonJSONBodyFallsBackToTheStatusText", func(t *testing.T) {
		if err == nil || !strings.HasSuffix(err.Error(), "Bad Gateway") {
			t.Errorf("err = %v, want the status text", err)
		}
	})
}

func TestUnanswered(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") == "warned" {
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c-warned", "Warnings": []string{"PIDs limit discarded."}})
			return
		}
		writeError(w, http.StatusInternalServerError, "refused")
	})
	d.handle("DELETE /containers/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	c := d.dial(t)
	_, refused := c.CreateContainer(t.Context(), "refused", &CreateRequest{Image: "x"})
	_, warned := c.CreateContainer(t.Context(), "warned", &CreateRequest{Image: "x"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cut := c.StartContainer(ctx, "c1")
	for _, tc := range []struct {
		err  error
		name string
		want bool
	}{
		{name: "none", err: nil, want: false},
		{name: "answeredRefusal", err: refused, want: false},
		{name: "createWarnings", err: warned, want: false},
		{name: "cutCall", err: cut, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Unanswered(tc.err); got != tc.want {
				t.Errorf("Unanswered(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestCallCapsTheBody(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Driver":"` + strings.Repeat("x", jsonLimit) + `"}`))
	})
	var tooLarge *httpclient.ResponseTooLargeError
	if _, err := d.dial(t).Info(t.Context()); !errors.As(err, &tooLarge) {
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
