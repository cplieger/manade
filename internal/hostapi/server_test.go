package hostapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, b *fakeBackend) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(b, "inst-1", nil).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// reply is what a test reads of an answer whose body do has read and closed.
type reply struct {
	Header     http.Header
	StatusCode int
}

func do(t *testing.T, method, url, body string) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{Header: resp.Header, StatusCode: resp.StatusCode}, b
}

func requestJSON(t *testing.T, req RunnerRequest) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func errorOf(t *testing.T, b []byte) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("error answer %q: %v", b, err)
	}
	return e
}

func TestStatusCarriesTheInstanceID(t *testing.T) {
	srv := serve(t, &fakeBackend{status: Status{Version: "v1.0.0-dev.11", InstanceID: "stale"}})
	resp, b := do(t, http.MethodGet, srv.URL+"/v1/status", "")
	var st Status
	if err := json.Unmarshal(b, &st); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %q: %v", resp.StatusCode, b, err)
	}
	if st.InstanceID != "inst-1" || st.Version != "v1.0.0-dev.11" {
		t.Errorf("status = %+v", st)
	}
}

func TestCreateRunner(t *testing.T) {
	t.Run("accepted with the instance ID", func(t *testing.T) {
		b := &fakeBackend{}
		srv := serve(t, b)
		resp, body := do(t, http.MethodPost, srv.URL+"/v1/runners", requestJSON(t, validRequest()))
		var a RunnerAnswer
		if err := json.Unmarshal(body, &a); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("answer %d %q: %v", resp.StatusCode, body, err)
		}
		if a.InstanceID != "inst-1" || a.Runner.Name != validRequest().RunnerName || a.Runner.State != StateCreating {
			t.Errorf("answer = %+v", a)
		}
		if len(b.created) != 1 || b.created[0] != validRequest() {
			t.Errorf("backend got %+v", b.created)
		}
	})
	t.Run("a repeated POST answers the same runner", func(t *testing.T) {
		b := &fakeBackend{}
		srv := serve(t, b)
		do(t, http.MethodPost, srv.URL+"/v1/runners", requestJSON(t, validRequest()))
		resp, body := do(t, http.MethodPost, srv.URL+"/v1/runners", requestJSON(t, validRequest()))
		var a RunnerAnswer
		if err := json.Unmarshal(body, &a); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("answer %d %q: %v", resp.StatusCode, body, err)
		}
		if len(b.created) != 1 || a.Runner.Name != validRequest().RunnerName || a.InstanceID != "inst-1" {
			t.Errorf("created %d, answer %+v", len(b.created), a)
		}
	})
	refusals := []struct {
		err    error
		name   string
		code   string
		status int
	}{
		{name: "unknown profile", err: ErrUnknownProfile, status: http.StatusNotFound, code: codeUnknownProfile},
		{name: "refused profile", err: ErrProfileRefused, status: http.StatusForbidden, code: codeRefused},
		{name: "backend failure", err: errors.New("disk on fire"), status: http.StatusInternalServerError, code: codeInternal},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, &fakeBackend{err: tc.err})
			resp, body := do(t, http.MethodPost, srv.URL+"/v1/runners", requestJSON(t, validRequest()))
			if resp.StatusCode != tc.status || errorOf(t, body).Code != tc.code {
				t.Errorf("answer %d %s, want %d %s", resp.StatusCode, body, tc.status, tc.code)
			}
		})
	}
	t.Run("not ready is 409 with the profile and reason", func(t *testing.T) {
		srv := serve(t, &fakeBackend{err: &NotReadyError{Profile: "small", Reason: ReasonImageMissing, Detail: "pulling again"}})
		resp, body := do(t, http.MethodPost, srv.URL+"/v1/runners", requestJSON(t, validRequest()))
		e := errorOf(t, body)
		if resp.StatusCode != http.StatusConflict || e.Code != codeNotReady || e.Profile != "small" || e.Reason != ReasonImageMissing || e.Detail != "pulling again" {
			t.Errorf("answer %d %+v", resp.StatusCode, e)
		}
	})
}

