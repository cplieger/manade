// Package docker is a narrow Docker Engine API client for a runner host's own
// daemon, over its Unix socket. It negotiates the API version at Dial, from a
// floor of 1.40, and every later request carries the negotiated version.
//
// Every call has its own CallTimeout and no client-wide header timeout, so a
// slow daemon is waited out and a call that passes its timeout is answered
// for what it is. Create and Start follow a timed-out call with an inspect
// and never judge a container that may still be starting as failed. The
// package stamps LabelManaged on every container and volume it creates, and
// their removals refuse one without it. Every answer is read through a size
// cap.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/go/textsafe"
)

// CallTimeout bounds each daemon call. Inside a host under load a create
// took up to 8.3 s and a start up to 45 s on a two-CPU machine; this is four
// times the slowest.
const CallTimeout = 3 * time.Minute

// The API versions this client speaks. minAPIVersion is the oldest that has
// every call and create field the host uses; maxAPIVersion is the newest whose
// reference this client was written against.
const (
	minAPIVersion apiVersion = 1<<16 | 40
	maxAPIVersion apiVersion = 1<<16 | 51

	// clientTimeout sits above CallTimeout so the call's context, not the
	// client, ends a slow call.
	clientTimeout = CallTimeout + 30*time.Second
	// pullTimeout bounds a pull, whose layers may be hundreds of megabytes.
	pullTimeout = 30 * time.Minute

	// jsonLimit caps a JSON answer; listLimit a list.
	jsonLimit = 4 << 20
	listLimit = 16 << 20
	// messageLimit caps one message of a pull, load or event stream.
	messageLimit = 1 << 20
	// errorTextLimit caps the daemon's error text kept in an APIError.
	errorTextLimit = 512
)

// Sentinels an APIError matches through errors.Is.
var (
	errNotFound = errors.New("docker: not found")
	ErrConflict = errors.New("docker: conflict")
	// ErrImageMissing is a create the daemon refused because its image is
	// not in the cache.
	ErrImageMissing = errors.New("docker: no such image")
)

// errAPIVersion is returned by Dial for a daemon whose API range does not
// overlap this client's.
var errAPIVersion = errors.New("docker: unsupported API version")

// APIError is a non-2xx answer. Message is the daemon's own text, sanitized
// to one line and capped.
type APIError struct {
	status  error
	Op      string
	Message string
	Code    int
}

func (e *APIError) Error() string {
	return "docker " + e.Op + ": status " + strconv.Itoa(e.Code) + ": " + e.Message
}

// Is matches errNotFound for a 404, ErrConflict for a 409 and
// ErrImageMissing for a 404 naming a missing image.
func (e *APIError) Is(target error) bool {
	switch target {
	case errNotFound:
		return e.Code == http.StatusNotFound
	case ErrConflict:
		return e.Code == http.StatusConflict
	case ErrImageMissing:
		return e.Code == http.StatusNotFound && strings.Contains(strings.ToLower(e.Message), "no such image")
	}
	return false
}

// Unwrap gives httpclient's status error, so its sentinels match too.
func (e *APIError) Unwrap() error { return e.status }

// dialer opens one connection to the daemon.
type dialer func(ctx context.Context) (net.Conn, error)

// Observer is told how long each call took the daemon to answer, by its op
// ("create", "start", ...), whether it succeeded or not. It must not block.
type Observer func(op string, took time.Duration)

// Client talks to one daemon at one negotiated version. It is safe for
// concurrent use.
type Client struct {
	api *http.Client
	// stream has no client timeout, for the pull and event streams and a
	// stop, which their contexts bound.
	stream  *http.Client
	dial    dialer
	observe Observer
	now     func() time.Time
	version apiVersion
}

// Dial connects to the daemon at socket, an absolute Unix socket path, and
// negotiates the version: the lower of the daemon's and maxAPIVersion. It
// fails with errAPIVersion when the daemon's newest version is below 1.40 or
// its oldest above maxAPIVersion. observe may be nil.
func Dial(ctx context.Context, socket string, observe Observer) (*Client, error) {
	return dialWith(ctx, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}, observe)
}

func dialWith(ctx context.Context, dial dialer, observe Observer) (*Client, error) {
	if observe == nil {
		observe = func(string, time.Duration) {}
	}
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:    8,
		IdleConnTimeout: 90 * time.Second,
	}
	c := &Client{
		api:     &http.Client{Transport: tr, Timeout: clientTimeout, CheckRedirect: httpclient.RefuseAllRedirects},
		stream:  &http.Client{Transport: tr, CheckRedirect: httpclient.RefuseAllRedirects},
		dial:    dial,
		observe: observe,
		now:     time.Now,
	}
	var v struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	if err := c.call(ctx, "version", http.MethodGet, "/version", nil, nil, &v, jsonLimit); err != nil {
		return nil, err
	}
	daemonMax, err := parseAPIVersion(v.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: the daemon reports %q", errAPIVersion, textsafe.SanitizeSingleLineBounded(v.APIVersion, 32))
	}
	if daemonMax < minAPIVersion {
		return nil, fmt.Errorf("%w: the daemon speaks up to %s, below the minimum %s (Docker Engine 19.03)", errAPIVersion, daemonMax, minAPIVersion)
	}
	c.version = min(daemonMax, maxAPIVersion)
	if daemonMin, err := parseAPIVersion(v.MinAPIVersion); err == nil && c.version < daemonMin {
		return nil, fmt.Errorf("%w: the daemon needs at least %s, above this client's %s", errAPIVersion, daemonMin, maxAPIVersion)
	}
	return c, nil
}

