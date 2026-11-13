package hostapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/config"
)

// APIError is an error answer the client has no sentinel for.
type APIError struct {
	Op      string
	Code    string
	Message string
	Status  int
}

func (e *APIError) Error() string {
	return "host API " + e.Op + ": status " + strconv.Itoa(e.Status) + " (" + e.Code + "): " + e.Message
}

// Client calls one host's API. It is safe for concurrent use.
type Client struct {
	api *http.Client
	// stream has no client timeout, for the events stream and the probe,
	// which their contexts bound.
	stream *http.Client
	base   string
}

// NewClient reaches host name at addr. An addr with HostPort needs tlsCfg,
// ClientTLS's config: the client verifies the host's certificate against
// name, never against the address's hostname, so the certificate binds the
// host wherever it moves.
func NewClient(name string, addr config.Address, tlsCfg *tls.Config) (*Client, error) {
	tr := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	var base string
	switch {
	case addr.Socket != "":
		socket := addr.Socket
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		base = "http://" + name
	case addr.HostPort != "" && tlsCfg != nil:
		cfg := tlsCfg.Clone()
		cfg.ServerName = name
		tr.TLSClientConfig = cfg
		tr.ForceAttemptHTTP2 = false
		base = "https://" + addr.HostPort
	case addr.HostPort != "":
		return nil, fmt.Errorf("host %s: an https address needs the orchestrator's TLS files", name)
	default:
		return nil, fmt.Errorf("host %s: no address", name)
	}
	return &Client{
		api:    &http.Client{Transport: tr, Timeout: deleteBound + 2*clientSlack, CheckRedirect: httpclient.RefuseAllRedirects},
		stream: &http.Client{Transport: tr, CheckRedirect: httpclient.RefuseAllRedirects},
		base:   base,
	}, nil
}

// CloseIdleConnections closes the client's idle connections.
func (c *Client) CloseIdleConnections() { c.api.CloseIdleConnections() }

// Status reads the host's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.callJSON(ctx, "status", quickTimeout, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// CreateRunner asks the host to start a runner; repeating it for the same
// runner answers the record the host holds. It fails with
// ErrUnknownProfile, ErrProfileRefused or a *NotReadyError, after which the
// host has created nothing.
func (c *Client) CreateRunner(ctx context.Context, req *RunnerRequest) (RunnerAnswer, error) {
	var a RunnerAnswer
	err := c.callJSON(ctx, "create runner", quickTimeout, http.MethodPost, "/v1/runners", req, &a)
	return a, err
}

// Runners lists every runner the host holds.
func (c *Client) Runners(ctx context.Context) (RunnerList, error) {
	var l RunnerList
	err := c.callJSON(ctx, "list runners", callBound, http.MethodGet, "/v1/runners", nil, &l)
	return l, err
}

// DeleteRunner removes the runner's container, any orphan under it and its
// record; with diag it answers the runner's _diag tar first, nil when there
// is none.
func (c *Client) DeleteRunner(ctx context.Context, name string, diag bool) ([]byte, error) {
	path := "/v1/runners/" + url.PathEscape(name)
	if diag {
		path += "?diag=1"
	}
	b, status, err := c.callBytes(ctx, "delete runner", deleteBound, http.MethodDelete, path, DiagLimit)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	return b, nil
}

// Logs answers the runner's last tail lines, or fails with ErrUnknownRunner.
func (c *Client) Logs(ctx context.Context, name string, tail int) ([]byte, error) {
	path := "/v1/runners/" + url.PathEscape(name) + "/logs?tail=" + strconv.Itoa(tail)
	b, _, err := c.callBytes(ctx, "runner logs", callBound, http.MethodGet, path, logLimit)
	return b, err
}

// Diag answers the runner's _diag tar, or fails with ErrUnknownRunner.
func (c *Client) Diag(ctx context.Context, name string) ([]byte, error) {
	b, _, err := c.callBytes(ctx, "runner diag", callBound, http.MethodGet, "/v1/runners/"+url.PathEscape(name)+"/diag", DiagLimit)
	return b, err
}

// Probe runs the host's egress probe. deadline is the host's reported
// ProbeDeadline; a run that passes it fails with ErrProbeTimeout.
func (c *Client) Probe(ctx context.Context, deadline time.Duration) (ProbeReport, error) {
	if deadline <= 0 {
		return ProbeReport{}, errors.New("host API probe: the host reported no probe deadline")
	}
	ctx, cancel := context.WithTimeout(ctx, deadline+clientSlack)
	defer cancel()
	body, _, err := c.send(ctx, c.stream, "probe", http.MethodPost, "/v1/probe", nil)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
			return ProbeReport{}, fmt.Errorf("%w of %s", ErrProbeTimeout, deadline)
		}
		return ProbeReport{}, err
	}
	var rep ProbeReport
	err = decodeAnswer(body, "probe", &rep)
	return rep, err
}