func TestCreateRunnerRefusesABadBody(t *testing.T) {
	long := validRequest()
	long.JITConfig = strings.Repeat("a", jitLimit+1)
	cases := map[string]string{
		"oversize body":    `{"profile":"` + strings.Repeat("a", requestLimit) + `"}`,
		"unknown field":    strings.TrimSuffix(requestJSON(t, validRequest()), "}") + `,"network":"host"}`,
		"two requests":     requestJSON(t, validRequest()) + requestJSON(t, validRequest()),
		"not JSON":         "profile=small",
		"empty profile":    requestJSON(t, RunnerRequest{RunnerName: "r1", Scope: "a/b", RunnerID: 1, JITConfig: "x"}),
		"bad runner name":  requestJSON(t, RunnerRequest{Profile: "small", RunnerName: "../etc", Scope: "a/b", RunnerID: 1, JITConfig: "x"}),
		"long runner name": requestJSON(t, RunnerRequest{Profile: "small", RunnerName: strings.Repeat("r", 65), Scope: "a/b", RunnerID: 1, JITConfig: "x"}),
		"empty scope":      requestJSON(t, RunnerRequest{Profile: "small", RunnerName: "r1", RunnerID: 1, JITConfig: "x"}),
		"no runner ID":     requestJSON(t, RunnerRequest{Profile: "small", RunnerName: "r1", Scope: "a/b", JITConfig: "x"}),
		"empty JIT config": requestJSON(t, RunnerRequest{Profile: "small", RunnerName: "r1", Scope: "a/b", RunnerID: 1}),
		"oversize JIT":     requestJSON(t, long),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackend{}
			srv := serve(t, b)
			resp, answer := do(t, http.MethodPost, srv.URL+"/v1/runners", body)
			if resp.StatusCode != http.StatusBadRequest || errorOf(t, answer).Code != codeBadRequest {
				t.Errorf("answer %d %s, want 400", resp.StatusCode, answer)
			}
			if len(b.created) != 0 {
				t.Errorf("backend reached with %+v", b.created)
			}
		})
	}
}

func TestRunnersCarriesTheInstanceID(t *testing.T) {
	t.Run("a list", func(t *testing.T) {
		srv := serve(t, &fakeBackend{runners: []Runner{{Name: "r1", State: StateRunning, RunnerID: 3, Limits: Limits{CPUSet: "1", Memory: 1 << 30}, RemovedBy: ""}}})
		_, body := do(t, http.MethodGet, srv.URL+"/v1/runners", "")
		var l RunnerList
		if err := json.Unmarshal(body, &l); err != nil {
			t.Fatal(err)
		}
		if l.InstanceID != "inst-1" || len(l.Runners) != 1 || l.Runners[0].RunnerID != 3 || l.Runners[0].Limits.CPUSet != "1" {
			t.Errorf("list = %+v", l)
		}
	})
	t.Run("an empty list is an array", func(t *testing.T) {
		srv := serve(t, &fakeBackend{})
		_, body := do(t, http.MethodGet, srv.URL+"/v1/runners", "")
		if !strings.Contains(string(body), `"runners":[]`) || !strings.Contains(string(body), `"instance_id":"inst-1"`) {
			t.Errorf("list = %s", body)
		}
	})
}

func TestDeleteRunner(t *testing.T) {
	t.Run("no diag answers 204", func(t *testing.T) {
		b := &fakeBackend{}
		srv := serve(t, b)
		resp, _ := do(t, http.MethodDelete, srv.URL+"/v1/runners/r1", "")
		if resp.StatusCode != http.StatusNoContent || len(b.deleted) != 1 || b.deleted[0] != "r1" || b.diagAsk {
			t.Errorf("answer %d, deleted %v diag %v", resp.StatusCode, b.deleted, b.diagAsk)
		}
	})
	t.Run("diag asked and found answers the tar", func(t *testing.T) {
		b := &fakeBackend{diag: []byte("tar bytes")}
		srv := serve(t, b)
		resp, body := do(t, http.MethodDelete, srv.URL+"/v1/runners/r1?diag=1", "")
		if resp.StatusCode != http.StatusOK || string(body) != "tar bytes" || !b.diagAsk || resp.Header.Get("Content-Type") != "application/x-tar" {
			t.Errorf("answer %d %q diag %v", resp.StatusCode, body, b.diagAsk)
		}
	})
	t.Run("diag asked with no container answers 204", func(t *testing.T) {
		srv := serve(t, &fakeBackend{})
		resp, _ := do(t, http.MethodDelete, srv.URL+"/v1/runners/r1?diag=1", "")
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("answer %d, want 204", resp.StatusCode)
		}
	})
	t.Run("a failure is 500", func(t *testing.T) {
		srv := serve(t, &fakeBackend{err: errors.New("daemon gone")})
		resp, _ := do(t, http.MethodDelete, srv.URL+"/v1/runners/r1", "")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("answer %d, want 500", resp.StatusCode)
		}
	})
	t.Run("a path that names no runner", func(t *testing.T) {
		b := &fakeBackend{}
		srv := serve(t, b)
		resp, _ := do(t, http.MethodDelete, srv.URL+"/v1/runners/-r1", "")
		if resp.StatusCode != http.StatusBadRequest || len(b.deleted) != 0 {
			t.Errorf("answer %d, deleted %v", resp.StatusCode, b.deleted)
		}
	})
}

