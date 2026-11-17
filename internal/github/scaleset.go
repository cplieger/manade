package github

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/config"
)

// defaultGroupID is a repository's one runner group, Default, which the
// actions service numbers 1, as ARC's controller assumes:
// https://github.com/actions/actions-runner-controller/blob/master/controllers/actions.github.com/autoscalingrunnerset_controller.go
const defaultGroupID = 1

// ScaleSet is one profile's scale set in one scope. GitHub numbers scale
// sets per scope, so ID alone names none.
type ScaleSet struct {
	Scope, Profile string
	ID             int
}

// JIT is a new runner's registration: its one-time config, a credential
// nothing may log, and its registration ID.
type JIT struct {
	Config   string
	RunnerID int64
}

// ErrRunnerBusy reports a runner GitHub refused to remove because it holds
// a job.
var ErrRunnerBusy = errors.New("github: the runner is running a job")

// ScaleSets is one credential's scale set side: an actions/scaleset client
// per scope for Ensure, JIT and RemoveRunner, made on the scope's first use
// for its installation; each Listen builds its own. Every call names its
// scope, so it serves every settings revision under that credential. Its
// methods are safe for concurrent use. Ensure, JIT and RemoveRunner keep
// scaleset's retries, which wait out GitHub's Retry-After up to 4 times; a
// wait that would pass ctx's deadline ends the call with GitHub's refusal
// instead, so give each call one.
type ScaleSets struct {
	rest *REST
	key  *creds.AppKey
	hc   *http.Client
	now  func() time.Time
	// resolve is REST.installation, a test seam: a reinstall shows there
	// only after forgeapi's five-minute re-probe.
	resolve func(context.Context, *config.Scope) (int64, error)
	byKey   map[string]*scopeClient
	gh      *config.GitHub
	info    scaleset.SystemInfo
	web     string
	mu      sync.Mutex
}

// scopeClient is one scope's client. gen counts the clients built, so a
// session opened on an older installation knows to reopen. scope is the
// scope's settings as the last call named them.
type scopeClient struct {
	c            *scaleset.Client
	scope        config.Scope
	installation int64
	gen          int
	mu           sync.Mutex
}

// NewScaleSets builds the scale set side under gh's credential over tr;
// version is manade's, sent in its user agent. It sends nothing.
func NewScaleSets(gh *config.GitHub, tr http.RoundTripper, r *REST, version string) *ScaleSets {
	return newScaleSets(gh, tr, r, version, "https://github.com")
}

func newScaleSets(gh *config.GitHub, tr http.RoundTripper, r *REST, version, web string) *ScaleSets {
	return &ScaleSets{
		rest: r, key: gh.AppKey, now: time.Now, resolve: r.installation, byKey: map[string]*scopeClient{}, gh: gh, web: web,
		// scaleset keeps its own retries above the client, and raises the
		// timeout for its long poll.
		hc:   &http.Client{Transport: tr, Timeout: 3 * requestTimeout},
		info: scaleset.SystemInfo{System: "manade", Version: version, Subsystem: "pool"},
	}
}

// scope answers the client of sc's key, taking sc's settings: a changed
// installation_id is resolved at the next call and rebuilds the client.
func (s *ScaleSets) scope(sc *config.Scope) *scopeClient {
	s.mu.Lock()
	c := s.byKey[sc.Key]
	if c == nil {
		c = &scopeClient{}
		s.byKey[sc.Key] = c
	}
	s.mu.Unlock()
	c.mu.Lock()
	c.scope = *sc
	c.mu.Unlock()
	return c
}

func (sc *scopeClient) get() (c *scaleset.Client, installation int64, gen int) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.c, sc.installation, sc.gen
}

func (sc *scopeClient) settings() config.Scope {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.scope
}

