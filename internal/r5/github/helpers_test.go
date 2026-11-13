package github

import (
	"bufio"
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/obs"
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

// load validates file through config.Load, under the App key when app is
// set and under testToken otherwise.
func load(t *testing.T, app bool, file string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("capacity: {cpus: 16, memory: 64GiB}\n"+file), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"CONFIG_FILE": path, "GITHUB_TOKEN": testToken, "GITHUB_APP_CLIENT_ID": "", "GITHUB_APP_PRIVATE_KEY": "",
		"GITHUB_APP_PRIVATE_KEY_FILE": "", "GITHUB_TOKEN_FILE": "", "GITHUB_PROXY": "", "POOL_NAME": "", "DOCKER_SOCKET": "",
	}
	if app {
		env["GITHUB_TOKEN"], env["GITHUB_APP_CLIENT_ID"], env["GITHUB_APP_PRIVATE_KEY"] = "", "Iv1.test", testPEM()
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load(t.Context())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
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
func newTestREST(t *testing.T, f *fakeGitHub, cfg *config.Config) (*REST, *obs.Metrics, *capture.Recorder) {
	t.Helper()
	log, rec := capture.New()
	m, err := obs.New(cfg, log)
	if err != nil {
		t.Fatalf("obs.New: %v", err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	conn := forgeapi.Connection{WebBaseURL: f.srv.URL, APIBaseURL: f.srv.URL, CABytes: ca}
	tr := f.srv.Client().Transport.(*http.Transport).Clone()
	r, err := newREST(cfg, tr, &conn, f.srv.URL, m, log, forgeapi.WithPrivateAddresses(true), forgeapi.WithLogger(log))
	if err != nil {
		t.Fatalf("newREST: %v", err)
	}
	return r, m, rec
}

// sample reads one series from m's page, -1 when it is absent.
func sample(t *testing.T, m *obs.Metrics, series string) float64 {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	sc := bufio.NewScanner(w.Body)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("parsing %s: %v", sc.Text(), err)
			}
			return f
		}
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