func TestLogs(t *testing.T) {
	t.Run("tail bounds", func(t *testing.T) {
		srv := serve(t, &fakeBackend{logs: []byte("a\n")})
		for _, q := range []string{"0", "1001", "x", "-1"} {
			resp, _ := do(t, http.MethodGet, srv.URL+"/v1/runners/r1/logs?tail="+q, "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("tail=%s answered %d, want 400", q, resp.StatusCode)
			}
		}
		resp, body := do(t, http.MethodGet, srv.URL+"/v1/runners/r1/logs?tail=1000", "")
		if resp.StatusCode != http.StatusOK || string(body) != "a\n" {
			t.Errorf("tail=1000 answered %d %q", resp.StatusCode, body)
		}
	})
	t.Run("an oversize tail keeps its last whole lines", func(t *testing.T) {
		big := strings.Repeat("x", logLimit) + "\nlast line\n"
		srv := serve(t, &fakeBackend{logs: []byte(big)})
		_, body := do(t, http.MethodGet, srv.URL+"/v1/runners/r1/logs", "")
		if string(body) != "last line\n" {
			t.Errorf("tail = %d bytes ending %q", len(body), body[max(0, len(body)-20):])
		}
	})
	t.Run("an unknown runner is 404", func(t *testing.T) {
		srv := serve(t, &fakeBackend{err: ErrUnknownRunner})
		resp, body := do(t, http.MethodGet, srv.URL+"/v1/runners/r1/logs", "")
		if resp.StatusCode != http.StatusNotFound || errorOf(t, body).Code != codeUnknownRunner {
			t.Errorf("answer %d %s", resp.StatusCode, body)
		}
	})
}

func TestDiagRefusesAnOversizeTar(t *testing.T) {
	srv := serve(t, &fakeBackend{diag: make([]byte, DiagLimit+1)})
	resp, _ := do(t, http.MethodGet, srv.URL+"/v1/runners/r1/diag", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("answer %d, want 500", resp.StatusCode)
	}
	srv = serve(t, &fakeBackend{diag: make([]byte, DiagLimit)})
	resp, body := do(t, http.MethodGet, srv.URL+"/v1/runners/r1/diag", "")
	if resp.StatusCode != http.StatusOK || len(body) != DiagLimit {
		t.Errorf("answer %d with %d bytes, want 200 with %d", resp.StatusCode, len(body), DiagLimit)
	}
}

func TestEvents(t *testing.T) {
	b := &fakeBackend{changes: make(chan Runner)}
	s := NewServer(b, "inst-1", nil)
	s.heartbeat = 20 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/events", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	next := func() Event {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("stream ended: %v", sc.Err())
		}
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	if ev := next(); ev.Type != EventHeartbeat || ev.InstanceID != "inst-1" {
		t.Errorf("first line = %+v, want a heartbeat", ev)
	}
	b.changes <- Runner{Name: "r1", State: StateExited}
	for {
		ev := next()
		if ev.Type == EventHeartbeat {
			continue
		}
		if ev.Type != EventRunner || ev.Runner == nil || ev.Runner.Name != "r1" || ev.Runner.State != StateExited || ev.InstanceID != "inst-1" {
			t.Errorf("event = %+v", ev)
		}
		break
	}
	close(b.changes)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || ev.Type != EventHeartbeat {
			t.Errorf("after the backend closed: %s", sc.Bytes())
		}
	}
}

func TestProbe(t *testing.T) {
	t.Run("a report carries the instance ID", func(t *testing.T) {
		srv := serve(t, &fakeBackend{})
		resp, body := do(t, http.MethodPost, srv.URL+"/v1/probe", "")
		var rep ProbeReport
		if err := json.Unmarshal(body, &rep); err != nil || resp.StatusCode != http.StatusOK || rep.InstanceID != "inst-1" || !rep.Proven {
			t.Errorf("answer %d %s", resp.StatusCode, body)
		}
	})
	t.Run("past the deadline is a probe timeout", func(t *testing.T) {
		srv := serve(t, &fakeBackend{deadline: 50 * time.Millisecond, probe: func(ctx context.Context) (ProbeReport, error) {
			<-ctx.Done()
			return ProbeReport{}, ctx.Err()
		}})
		resp, body := do(t, http.MethodPost, srv.URL+"/v1/probe", "")
		e := errorOf(t, body)
		if resp.StatusCode != http.StatusGatewayTimeout || e.Code != codeProbeTimeout || !strings.Contains(e.Message, "50ms") {
			t.Errorf("answer %d %+v", resp.StatusCode, e)
		}
	})
	t.Run("a backend failure is not a timeout", func(t *testing.T) {
		srv := serve(t, &fakeBackend{probe: func(context.Context) (ProbeReport, error) { return ProbeReport{}, errors.New("no daemon") }})
		resp, body := do(t, http.MethodPost, srv.URL+"/v1/probe", "")
		if resp.StatusCode != http.StatusInternalServerError || errorOf(t, body).Code != codeInternal {
			t.Errorf("answer %d %s", resp.StatusCode, body)
		}
	})
}

func TestUnknownRoutes(t *testing.T) {
	srv := serve(t, &fakeBackend{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v2/status"}, {http.MethodPost, "/v1/status"}, {http.MethodPut, "/v1/runners"},
	} {
		resp, _ := do(t, tc.method, srv.URL+tc.path, "")
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}