// APIVersion is the negotiated version, such as "1.43".
func (c *Client) APIVersion() string { return c.version.String() }

// Close closes the client's idle connections.
func (c *Client) Close() { c.api.CloseIdleConnections() }

// path is the versioned form of an unversioned API path, once Dial has set
// the version.
func (c *Client) path(p string) string {
	if c.version == 0 {
		return p
	}
	return "/v" + c.version.String() + p
}

// request sends one call and returns its answer's body, or an *APIError for
// a non-2xx answer. header may be nil. The caller closes the body.
func (c *Client) request(ctx context.Context, hc *http.Client, op, method, path string, query url.Values, body any, header http.Header) (io.ReadCloser, error) {
	var rd io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case io.Reader:
		rd, contentType = b, "application/x-tar"
	default:
		// body, not b: deadset follows a parameter forwarded to the encoder
		// and keeps the fields it writes, never a type switch's binding.
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("docker %s: %w", op, err)
		}
		rd, contentType = bytes.NewReader(raw), "application/json"
	}
	u := url.URL{Scheme: "http", Host: "docker", Path: c.path(path), RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", op, err)
	}
	maps.Copy(req.Header, header)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	start := time.Now()
	resp, err := hc.Do(req)
	c.observe(op, time.Since(start))
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", op, err)
	}
	if status := httpclient.CheckStatus(resp); status != nil {
		return nil, apiError(op, resp, status)
	}
	return resp.Body, nil
}

// apiError reads the daemon's {"message": ...} body and closes it.
func apiError(op string, resp *http.Response, status error) *APIError {
	defer httpclient.DrainClose(resp.Body)
	e := &APIError{Op: op, Code: resp.StatusCode, status: status}
	b, err := httpclient.ReadLimitedBody(resp.Body, jsonLimit)
	var m struct {
		Message string `json:"message"`
	}
	if err == nil && json.Unmarshal(b, &m) == nil {
		e.Message, _ = textsafe.SanitizeSingleLineCapped(m.Message, errorTextLimit, "…")
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// call sends a JSON call bounded by CallTimeout and decodes its answer into
// out, when out is not nil.
func (c *Client) call(ctx context.Context, op, method, path string, query url.Values, in, out any, limit int64) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return c.callIn(ctx, op, method, path, query, in, out, limit)
}

// callAuth is call with a registry credential, auth, sent when not "".
func (c *Client) callAuth(ctx context.Context, op, method, path, auth string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	body, err := c.request(ctx, c.api, op, method, path, nil, nil, authHeaders(auth))
	if err != nil {
		return err
	}
	defer httpclient.DrainClose(body)
	return decode(body, op, out, jsonLimit)
}

func authHeaders(auth string) http.Header {
	if auth == "" {
		return nil
	}
	return http.Header{authHeader: {auth}}
}

func decode(body io.ReadCloser, op string, out any, limit int64) error {
	b, err := httpclient.ReadLimitedBody(body, limit)
	if err != nil {
		return fmt.Errorf("docker %s: %w", op, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("docker %s: decoding the answer: %w", op, err)
	}
	return nil
}

// callIn is call within ctx's own deadline.
func (c *Client) callIn(ctx context.Context, op, method, path string, query url.Values, in, out any, limit int64) error {
	body, err := c.request(ctx, c.api, op, method, path, query, in, nil)
	if err != nil {
		return err
	}
	defer httpclient.DrainClose(body)
	if out == nil {
		return nil
	}
	return decode(body, op, out, limit)
}

// timed runs fn under its own CallTimeout and reports whether that timeout,
// rather than ctx or the daemon, ended it.
func timed(ctx context.Context, fn func(ctx context.Context) error) (timedOut bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	err = fn(cctx)
	return err != nil && ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded), err
}

// managedFilter is the filters parameter selecting the host's own objects,
// plus any extra filters.
func managedFilter(extra map[string][]string) url.Values {
	f := map[string][]string{"label": {LabelManaged + "=" + managedValue}}
	maps.Copy(f, extra)
	b, _ := json.Marshal(f) // a map of string slices cannot fail to marshal
	return url.Values{"filters": {string(b)}}
}

// managedLabels returns labels with the managed and created labels set over
// any the caller gave.
func (c *Client) managedLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+2)
	maps.Copy(out, labels)
	out[LabelManaged] = managedValue
	out[labelCreated] = c.now().UTC().Format(time.RFC3339)
	return out
}

// apiVersion is a Docker API version, major<<16 | minor, so versions order
// as integers.
type apiVersion uint64

func parseAPIVersion(s string) (apiVersion, error) {
	maj, minor, _ := strings.Cut(s, ".")
	a, err1 := strconv.ParseUint(maj, 10, 15)
	b, err2 := strconv.ParseUint(minor, 10, 16)
	if err1 != nil || err2 != nil {
		return 0, errors.New("malformed API version")
	}
	return apiVersion(a<<16 | b), nil
}

func (v apiVersion) String() string {
	return strconv.FormatUint(uint64(v>>16), 10) + "." + strconv.FormatUint(uint64(v&0xffff), 10)
}
