package github

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/manade/internal/config"
)

const patLayout = `scopes:
  - repo: acme/app
  - org: acme
    runner_group: CI
  - enterprise: big
profiles:
  small: {scopes: [acme/app]}
`

func TestNewTransportFailsClosed(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy.invalid:3128")
	proxy, _ := url.Parse("http://proxy.invalid:3128")
	for _, tc := range []struct {
		proxy *url.URL
		want  string
		name  string
	}{
		{name: "direct", proxy: nil, want: ""},
		{name: "githubProxy", proxy: proxy, want: proxy.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := NewTransport(tc.proxy)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if tr.Proxy != nil {
				u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.github.com"}})
				if err != nil {
					t.Fatal(err)
				}
				got = u.String()
			}
			if got != tc.want {
				t.Errorf("proxy = %q, want %q: the environment's proxy is never read", got, tc.want)
			}
			if c := tr.TLSClientConfig; c.RootCAs != nil || c.InsecureSkipVerify || c.MinVersion != tls.VersionTLS12 {
				t.Errorf("TLS config = %+v, want the system trust store, verification on, TLS 1.2 or newer", c)
			}
		})
	}
	t.Run("refusesUntrustedCertificate", func(t *testing.T) {
		f := newFake(t)
		tr, err := NewTransport(nil)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.srv.URL, http.NoBody)
		if resp, err := (&http.Client{Transport: tr}).Do(req); err == nil {
			_ = resp.Body.Close()
			t.Error("a server outside the system trust store was accepted")
		}
	})
}

// GITHUB_PROXY is any http or https proxy URL, so an App credential must
// start behind a LAN proxy as a PAT does.
func TestNewRESTAppBehindPrivateProxy(t *testing.T) {
	cfg := load(t, true, appLayout)
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct{ name, proxy string }{
		{name: "privateAddress", proxy: "http://10.1.2.3:3128"},
		{name: "singleLabel", proxy: "http://proxy:3128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, err := url.Parse(tc.proxy)
			if err != nil {
				t.Fatal(err)
			}
			tr, err := NewTransport(proxy)
			if err != nil {
				t.Fatal(err)
			}
			withProxy := *cfg
			withProxy.GitHub.Proxy = proxy
			if _, err := NewREST(&withProxy, tr, nil, log); err != nil {
				t.Errorf("NewREST behind %s: %v, want the App's token sources built", tc.proxy, err)
			}
		})
	}
}

// Without GITHUB_PROXY the connection keeps forgeapi's default refusal of a
// private address.
func TestConnectionWithoutProxyRefusesPrivateAddress(t *testing.T) {
	cfg := load(t, true, appLayout)
	conn, opts := connection(cfg, slog.New(slog.DiscardHandler))
	conn.Proxy = "http://10.1.2.3:3128"
	_, err := creds.NewAppSource(conn, cfg.GitHub.AppKey, creds.AppSourceConfig{Installation: creds.InstallationOfOrg("acme"), Grant: creds.AppGrant{Permissions: orgGrant}}, opts...)
	var fe *forgeapi.Error
	if !errors.As(err, &fe) || fe.Code != forgeapi.CodePrivateAddressRefused {
		t.Errorf("NewAppSource = %v, want %s", err, forgeapi.CodePrivateAddressRefused)
	}
}

func TestPrivate(t *testing.T) {
	for _, tc := range []struct {
		is                      error
		name, scope, path, body string
		code                    int
		want, fails             bool
	}{
		{name: "privateRepo", scope: "acme/app", path: "/repos/acme/app", body: `{"private":true}`, want: true},
		{name: "publicRepo", scope: "acme/app", path: "/repos/acme/app", body: `{"private":false}`},
		{name: "noVisibility", scope: "acme/app", path: "/repos/acme/app", body: `{}`, fails: true},
		{
			name: "privateGroup", scope: "acme", path: "/orgs/acme/actions/runner-groups",
			body: `{"runner_groups":[{"name":"Default","allows_public_repositories":true},{"name":"ci","allows_public_repositories":false}]}`, want: true,
		},
		{
			name: "publicGroup", scope: "acme", path: "/orgs/acme/actions/runner-groups",
			body: `{"runner_groups":[{"name":"CI","allows_public_repositories":true}]}`,
		},
		{name: "missingGroup", scope: "acme", path: "/orgs/acme/actions/runner-groups", body: `{"runner_groups":[]}`, fails: true, is: errNoGroup},
		{
			name: "groupWithoutPublicSetting", scope: "acme", path: "/orgs/acme/actions/runner-groups",
			body: `{"runner_groups":[{"name":"CI"}]}`, fails: true, is: errGroupPublic,
		},
		{
			name: "groupsUnseen", scope: "acme", path: "/orgs/acme/actions/runner-groups",
			code: http.StatusNotFound, body: `{"message":"Not Found"}`, fails: true, is: errGroupsUnseen,
		},
		{
			name: "enterpriseGroup", scope: "enterprises/big", path: "/enterprises/big/actions/runner-groups",
			body: `{"runner_groups":[{"name":"Default","allows_public_repositories":false}]}`, want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			cfg := load(t, false, patLayout)
			r, m, _ := newTestREST(t, f, cfg)
			f.route(tc.path, reply(cmp.Or(tc.code, http.StatusOK), tc.body))
			got, err := r.Private(t.Context(), scope(t, cfg, tc.scope))
			if got != tc.want || (err != nil) != tc.fails || (tc.is != nil && !errors.Is(err, tc.is)) {
				t.Errorf("Private = %v, %v; want %v, error %v matching %v", got, err, tc.want, tc.fails, tc.is)
			}
			if errs := sample(t, m, `manade_github_errors_total{op="visibility"}`); (errs == 1) != tc.fails {
				t.Errorf("visibility errors = %v, want one per failed read", errs)
			}
			if c := f.seen(tc.path); len(c) != 1 || c[0].header.Get("Authorization") != "Bearer "+testToken {
				t.Errorf("calls = %+v, want one read with the PAT", c)
			}
		})
	}
}

