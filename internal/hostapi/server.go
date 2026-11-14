package hostapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/cplieger/go/textsafe"
)

// Backend is what the server serves: the supervisor. Each method answers
// from the host's own state; the server adds the instance ID, the bounds and
// the wire form.
type Backend interface {
	Status(ctx context.Context) Status
	// CreateRunner validates req against the host's own profile, records
	// the runner and answers at once, before any daemon call. A runner the
	// host already holds answers its record and starts nothing. It fails
	// with ErrUnknownProfile, ErrProfileRefused or a *NotReadyError.
	CreateRunner(ctx context.Context, req *RunnerRequest) (Runner, error)
	Runners(ctx context.Context) ([]Runner, error)
	// DeleteRunner stops and removes the runner's container and any orphan
	// under it and drops its record; with diag it first copies the runner's
	// _diag folder as a tar, nil when there is none. A runner with no
	// container is no error. It waits out a refused start's own removal,
	// within one docker.CallTimeout and 2 s, then makes at most seven daemon
	// calls, each within docker.CallTimeout, and gives the stop at most
	// DeleteStopGrace.
	DeleteRunner(ctx context.Context, name string, diag bool) ([]byte, error)
	// Logs answers the runner's last tail lines, or ErrUnknownRunner.
	Logs(ctx context.Context, name string, tail int) ([]byte, error)
	// Diag answers the runner's _diag folder as a tar of at most DiagLimit
	// bytes, or ErrUnknownRunner.
	Diag(ctx context.Context, name string) ([]byte, error)
	// Subscribe sends each runner whose state changes until ctx ends, then
	// closes the channel; it may close it early for a reader that lags.
	Subscribe(ctx context.Context) <-chan Runner
	// Probe runs the egress probe, after any run in progress ends.
	Probe(ctx context.Context) (ProbeReport, error)
	// ProbeDeadline bounds a probe request: two runs of the current plan.
	ProbeDeadline() time.Duration
}

// The errors a Backend fails with, each with its own answer.
var (
	ErrUnknownProfile = errors.New("the host's config defines no such profile")
	ErrProfileRefused = errors.New("the profile is refused on this host")
	ErrUnknownRunner  = errors.New("the host holds no such runner")
	// ErrProbeTimeout is a probe request that passed its deadline.
	ErrProbeTimeout = errors.New("the probe request passed its deadline")
)

// NotReadyError is a runner refused because its profile is not ready.
type NotReadyError struct {
	Profile string
	Reason  Reason
	Detail  string
}

