// Package github is manade's GitHub side: an actions/scaleset client per
// scope for its scale sets and JIT configs, and one per session loop, and
// the REST reads behind the one queue and the visibility proofs, sent
// through go/httpclient and go/httpcache with forgeapi's App token sources.
//
// Every GitHub request leaves through the one transport NewTransport builds.
// No job, run or runner name read from GitHub is trusted text: a caller logs
// it through textsafe and never puts it in a label or an argument.
package github

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/go/httpcache"
	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/obs"
)

const (
	apiBase = "https://api.github.com"
	// maxBody caps one REST answer: a page of 100 runs or jobs is well
	// under 1 MiB.
	maxBody = 8 << 20
	// requestTimeout bounds one REST read, its retries included.
	requestTimeout = 30 * time.Second
	// maxGroupPages bounds the runner-group list at 1,000 groups.
	maxGroupPages = 10
)

// discardDelays are the waits before a 401's token is sent again when its
// source keeps it: GitHub's replication delay after a mint, the waits
// forgeapi's own transport uses.
var discardDelays = []time.Duration{time.Second, 2 * time.Second}

// sleep is a test seam over the waits this package takes.
var sleep = httpclient.SleepCtx

// NewTransport builds the transport every GitHub request uses: the system
// trust store, TLS 1.2 or newer, and proxy as the only proxy. The
// environment's proxy variables are never read, so no process-wide setting
// can send GitHub traffic where the config does not.
func NewTransport(proxy *url.URL) (*http.Transport, error) {
	t, err := httpclient.CloneDefaultTransport()
	if err != nil {
		return nil, fmt.Errorf("github: transport: %w", err)
	}
	t.Proxy = nil
	if proxy != nil {
		t.Proxy = http.ProxyURL(proxy)
	}
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return t, nil
}

// REST holds each scope's token source and sends the REST reads. It is safe
// for concurrent use; the queue sweep is its cache's only reader.
type REST struct {
	auth   *auth
	plain  *http.Client
	cached *http.Client
	cache  *httpcache.RoundTripper
	m      *obs.Metrics
	log    *slog.Logger
	api    string
}

// NewREST builds the REST side for cfg's scopes over tr. It sends nothing:
// installations resolve on first use, or at CheckInstallations.
func NewREST(cfg *config.Config, tr http.RoundTripper, m *obs.Metrics, log *slog.Logger) (*REST, error) {
	conn, opts := connection(cfg, log)
	return newREST(cfg, tr, &conn, apiBase, m, log, opts...)
}

// connection is forgeapi's github.com connection under cfg. forgeapi refuses
// a private or single-label proxy host unless the connection opts in, and
// setting GITHUB_PROXY is that opt-in. It also lifts forgeapi's address
// check on the dial, which then only ever reaches the proxy, and on each
// redirect hop, whose host the proxy then reaches whatever its range.
func connection(cfg *config.Config, log *slog.Logger) (forgeapi.Connection, []forgeapi.Option) {
	conn := forgeapi.Connection{WebBaseURL: "https://github.com"}
	opts := []forgeapi.Option{forgeapi.WithLogger(log)}
	if cfg.GitHub.Proxy != nil {
		conn.Proxy = cfg.GitHub.Proxy.String()
		opts = append(opts, forgeapi.WithPrivateAddresses(true))
	}
	return conn, opts
}