// A read shutdown cancels mid-flight is no failure, so it counts no error;
// one its deadline ended still does.
func TestPrivateCutShortCountsOnlyADeadline(t *testing.T) {
	for _, tc := range []struct {
		ctx    func(*testing.T) (context.Context, context.CancelFunc)
		name   string
		errors float64
	}{
		{name: "cancelled", ctx: func(t *testing.T) (context.Context, context.CancelFunc) { return context.WithCancel(t.Context()) }},
		{name: "deadline", errors: 1, ctx: func(t *testing.T) (context.Context, context.CancelFunc) {
			return context.WithTimeout(t.Context(), 50*time.Millisecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			cfg := load(t, false, patLayout)
			r, m, _ := newTestREST(t, f, cfg)
			ctx, cancel := tc.ctx(t)
			defer cancel()
			f.route("/repos/acme/app", func(w http.ResponseWriter, req *http.Request) {
				if tc.errors == 0 {
					cancel()
				}
				select {
				case <-req.Context().Done():
				case <-time.After(10 * time.Second):
				}
			})
			if _, err := r.Private(ctx, scope(t, cfg, "acme/app")); err == nil {
				t.Fatal("Private answered a read cut short")
			}
			if got := sample(t, m, `manade_github_errors_total{op="visibility"}`); got != tc.errors {
				t.Errorf("visibility errors = %v, want %v", got, tc.errors)
			}
		})
	}
}

// The runner-group list follows its Link pages on the API origin, up to
// its bound of 10.
func TestPrivatePagesRunnerGroups(t *testing.T) {
	const groups = "/orgs/acme/actions/runner-groups"
	page := func(f *fakeGitHub, n int, names ...string) http.HandlerFunc {
		var body []string
		for _, g := range names {
			body = append(body, fmt.Sprintf(`{"name":%q,"allows_public_repositories":false}`, g))
		}
		link := fmt.Sprintf(`<%s%s?per_page=100&page=%d>; rel="next"`, f.srv.URL, groups, n+1)
		return reply(http.StatusOK, `{"runner_groups":[`+strings.Join(body, ",")+`]}`, "Link", link)
	}
	for _, tc := range []struct {
		route func(f *fakeGitHub)
		name  string
		reads int
		want  bool
	}{
		{name: "groupOnPageTwo", reads: 2, want: true, route: func(f *fakeGitHub) {
			f.route(groups+"?per_page=100", page(f, 1, "Default"))
			f.route(groups+"?per_page=100&page=2", jsonOK(`{"runner_groups":[{"name":"Default"},{"name":"ci","allows_public_repositories":false}]}`))
		}},
		{name: "endlessListFails", reads: 10, route: func(f *fakeGitHub) {
			f.fallback = func(w http.ResponseWriter, r *http.Request) {
				n, _ := strconv.Atoi(r.URL.Query().Get("page"))
				page(f, max(n, 1), "Default")(w, r)
			}
		}},
		{name: "linkOffOriginFails", reads: 1, route: func(f *fakeGitHub) {
			f.route(groups+"?per_page=100", reply(http.StatusOK, `{"runner_groups":[]}`, "Link", `<https://elsewhere.example/next>; rel="next"`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			cfg := load(t, false, patLayout)
			r, _, _ := newTestREST(t, f, cfg)
			tc.route(f)
			got, err := r.Private(t.Context(), scope(t, cfg, "acme"))
			if got != tc.want || (err == nil) != tc.want {
				t.Errorf("Private = %v, %v; want %v", got, err, tc.want)
			}
			if n := len(f.seen(groups)); n != tc.reads {
				t.Errorf("group list reads = %d, want %d", n, tc.reads)
			}
		})
	}
}

func scope(t *testing.T, cfg *config.Config, key string) *config.Scope {
	t.Helper()
	for i := range cfg.Scopes {
		if cfg.Scopes[i].Key == key {
			return &cfg.Scopes[i]
		}
	}
	t.Fatalf("no scope %s", key)
	return nil
}

func TestRunnerAtScopeURL(t *testing.T) {
	f := newFake(t)
	cfg := load(t, false, patLayout)
	r, _, _ := newTestREST(t, f, cfg)
	f.route("/repos/acme/app/actions/runners/5", jsonOK(`{"status":"online","busy":true}`))
	f.route("/orgs/acme/actions/runners/5", jsonOK(`{"status":"offline","busy":false}`))
	f.route("/repos/acme/app/actions/runners/8", jsonOK(`{"status":"online"}`))
	f.route("/enterprises/big/actions/runners/6", reply(http.StatusForbidden, `{}`))
	f.route("/enterprises/big/actions/runners", jsonOK(`{"total_count":0,"runners":[]}`))
	for _, tc := range []struct {
		name, scope string
		want        Runner
		id          int64
		found, fail bool
	}{
		{name: "repoBusy", scope: "acme/app", id: 5, want: Runner{Busy: true, Online: true}, found: true},
		{name: "orgOffline", scope: "acme", id: 5, found: true},
		{name: "enterpriseGone", scope: "enterprises/big", id: 5},
		{name: "refusedKeeps", scope: "enterprises/big", id: 6, fail: true},
		{name: "noBusyKeeps", scope: "acme/app", id: 8, fail: true},
		// GitHub answers 404 for a repository the token cannot see.
		{name: "invisibleScopeKeeps", scope: "acme/app", id: 9, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := r.Runner(t.Context(), scope(t, cfg, tc.scope), tc.id)
			if got != tc.want || found != tc.found || (err != nil) != tc.fail {
				t.Errorf("Runner = %+v, %v, %v; want %+v, %v, error %v", got, found, err, tc.want, tc.found, tc.fail)
			}
		})
	}
	if c := f.seen("/enterprises/big/actions/runners"); len(c) != 1 || c[0].query != "per_page=1" {
		t.Errorf("runner list reads = %+v, want one, after the gone registration's 404 only", c)
	}
}

// discardSource is a token source whose Discard answers keep in turn.
type discardSource struct {
	patSource
	keep      []bool
	discarded []string
	tokens    int
}

func (d *discardSource) Token(ctx context.Context) (string, error) {
	d.tokens++
	return d.patSource.Token(ctx)
}

func (d *discardSource) Discard(token string) bool {
	d.discarded = append(d.discarded, token)
	keep := len(d.keep) > 0 && d.keep[0]
	if len(d.keep) > 0 {
		d.keep = d.keep[1:]
	}
	return keep
}

func TestGetResendsKeptTokenAfter401(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keep     []bool
		refusals int
		sends    int
		waits    []time.Duration
		status   int
	}{
		{name: "replicationDelay", keep: []bool{true}, refusals: 1, sends: 2, waits: []time.Duration{time.Second}, status: 200},
		{name: "twoDelays", keep: []bool{true, true}, refusals: 2, sends: 3, waits: []time.Duration{time.Second, 2 * time.Second}, status: 200},
		{name: "dropped", keep: []bool{false}, refusals: 1, sends: 1, status: 401},
		{name: "keptThrice", keep: []bool{true, true, true}, refusals: 9, sends: 3, waits: []time.Duration{time.Second, 2 * time.Second}, status: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			r, _, _ := newTestREST(t, f, load(t, false, patLayout))
			waits := noSleep(t)
			var n int
			f.route("/repos/acme/app", func(w http.ResponseWriter, _ *http.Request) {
				if n++; n <= tc.refusals {
					w.WriteHeader(http.StatusUnauthorized)
				}
			})
			src := &discardSource{patSource: patSource{config.Secret{}}, keep: tc.keep}
			a, err := r.get(t.Context(), r.plain, src, f.srv.URL+"/repos/acme/app")
			if err != nil || a.status != tc.status {
				t.Fatalf("get = %d, %v; want %d", a.status, err, tc.status)
			}
			if got := len(f.seen("/repos/acme/app")); got != tc.sends || src.tokens != 1 {
				t.Errorf("sends = %d with %d token calls, want %d sends of one token", got, src.tokens, tc.sends)
			}
			if len(*waits) != len(tc.waits) || (len(tc.waits) > 0 && (*waits)[len(tc.waits)-1] != tc.waits[len(tc.waits)-1]) {
				t.Errorf("waits = %v, want %v", *waits, tc.waits)
			}
		})
	}
}

func TestGetRetryLeavesRateLimitsToCaller(t *testing.T) {
	f := newFake(t)
	r, _, _ := newTestREST(t, f, load(t, false, patLayout))
	f.route("/repos/acme/app", reply(http.StatusTooManyRequests, `{}`, "Retry-After", "1"))
	a, err := r.get(t.Context(), r.plain, patSource{}, f.srv.URL+"/repos/acme/app")
	if err != nil || a.status != http.StatusTooManyRequests || !errors.Is(a.err(), httpclient.ErrRateLimited) {
		t.Errorf("get = %d, %v; want the 429 answered to the caller", a.status, err)
	}
	if got := len(f.seen("/repos/acme/app")); got != 1 {
		t.Errorf("sends = %d, want 1: a rate limit is not retried inside a request", got)
	}
}

var _ forgeapi.CredentialSource = (*discardSource)(nil)