// client answers sc's client, resolving the scope's installation the first
// time.
func (s *ScaleSets) client(ctx context.Context, sc *scopeClient) (*scaleset.Client, error) {
	if c, _, _ := sc.get(); c != nil {
		return c, nil
	}
	scope := sc.settings()
	id, err := s.resolve(ctx, &scope)
	if err != nil {
		return nil, err
	}
	return s.install(sc, &scope, id)
}

// install builds sc's client for installation id unless its current one
// already serves it.
func (s *ScaleSets) install(sc *scopeClient, scope *config.Scope, id int64) (*scaleset.Client, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.c != nil && sc.installation == id {
		return sc.c, nil
	}
	c, err := s.build(scope.Key, id)
	if err != nil {
		return nil, err
	}
	if sc.c != nil {
		s.rest.log.Info("The App installation changed, so the scope's sessions reopen on the new one",
			"scope", scope.Key, "from", sc.installation, "to", id)
	}
	sc.c, sc.installation = c, id
	sc.gen++
	return c, nil
}

// build makes a client for the scope with key on installation id. It
// sends nothing.
func (s *ScaleSets) build(key string, id int64) (*scaleset.Client, error) {
	u := s.web + "/" + key
	opts := []scaleset.HTTPOption{scaleset.WithHTTPClient(s.hc), scaleset.WithLogger(s.rest.log)}
	var c *scaleset.Client
	var err error
	if s.key == nil {
		c, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: u, PersonalAccessToken: s.gh.Token.Reveal(), SystemInfo: s.info,
		}, opts...)
	} else {
		c, err = scaleset.NewClientWithJWTProvider(scaleset.ClientWithJWTProviderConfig{
			GitHubConfigURL: u, InstallationID: id, SystemInfo: s.info,
		}, scaleset.JWTProviderFunc(s.key.JWT), opts...)
	}
	if err != nil {
		return nil, failed("scale set client for "+key, err)
	}
	return c, nil
}

