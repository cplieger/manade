package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/manade/internal/config"
	"go.yaml.in/yaml/v4"
)

const testToken = "test-token"

// testPEM is a throwaway App key generated per test binary, so no key is
// ever committed.
var testPEM = sync.OnceValue(func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
})

// load parses layout, a settings document in YAML flow notation, on one
// host, default, and answers it with the App key when app is set and
// testToken otherwise.
func load(t *testing.T, app bool, layout string) (*config.Config, *config.GitHub) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(layout), &doc); err != nil {
		t.Fatalf("layout: %v", err)
	}
	doc["hosts"] = map[string]any{"default": map[string]any{"capacity": map[string]any{"cpus": 16, "memory": "64GiB"}}}
	fillDefaults(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data, nil, nil)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	env := map[string]string{
		"GITHUB_TOKEN": testToken, "GITHUB_APP_CLIENT_ID": "", "GITHUB_APP_PRIVATE_KEY": "",
		"GITHUB_APP_PRIVATE_KEY_FILE": "", "GITHUB_TOKEN_FILE": "", "GITHUB_PROXY": "", "DATA_DIR": "",
	}
	if app {
		env["GITHUB_TOKEN"], env["GITHUB_APP_CLIENT_ID"], env["GITHUB_APP_PRIVATE_KEY"] = "", "Iv1.test", testPEM()
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cp, err := config.LoadControlPlane()
	if err != nil {
		t.Fatalf("config.LoadControlPlane: %v", err)
	}
	return cfg, &cp.GitHub
}

// call is one request the fake GitHub received.
type call struct {
	header http.Header
	path   string
	query  string
	body   string
}

// fakeGitHub is GitHub's REST API on a TLS server: routes answer by path,
// or by "path?query" first, then fallback, and anything else answers 404.
type fakeGitHub struct {
	srv      *httptest.Server
	routes   map[string]http.HandlerFunc
	fallback http.HandlerFunc
	calls    []call
	mu       sync.Mutex
}

func newFake(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.calls = append(f.calls, call{header: r.Header.Clone(), path: r.URL.Path, query: r.URL.RawQuery, body: string(body)})
		h, ok := f.routes[r.URL.Path+"?"+r.URL.RawQuery]
		if !ok {
			h, ok = f.routes[r.URL.Path]
		}
		if !ok && f.fallback != nil {
			h, ok = f.fallback, true
		}
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) route(key string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[key] = h
}

// seen answers the calls whose path is path.
func (f *fakeGitHub) seen(path string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// app serves an App installation: each lookup answers id, and each mint a
// new token.
func (f *fakeGitHub) app(id int64, lookups ...string) {
	for _, p := range lookups {
		f.route(p, jsonOK(fmt.Sprintf(`{"id":%d}`, id)))
	}
	n := 0
	f.route(fmt.Sprintf("/app/installations/%d/access_tokens", id), func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		n++
		tok := fmt.Sprintf("ghs_minted%d", n)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, tok, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
}

func jsonOK(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }
}

// etagged answers body with a strong ETag, and 304 to a request carrying it.
func etagged(body string, header ...string) http.HandlerFunc {
	tag := strconv.Quote(fmt.Sprintf("%x", sha256.Sum256([]byte(body))))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", tag)
		// A 304 carries none of header, as GitHub's omits Link.
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		_, _ = io.WriteString(w, body)
	}
}