// Events calls fn with each line of the host's events stream until ctx
// ends, the host ends the stream (nil) or the link fails. A stream that
// stays silent past three heartbeats is a failed link.
func (c *Client) Events(ctx context.Context, fn func(*Event)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errSilent := fmt.Errorf("host API events: no line for %s", 3*Heartbeat)
	watchdog := time.AfterFunc(3*Heartbeat, func() { cancel(errSilent) })
	defer watchdog.Stop()
	body, _, err := c.send(ctx, c.stream, "events", http.MethodGet, "/v1/events", nil)
	if err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	defer httpclient.DrainClose(body)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), eventLimit)
	for sc.Scan() {
		watchdog.Reset(3 * Heartbeat)
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return fmt.Errorf("host API events: %w", err)
		}
		fn(&ev)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("host API events: %w", err)
	}
	return nil
}

// callJSON sends in (nil for no body) and decodes the answer into out,
// bounded by the server's bound plus clientSlack.
func (c *Client) callJSON(ctx context.Context, op string, bound time.Duration, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, bound+clientSlack)
	defer cancel()
	body, _, err := c.send(ctx, c.api, op, method, path, in)
	if err != nil {
		return err
	}
	return decodeAnswer(body, op, out)
}

// callBytes answers a body of at most limit bytes and its status.
func (c *Client) callBytes(ctx context.Context, op string, bound time.Duration, method, path string, limit int64) (data []byte, status int, err error) {
	ctx, cancel := context.WithTimeout(ctx, bound+clientSlack)
	defer cancel()
	body, status, err := c.send(ctx, c.api, op, method, path, nil)
	if err != nil {
		return nil, 0, err
	}
	defer httpclient.DrainClose(body)
	if data, err = httpclient.ReadLimitedBody(body, limit); err != nil {
		return nil, 0, fmt.Errorf("host API %s: %w", op, err)
	}
	return data, status, nil
}

// send answers a 2xx answer's body, which the caller closes, and status, or
// the error answer's error.
func (c *Client) send(ctx context.Context, hc *http.Client, op, method, path string, in any) (io.ReadCloser, int, error) {
	var body io.Reader = http.NoBody
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, 0, fmt.Errorf("host API %s: %w", op, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, 0, fmt.Errorf("host API %s: %w", op, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req) //nolint:bodyclose // a 2xx body goes to the caller, who closes it; any other is drained below
	if err != nil {
		return nil, 0, fmt.Errorf("host API %s: %w", op, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.Body, resp.StatusCode, nil
	}
	defer httpclient.DrainClose(resp.Body)
	return nil, 0, answerError(op, resp)
}

func decodeAnswer(body io.ReadCloser, op string, out any) error {
	defer httpclient.DrainClose(body)
	b, err := httpclient.ReadLimitedBody(body, answerLimit)
	if err != nil {
		return fmt.Errorf("host API %s: %w", op, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("host API %s: decoding the answer: %w", op, err)
	}
	return nil
}

// answerError maps an error answer back to the error the host's backend
// failed with.
func answerError(op string, resp *http.Response) error {
	var body errorBody
	b, err := httpclient.ReadLimitedBody(resp.Body, answerLimit)
	if err == nil {
		err = json.Unmarshal(b, &body)
	}
	if err != nil || body.Code == "" {
		return &APIError{Op: op, Status: resp.StatusCode, Code: codeInternal, Message: http.StatusText(resp.StatusCode)}
	}
	msg, _ := textsafe.SanitizeSingleLineCapped(body.Message, errorTextLimit, "…")
	switch body.Code {
	case codeNotReady:
		detail, _ := textsafe.SanitizeSingleLineCapped(body.Detail, errorTextLimit, "…")
		return &NotReadyError{Profile: body.Profile, Reason: body.Reason, Detail: detail}
	case codeUnknownProfile:
		return fmt.Errorf("%w: %s", ErrUnknownProfile, msg)
	case codeRefused:
		return fmt.Errorf("%w: %s", ErrProfileRefused, msg)
	case codeUnknownRunner:
		return fmt.Errorf("%w: %s", ErrUnknownRunner, msg)
	case codeProbeTimeout:
		return fmt.Errorf("%w: %s", ErrProbeTimeout, msg)
	}
	return &APIError{Op: op, Status: resp.StatusCode, Code: body.Code, Message: msg}
}