// Recheck resolves each built client's installation again and rebuilds a
// client whose installation changed, after a reinstall; that scope's
// sessions reopen on the new installation from their next poll. A
// configured installation_id is kept, and a PAT has no installation.
func (s *ScaleSets) Recheck(ctx context.Context) error {
	s.mu.Lock()
	all := slices.Collect(maps.Values(s.byKey))
	s.mu.Unlock()
	var errs []error
	for _, sc := range all {
		if c, _, _ := sc.get(); c == nil {
			continue
		}
		scope := sc.settings()
		id, err := s.resolve(ctx, &scope)
		if err == nil {
			_, err = s.install(sc, &scope, id)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Ensure finds or creates the scale set of each profile of cfg that serves
// scope, named for the profile with that name as its only label and runner
// updates off, and updates one that differs. It never deletes one: the
// scale sets in the scope's runner group cfg does not name are logged and
// counted as orphans. A failed call is counted as a scale set error; an
// enterprise scope under an App answers ErrEnterpriseNeedsToken.
func (s *ScaleSets) Ensure(ctx context.Context, cfg *config.Config, scope *config.Scope) ([]ScaleSet, error) {
	c, err := s.client(ctx, s.scope(scope))
	if err != nil {
		return nil, err
	}
	out, err := s.ensure(ctx, c, cfg, scope)
	if err != nil {
		s.rest.failure(ctx, OpScaleSet)
		return nil, failed("scale sets of "+scope.Key, err)
	}
	return out, nil
}

func (s *ScaleSets) ensure(ctx context.Context, c *scaleset.Client, cfg *config.Config, scope *config.Scope) ([]ScaleSet, error) {
	group := defaultGroupID
	if scope.Kind != config.ScopeRepo {
		g, err := c.GetRunnerGroupByName(ctx, scope.RunnerGroup)
		if err != nil {
			return nil, err
		}
		group = g.ID
	}
	all, err := c.ListRunnerScaleSets(ctx, group)
	if err != nil {
		return nil, err
	}
	var out []ScaleSet
	for i := range cfg.Profiles {
		p := &cfg.Profiles[i]
		if !p.Serves(scope.Key) {
			continue
		}
		want := scaleset.RunnerScaleSet{
			Name: p.Name, RunnerGroupID: group, Labels: []scaleset.Label{{Name: p.Name, Type: "System"}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		}
		var got *scaleset.RunnerScaleSet
		if at := slices.IndexFunc(all, func(set scaleset.RunnerScaleSet) bool { return set.Name == p.Name }); at >= 0 {
			found := all[at]
			got, all = &found, slices.Delete(all, at, at+1)
		}
		switch {
		case got == nil:
			got, err = c.CreateRunnerScaleSet(ctx, &want)
		case !got.RunnerSetting.DisableUpdate || len(got.Labels) != 1 || got.Labels[0].Name != p.Name:
			got, err = c.UpdateRunnerScaleSet(ctx, got.ID, &want)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ScaleSet{Scope: scope.Key, Profile: p.Name, ID: got.ID})
	}
	for i := range all {
		set := &all[i]
		s.rest.log.Warn("A scale set the config does not name is left in place; delete it by hand if no workflow uses it",
			"scope", scope.Key, "scale_set", textsafe.SanitizeSingleLineBounded(set.Name, 64), "id", set.ID)
	}
	return out, nil
}

// Exists reports whether GitHub still has ss, a scale set of scope: false
// once GitHub answers not found for its ID or names another scale set by
// it. Any other failure is an error, counted as a scale set error.
func (s *ScaleSets) Exists(ctx context.Context, scope *config.Scope, ss ScaleSet) (bool, error) {
	c, err := s.client(ctx, s.scope(scope))
	if err != nil {
		return false, err
	}
	got, err := c.GetRunnerScaleSetByID(ctx, ss.ID)
	switch {
	case errors.Is(err, scaleset.NotFoundError):
		return false, nil
	case err != nil:
		s.rest.failure(ctx, OpScaleSet)
		return false, failed("scale set "+strconv.Itoa(ss.ID)+" of "+ss.Scope, err)
	}
	return got != nil && got.Name == ss.Profile, nil
}

// JIT registers runner name in ss, a scale set of scope. A failure is
// counted as a jit error.
func (s *ScaleSets) JIT(ctx context.Context, scope *config.Scope, ss ScaleSet, name string) (JIT, error) {
	c, err := s.client(ctx, s.scope(scope))
	if err != nil {
		return JIT{}, err
	}
	j, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, ss.ID)
	if err == nil && (j == nil || j.Runner == nil || j.EncodedJITConfig == "") {
		err = errors.New("the answer carries no runner or config")
	}
	if err != nil {
		s.rest.failure(ctx, OpJIT)
		return JIT{}, failed("JIT config in "+ss.Scope, err)
	}
	return JIT{Config: j.EncodedJITConfig, RunnerID: int64(j.Runner.ID)}, nil
}

// RemoveRunner deletes registration id from scope; one already gone counts
// as removed. It answers ErrRunnerBusy while the runner holds a job, and any
// other failure, counted as a scale set error, keeps the container.
func (s *ScaleSets) RemoveRunner(ctx context.Context, scope *config.Scope, id int64) error {
	c, err := s.client(ctx, s.scope(scope))
	if err != nil {
		return err
	}
	switch err := c.RemoveRunner(ctx, id); {
	case err == nil, errors.Is(err, scaleset.NotFoundError), errors.Is(err, scaleset.RunnerNotFoundError):
		return nil
	case errors.Is(err, scaleset.JobStillRunningError):
		return ErrRunnerBusy
	default:
		s.rest.failure(ctx, OpScaleSet)
		return failed("removing a runner in "+scope.Key, err)
	}
}

// failed is a scaleset error at the package edge: its text, which can quote
// GitHub's answer, made one bounded line, with no scaleset type left to
// match. A context error stays matchable.
func failed(what string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("github: %s: %w", what, err)
	}
	text, _ := textsafe.SanitizeSingleLineCapped(err.Error(), 512, "…")
	return fmt.Errorf("github: %s: %s", what, text)
}