func (e *NotReadyError) Error() string {
	s := "profile " + e.Profile + " is not ready on this host (" + string(e.Reason) + ")"
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// The codes an error answer carries, which the client maps back.
const (
	codeBadRequest     = "bad_request"
	codeUnknownProfile = "unknown_profile"
	codeRefused        = "profile_refused"
	codeNotReady       = "not_ready"
	codeUnknownRunner  = "unknown_runner"
	codeProbeTimeout   = "probe_timeout"
	codeInternal       = "internal"
)

// errorBody is every error answer.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Profile string `json:"profile,omitempty"`
	Reason  Reason `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// errorTextLimit caps an error answer's message.
const errorTextLimit = 1024

// runnerNameRe is a container name Docker accepts that fits GitHub's 64
// bytes for a runner name.
var runnerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Server serves the host API for one supervisor run.
type Server struct {
	backend    Backend
	log        *slog.Logger
	instanceID string
	// heartbeat is Heartbeat, a test seam.
	heartbeat time.Duration
}

// NewServer serves backend, stamping instanceID on every answer that names
// runners or the host. log may be nil for slog.Default.
func NewServer(backend Backend, instanceID string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{backend: backend, instanceID: instanceID, log: log, heartbeat: Heartbeat}
}

// Serve answers on ln until ctx ends, then closes ln and waits up to
// quickTimeout for the requests in flight, whose contexts end with ctx.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       quickTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return fmt.Errorf("host API: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quickTimeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	if serr := <-done; !errors.Is(serr, http.ErrServerClosed) {
		err = errors.Join(err, serr)
	}
	if err != nil {
		return fmt.Errorf("host API shutdown: %w", err)
	}
	return nil
}

// handler answers every route under /v1.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/status", s.bounded(quickTimeout, s.status))
	mux.Handle("POST /v1/runners", s.bounded(quickTimeout, s.createRunner))
	mux.Handle("GET /v1/runners", s.bounded(callBound, s.runners))
	mux.Handle("DELETE /v1/runners/{name}", s.bounded(deleteBound, s.deleteRunner))
	mux.Handle("GET /v1/runners/{name}/logs", s.bounded(callBound, s.logs))
	mux.Handle("GET /v1/runners/{name}/diag", s.bounded(callBound, s.diag))
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("POST /v1/probe", s.probe)
	return mux
}

// bounded cuts h's context and its answer's write at bound.
func (s *Server) bounded(bound time.Duration, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), bound)
		defer cancel()
		s.writeDeadline(w, bound+quickTimeout)
		h(w, r.WithContext(ctx))
	})
}

func (s *Server) writeDeadline(w http.ResponseWriter, d time.Duration) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.log.Warn("The host API could not bound an answer's write", "error", err)
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st := s.backend.Status(r.Context())
	st.InstanceID = s.instanceID
	s.writeJSON(w, http.StatusOK, &st)
}

func (s *Server) createRunner(w http.ResponseWriter, r *http.Request) {
	var req RunnerRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, &errorBody{Code: codeBadRequest, Message: err.Error()})
		return
	}
	if err := checkRequest(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, &errorBody{Code: codeBadRequest, Message: err.Error()})
		return
	}
	run, err := s.backend.CreateRunner(r.Context(), &req)
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, &RunnerAnswer{InstanceID: s.instanceID, Runner: run})
}

// checkRequest refuses a request no profile could serve.
func checkRequest(req *RunnerRequest) error {
	var errs []error
	if req.Profile == "" {
		errs = append(errs, errors.New("profile is empty"))
	}
	if !runnerNameRe.MatchString(req.RunnerName) {
		errs = append(errs, fmt.Errorf("runner_name %q is not a runner name", textsafe.SanitizeSingleLineBounded(req.RunnerName, 80)))
	}
	if req.Scope == "" {
		errs = append(errs, errors.New("scope is empty"))
	}
	if req.RunnerID <= 0 {
		errs = append(errs, errors.New("runner_id is not a runner ID"))
	}
	if req.JITConfig == "" || len(req.JITConfig) > jitLimit {
		errs = append(errs, fmt.Errorf("jit_config is empty or over %d bytes", jitLimit))
	}
	return errors.Join(errs...)
}

func (s *Server) runners(w http.ResponseWriter, r *http.Request) {
	list, err := s.backend.Runners(r.Context())
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	if list == nil {
		list = []Runner{}
	}
	s.writeJSON(w, http.StatusOK, &RunnerList{InstanceID: s.instanceID, Runners: list})
}

func (s *Server) deleteRunner(w http.ResponseWriter, r *http.Request) {
	name, ok := s.runnerName(w, r)
	if !ok {
		return
	}
	diag, err := s.backend.DeleteRunner(r.Context(), name, r.URL.Query().Get("diag") == "1")
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	if len(diag) > DiagLimit {
		s.log.Warn("A deleted runner's _diag tar passed the limit and was dropped", "runner", name, "bytes", len(diag), "limit", DiagLimit)
		diag = nil
	}
	if diag == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeBytes(w, "application/x-tar", diag, DiagLimit)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	name, ok := s.runnerName(w, r)
	if !ok {
		return
	}
	tail := LogTailDefault
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > LogTailMax {
			s.writeJSON(w, http.StatusBadRequest, &errorBody{Code: codeBadRequest, Message: fmt.Sprintf("tail must be 1 to %d", LogTailMax)})
			return
		}
		tail = n
	}
	b, err := s.backend.Logs(r.Context(), name, tail)
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	if cut := len(b) - logLimit; cut > 0 {
		onALine := b[cut-1] == '\n'
		b = b[cut:]
		if !onALine {
			_, b, _ = bytes.Cut(b, []byte("\n"))
		}
	}
	s.writeBytes(w, "text/plain; charset=utf-8", b, logLimit)
}

func (s *Server) diag(w http.ResponseWriter, r *http.Request) {
	name, ok := s.runnerName(w, r)
	if !ok {
		return
	}
	b, err := s.backend.Diag(r.Context(), name)
	if err != nil {
		s.writeBackendError(w, err)
		return
	}
	s.writeBytes(w, "application/x-tar", b, DiagLimit)
}

func (s *Server) runnerName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !runnerNameRe.MatchString(name) {
		s.writeJSON(w, http.StatusBadRequest, &errorBody{Code: codeBadRequest, Message: "the path names no runner"})
		return "", false
	}
	return name, true
}

// events streams one JSON line per runner change, and a heartbeat line every
// heartbeat, until the client leaves, the host stops or eventsBound passes.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), eventsBound)
	defer cancel()
	rc := http.NewResponseController(w)
	changes := s.backend.Subscribe(ctx)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	write := func(ev *Event) bool {
		s.writeDeadline(w, quickTimeout)
		return enc.Encode(ev) == nil && rc.Flush() == nil
	}
	if !write(&Event{Type: EventHeartbeat, Time: time.Now(), InstanceID: s.instanceID}) {
		return
	}
	for {
		var ev Event
		select {
		case <-ctx.Done():
			return
		case run, open := <-changes:
			if !open {
				return
			}
			ev = Event{Type: EventRunner, Runner: &run}
		case <-tick.C:
			ev = Event{Type: EventHeartbeat}
		}
		ev.Time, ev.InstanceID = time.Now(), s.instanceID
		if !write(&ev) {
			return
		}
	}
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	bound := s.backend.ProbeDeadline()
	ctx, cancel := context.WithTimeout(r.Context(), bound)
	defer cancel()
	s.writeDeadline(w, bound+quickTimeout)
	rep, err := s.backend.Probe(ctx)
	if err == nil {
		rep.InstanceID = s.instanceID
		s.writeJSON(w, http.StatusOK, &rep)
		return
	}
	if ctx.Err() != nil && r.Context().Err() == nil {
		err = fmt.Errorf("%w of %s", ErrProbeTimeout, bound)
	}
	s.writeBackendError(w, err)
}

// decodeBody reads one JSON object of at most requestLimit bytes, refusing
// unknown fields and trailing data.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, requestLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the body is not a request: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the body holds more than one request")
	}
	return nil
}

func (s *Server) writeBackendError(w http.ResponseWriter, err error) {
	msg, _ := textsafe.SanitizeSingleLineCapped(err.Error(), errorTextLimit, "…")
	body := errorBody{Message: msg}
	var status int
	nr, isNotReady := errors.AsType[*NotReadyError](err)
	switch {
	case isNotReady:
		status, body.Code, body.Profile, body.Reason = http.StatusConflict, codeNotReady, nr.Profile, nr.Reason
		body.Detail, _ = textsafe.SanitizeSingleLineCapped(nr.Detail, errorTextLimit, "…")
	case errors.Is(err, ErrUnknownProfile):
		status, body.Code = http.StatusNotFound, codeUnknownProfile
	case errors.Is(err, ErrProfileRefused):
		status, body.Code = http.StatusForbidden, codeRefused
	case errors.Is(err, ErrUnknownRunner):
		status, body.Code = http.StatusNotFound, codeUnknownRunner
	case errors.Is(err, ErrProbeTimeout):
		status, body.Code = http.StatusGatewayTimeout, codeProbeTimeout
	default:
		status, body.Code = http.StatusInternalServerError, codeInternal
		s.log.Warn("A host API request failed", "error", err)
	}
	s.writeJSON(w, status, &body)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.log.Error("The host API could not encode an answer", "error", err)
		http.Error(w, "encoding the answer failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeBytes answers b, refusing one over limit rather than cutting it: a
// cut tar would read as a whole one.
func (s *Server) writeBytes(w http.ResponseWriter, contentType string, b []byte, limit int) {
	if len(b) > limit {
		s.writeBackendError(w, fmt.Errorf("the answer passes %d bytes", limit))
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b) //nolint:gosec // G705: a log tail or tar, served as text/plain or a tar with nosniff to the orchestrator, never a browser
}
