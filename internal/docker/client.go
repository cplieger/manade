// Package docker is a narrow Docker Engine API client over the daemon's Unix
// socket. It negotiates the API version at Dial, from a floor of 1.40, and
// every later request carries the negotiated version.
//
// It owns the pool's ownership checks: it stamps the pool labels on every
// object it creates, and its only removals are RemoveOwned and
// RemoveOwnedImage, which re-inspect the object and refuse one this pool does
// not own. Every response body is read through a size cap, and every call
// but Events gets the package's default deadline when its caller sets none.
// Only its context bounds Events.
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

// The API versions this client speaks. minAPIVersion is the oldest that has
// every call and create field the pool uses; maxAPIVersion is the newest whose
// reference this client was written against.
const (
	minAPIVersion apiVersion = 1<<16 | 40
	maxAPIVersion apiVersion = 1<<16 | 51

	// callTimeout bounds a call whose context has no deadline; httpTimeout
	// sits above it so the context, not the client, ends a slow call.
	callTimeout = 60 * time.Second
	httpTimeout = 90 * time.Second
	// pullTimeout bounds a pull whose context has no deadline.
	pullTimeout = 30 * time.Minute

	// jsonLimit caps a JSON answer; listLimit the container list.
	jsonLimit = 4 << 20
	listLimit = 16 << 20
	// messageLimit caps one message of a pull or event stream.
	messageLimit = 1 << 20
	// errorTextLimit caps the daemon's error text kept in an APIError.
	errorTextLimit = 512
)

// headerTimeout is how long the API client waits for an answer's headers, a
// test seam over callTimeout.
var headerTimeout = callTimeout

// Sentinels an APIError matches through errors.Is.
var (
	errNotFound = errors.New("docker: not found")
	ErrConflict = errors.New("docker: conflict")
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

// ImageMissing reports a create the daemon refused because its image is
// not in the cache, as when something outside the pool removed it.
func ImageMissing(err error) bool {
	e, ok := errors.AsType[*APIError](err)
	return ok && e.Code == http.StatusNotFound && strings.Contains(strings.ToLower(e.Message), "no such image")
}

// Is matches errNotFound for a 404 and ErrConflict for a 409.
func (e *APIError) Is(target error) bool {
	return (target == errNotFound && e.Code == http.StatusNotFound) ||
		(target == ErrConflict && e.Code == http.StatusConflict)
}

// Unwrap gives httpclient's status error, so its sentinels match too.
func (e *APIError) Unwrap() error { return e.status }

// Client talks to one daemon at one negotiated version. It is safe for
// concurrent use.
type Client struct {
	api *http.Client
	// stream has no client or header timeout, for the pull and event streams
	// that the caller's context bounds: a pull's status line arrives only with
	// its first progress line, after the registry answers.
	stream  *http.Client
	now     func() time.Time
	socket  string
	pool    string
	version apiVersion
}

// Dial connects to the daemon at socket, an absolute Unix socket path, and
// negotiates the version: the lower of the daemon's and maxAPIVersion. It
// fails with errAPIVersion when the daemon's newest version is below 1.40 or
// its oldest above maxAPIVersion. pool names this pool's labels.
func Dial(ctx context.Context, socket, pool string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}
	str := tr.Clone()
	str.ResponseHeaderTimeout = 0
	c := &Client{
		api:    &http.Client{Transport: tr, Timeout: httpTimeout, CheckRedirect: httpclient.RefuseAllRedirects},
		stream: &http.Client{Transport: str, CheckRedirect: httpclient.RefuseAllRedirects},
		now:    time.Now,
		socket: socket,
		pool:   pool,
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

// request sends one call and returns its answer's body, or an *APIError for
// a non-2xx answer. path is unversioned; the negotiated version is prefixed
// once Dial has set it. The caller closes the body.
func (c *Client) request(ctx context.Context, hc *http.Client, op, method, path string, query url.Values, body any) (io.ReadCloser, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("docker %s: %w", op, err)
		}
		rd = bytes.NewReader(b)
	}
	u := url.URL{Scheme: "http", Host: "docker", Path: path, RawQuery: query.Encode()}
	if c.version != 0 {
		u.Path = "/v" + c.version.String() + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
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

// call sends a bounded JSON call and decodes its answer into out, when out is
// not nil.
func (c *Client) call(ctx context.Context, op, method, path string, query url.Values, in, out any, limit int64) error {
	ctx, cancel := httpclient.ContextWithDefaultTimeout(ctx, callTimeout)
	defer cancel()
	body, err := c.request(ctx, c.api, op, method, path, query, in)
	if err != nil {
		return err
	}
	if out == nil {
		httpclient.DrainClose(body)
		return nil
	}
	b, err := httpclient.ReadLimitedBody(body, limit)
	if err != nil {
		return fmt.Errorf("docker %s: %w", op, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("docker %s: decoding the answer: %w", op, err)
	}
	return nil
}

// labelFilter is the filters parameter selecting this pool's objects, plus
// any extra filters.
func (c *Client) labelFilter(extra map[string][]string) url.Values {
	f := map[string][]string{"label": {LabelManaged + "=true", labelPool + "=" + c.pool}}
	maps.Copy(f, extra)
	b, _ := json.Marshal(f) // a map of string slices cannot fail to marshal
	return url.Values{"filters": {string(b)}}
}

// poolLabels returns labels with the pool labels set over any the caller gave.
func (c *Client) poolLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+3)
	maps.Copy(out, labels)
	out[LabelManaged] = managedValue
	out[labelPool] = c.pool
	out[LabelCreated] = c.now().UTC().Format(time.RFC3339)
	return out
}

// owned reports whether labels carry this pool's ownership labels.
func (c *Client) owned(labels map[string]string) bool {
	return labels[LabelManaged] == managedValue && labels[labelPool] == c.pool
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