// reply answers code and body with header's name and value pairs.
func reply(code int, body string, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// clock is a settable now, safe for concurrent use.
type clock struct {
	t  time.Time
	mu sync.Mutex
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *clock) unix(d time.Duration) string { return strconv.FormatInt(c.now().Add(d).Unix(), 10) }

// newTestREST builds REST over the fake, with forgeapi trusting the fake's
// certificate as its connection's only CA.
func newTestREST(t *testing.T, f *fakeGitHub, cfg *config.Config, gh *config.GitHub) (*REST, *recorder, *capture.Recorder) {
	t.Helper()
	log, rec := capture.New()
	m := &recorder{values: map[string]float64{}}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	conn := forgeapi.Connection{WebBaseURL: f.srv.URL, APIBaseURL: f.srv.URL, CABytes: ca}
	tr := f.srv.Client().Transport.(*http.Transport).Clone()
	r, err := newREST(cfg, gh, tr, &conn, f.srv.URL, m, log, forgeapi.WithPrivateAddresses(true), forgeapi.WithLogger(log))
	if err != nil {
		t.Fatalf("newREST: %v", err)
	}
	return r, m, rec
}

// recorder holds what the client recorded under each series' exposition
// name, so a test names the series it asserts.
type recorder struct {
	values map[string]float64
	mu     sync.Mutex
}

func (r *recorder) set(series string, v float64, add bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if add {
		v += r.values[series]
	}
	r.values[series] = v
}

func (r *recorder) GitHubError(op Op) {
	r.set(`manade_github_errors_total{op="`+string(op)+`"}`, 1, true)
}

func (r *recorder) QueueRead(res ReadResult) {
	r.set(`manade_rest_reads_total{result="`+string(res)+`"}`, 1, true)
}

func (r *recorder) QueueScopeRead(scope string, at time.Time) {
	r.set(`manade_queue_read_last_success_timestamp_seconds{scope="`+scope+`"}`, float64(at.Unix()), false)
}

func (r *recorder) PollOK(ss ScaleSet, at time.Time) {
	r.set(`manade_poll_last_success_timestamp_seconds{profile="`+ss.Profile+`",scope="`+ss.Scope+`"}`, float64(at.Unix()), false)
}

func (r *recorder) Orphaned(scope string, n int) {
	r.set(`manade_scale_sets_orphaned{scope="`+scope+`"}`, float64(n), false)
}

var _ Recorder = (*recorder)(nil)

// fillDefaults gives each scope with no default_profile the first profile
// by name it may use, since the parser refuses a served scope without one.
func fillDefaults(doc map[string]any) {
	profiles, _ := doc["profiles"].(map[string]any)
	names := slices.Sorted(maps.Keys(profiles))
	scopes, _ := doc["scopes"].([]any)
	for _, sc := range scopes {
		m, _ := sc.(map[string]any)
		if _, set := m["default_profile"]; set {
			continue
		}
		key, _ := m["repo"].(string)
		if org, ok := m["org"].(string); ok {
			key = org
		}
		if ent, ok := m["enterprise"].(string); ok {
			key = "enterprises/" + ent
		}
		for _, n := range names {
			body, _ := profiles[n].(map[string]any)
			allowed, limited := body["scopes"].([]any)
			if !limited || slices.ContainsFunc(allowed, func(a any) bool { return strings.EqualFold(fmt.Sprint(a), key) }) {
				m["default_profile"] = n
				break
			}
		}
	}
}

// sample reads one series from r: a counter never counted is 0, any other
// series never set -1.
func sample(t *testing.T, r *recorder, series string) float64 {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.values[series]; ok {
		return v
	}
	if strings.Contains(series, "_total{") {
		return 0
	}
	return -1
}

// noSleep records the waits the package asks for and takes none.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = prev })
	return &waits
}

// runs is a runs-list page holding ids.
func runs(total int, ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf(`{"id":%d}`, id)
	}
	return fmt.Sprintf(`{"total_count":%d,"workflow_runs":[%s]}`, total, strings.Join(parts, ","))
}

// jobsPage is a jobs-list page; each job is name, status and its labels.
func jobsPage(runID int64, jobs ...[]string) string {
	parts := make([]string, len(jobs))
	for i, j := range jobs {
		labels, _ := json.Marshal(j[2:])
		parts[i] = fmt.Sprintf(`{"run_id":%d,"name":%q,"status":%q,"labels":%s,"created_at":"2026-11-10T08:00:0%dZ"}`, runID, j[0], j[1], labels, i)
	}
	return `{"total_count":` + strconv.Itoa(len(jobs)) + `,"jobs":[` + strings.Join(parts, ",") + `]}`
}