func newREST(cfg *config.Config, tr http.RoundTripper, conn *forgeapi.Connection, api string, m *obs.Metrics, log *slog.Logger, opts ...forgeapi.Option) (*REST, error) {
	a, err := newAuth(cfg, conn, opts)
	if err != nil {
		return nil, err
	}
	// 401, 403 and 429 are never retried here: a 401 belongs to the token
	// source, and the queue's rate-limit waits are longer than any request.
	retry := httpclient.NewRetryRoundTripper(tr, httpclient.TransportConfig{CheckRetry: func(_ context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return httpclient.IsTransient(err), nil
		}
		return resp.StatusCode >= 500 && httpclient.IsRetryableStatus(resp.StatusCode), nil
	}})
	cache := httpcache.NewRoundTripper(retry, httpcache.Config{Held: []httpcache.HeldField{{Name: "Link"}}, FreeNotModified: true})
	client := func(rt http.RoundTripper) *http.Client {
		return &http.Client{Transport: rt, Timeout: 3 * requestTimeout, CheckRedirect: httpclient.RefuseAllRedirects}
	}
	return &REST{auth: a, plain: client(retry), cached: client(cache), cache: cache, m: m, log: log, api: api}, nil
}

// discarder is a token source told of a 401 (forgeapi.CredentialSource's
// optional Discard).
type discarder interface {
	Discard(token string) (retry bool)
}

// answer is one REST response, read in full.
type answer struct {
	header http.Header
	body   []byte
	status int
}

func (a *answer) err() error {
	if a.status/100 == 2 {
		return nil
	}
	return &httpclient.StatusError{Code: a.status}
}

// get sends one GET with the scope's token. A 401 is told to the source;
// when the source keeps the token, the same token is sent again after each
// of discardDelays, so a fresh token's replication delay costs no mint.
func (r *REST) get(ctx context.Context, c *http.Client, src forgeapi.CredentialSource, u string) (answer, error) {
	token, err := src.Token(ctx)
	if err != nil {
		return answer{}, fmt.Errorf("github: token: %w", err)
	}
	for i := 0; ; i++ {
		a, err := r.send(ctx, c, token, u)
		if err != nil || a.status != http.StatusUnauthorized {
			return a, err
		}
		d, ok := src.(discarder)
		if !ok || !d.Discard(token) || i == len(discardDelays) {
			return a, nil
		}
		if err := sleep(ctx, discardDelays[i]); err != nil {
			return a, err
		}
	}
}

func (r *REST) send(ctx context.Context, c *http.Client, token, u string) (answer, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return answer{}, fmt.Errorf("github: request: %w", httpclient.LogSafeError(err))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.Do(req)
	if err != nil {
		return answer{}, fmt.Errorf("github: %w", httpclient.RedactTransportError(err, "", httpclient.Secret(token)))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := httpclient.ReadLimitedBody(resp.Body, maxBody)
	if err != nil {
		return answer{}, fmt.Errorf("github: reading the answer: %w", httpclient.LogSafeError(err))
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// getJSON reads u on the plain client into v; found is false on a 404.
func (r *REST) getJSON(ctx context.Context, s *config.Scope, u string, v any) (h http.Header, found bool, err error) {
	a, err := r.get(ctx, r.plain, r.auth.source(s), u)
	if err != nil {
		return nil, false, err
	}
	if a.status == http.StatusNotFound {
		return a.header, false, nil
	}
	if err := a.err(); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(a.body, v); err != nil {
		return nil, false, fmt.Errorf("github: decoding an answer: %w", err)
	}
	return a.header, true, nil
}

// nextPage answers the Link header's next page, which must stay on the API
// origin api: the request carries the token.
func nextPage(api string, h http.Header) (string, error) {
	for part := range strings.SplitSeq(h.Get("Link"), ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		target = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		if !strings.HasPrefix(target, api+"/") {
			return "", errors.New("github: a page link left the API origin")
		}
		return target, nil
	}
	return "", nil
}

// accountPath is the REST path of the account a scope registers under.
func accountPath(s *config.Scope) string {
	switch s.Kind {
	case config.ScopeRepo:
		return "/repos/" + s.Key
	case config.ScopeOrg:
		return "/orgs/" + s.Owner
	}
	return "/enterprises/" + s.Owner
}

var (
	errNoGroup      = errors.New("github: the scope's runner group is not among the account's runner groups")
	errGroupsUnseen = errors.New("github: the account's runner groups answer 404, so the token may not see the account")
	errGroupPublic  = errors.New("github: the runner group answer carries no public-repository setting")
)

// failure counts a failed call of op and reports true, or reports false
// once ctx was cancelled: a call shutdown cut short is no failure. A call
// a deadline ended is one, so only cancellation is spared.
func (r *REST) failure(ctx context.Context, op obs.GitHubOp) bool {
	if errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	r.m.GitHubError(op)
	return true
}

// Private reports whether only private repositories reach the scope: a repo
// scope's repository is private, or an organization or enterprise scope's
// runner group admits no public repository. A failed read proves nothing
// and is counted as a visibility error.
func (r *REST) Private(ctx context.Context, s *config.Scope) (bool, error) {
	ok, err := r.private(ctx, s)
	if err != nil {
		r.failure(ctx, obs.GitHubVisibility)
	}
	return ok, err
}

func (r *REST) private(ctx context.Context, s *config.Scope) (bool, error) {
	if s.Kind == config.ScopeRepo {
		var repo struct {
			Private *bool `json:"private"`
		}
		_, found, err := r.getJSON(ctx, s, r.api+accountPath(s), &repo)
		if err == nil && (!found || repo.Private == nil) {
			err = errors.New("github: the repository answer carries no visibility")
		}
		return err == nil && *repo.Private, err
	}
	return r.privateGroup(ctx, s)
}

// privateGroup reads the scope's runner group through the list's pages.
func (r *REST) privateGroup(ctx context.Context, s *config.Scope) (bool, error) {
	u := r.api + accountPath(s) + "/actions/runner-groups?per_page=100"
	for range maxGroupPages {
		var list struct {
			Groups []struct {
				Public *bool  `json:"allows_public_repositories"`
				Name   string `json:"name"`
			} `json:"runner_groups"`
		}
		h, found, err := r.getJSON(ctx, s, u, &list)
		if err != nil || !found {
			return false, cmp.Or(err, errGroupsUnseen)
		}
		for _, g := range list.Groups {
			switch {
			case !strings.EqualFold(g.Name, s.RunnerGroup):
			case g.Public == nil:
				return false, errGroupPublic
			default:
				return !*g.Public, nil
			}
		}
		if u, err = nextPage(r.api, h); err != nil || u == "" {
			return false, cmp.Or(err, errNoGroup)
		}
	}
	return false, errPages
}

// Runner is a registration's state on GitHub.
type Runner struct {
	Busy   bool
	Online bool
}

var (
	errUnseen = errors.New("github: the scope's runner list answers 404 too, so the token may not see the scope")
	errNoBusy = errors.New("github: the runner answer carries no busy flag")
)

// Runner reads registration id at the scope's own runners URL. found is
// false only on that URL's 404 while the scope's runner list still reads,
// so any other failure keeps the container.
func (r *REST) Runner(ctx context.Context, s *config.Scope, id int64) (Runner, bool, error) {
	var w struct {
		Busy   *bool  `json:"busy"`
		Status string `json:"status"`
	}
	runners := r.api + accountPath(s) + "/actions/runners"
	_, found, err := r.getJSON(ctx, s, runners+"/"+strconv.FormatInt(id, 10), &w)
	if err == nil && found && w.Busy == nil {
		err = errNoBusy
	}
	if err == nil && !found {
		// GitHub answers 404 for a scope the token cannot see
		// (https://docs.github.com/en/rest/using-the-rest-api/troubleshooting-the-rest-api).
		var list struct{}
		if _, seen, lerr := r.getJSON(ctx, s, runners+"?per_page=1", &list); lerr != nil || !seen {
			err = cmp.Or(lerr, errUnseen)
		}
	}
	if err != nil {
		r.failure(ctx, obs.GitHubScaleSet)
		return Runner{}, false, err
	}
	return Runner{Busy: w.Busy != nil && *w.Busy, Online: w.Status == "online"}, found, nil
}
