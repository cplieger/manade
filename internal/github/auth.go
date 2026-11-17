package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cplieger/forgeapi/v3"
	"github.com/cplieger/forgeapi/v3/creds"
	"github.com/cplieger/manade/internal/config"
)

// The REST grants per scope kind. Administration is a repository
// permission; an organization's runner groups answer to Self-hosted runners.
const (
	read     = "read"
	metadata = "metadata"
)

var (
	repoGrant = map[string]string{metadata: read, "administration": read, "actions": read}
	orgGrant  = map[string]string{metadata: read, "organization_self_hosted_runners": read}
	// The routing variable's grants: its own, so the scope's grant
	// stays read-only. Write includes read.
	repoVariableGrant = map[string]string{metadata: read, "actions_variables": "write"}
	orgVariableGrant  = map[string]string{metadata: read, "organization_actions_variables": "write"}
)

// ErrEnterpriseNeedsToken refuses an enterprise scope under a GitHub App:
// only a classic token reaches an enterprise's runner groups.
var ErrEnterpriseNeedsToken = errors.New("github: an enterprise scope needs a classic token, and the credential is a GitHub App")

// sourceKey names a scope's token source: its key, its configured
// installation, since a changed installation_id needs a source of its own,
// and whether it mints the variable grant.
type sourceKey struct {
	key          string
	installation int64
	variables    bool
}

// auth is one credential's token sources, made on a scope's first use.
// Under a PAT every scope shares the token. Under an App, each repo scope
// has its own, whose grant names only its repository: GitHub refuses a
// whole mint with a 422, which forgeapi holds as terminal, when one named
// repository is gone from the installation. An organization scope's grant
// names no repository.
type auth struct {
	gh      *config.GitHub
	byScope map[sourceKey]forgeapi.CredentialSource
	// proven holds each configured installation_id GitHub answered for its
	// scope's account.
	proven map[sourceKey]bool
	opts   []forgeapi.Option
	conn   forgeapi.Connection
	mu     sync.Mutex
}

func newAuth(gh *config.GitHub, conn *forgeapi.Connection, opts []forgeapi.Option) *auth {
	return &auth{gh: gh, conn: *conn, opts: opts, byScope: map[sourceKey]forgeapi.CredentialSource{}, proven: map[sourceKey]bool{}}
}

// app reports whether the credential is a GitHub App.
func (a *auth) app() bool { return a.gh.AppKey != nil }

// source answers s's token source, made on first use.
func (a *auth) source(s *config.Scope) (forgeapi.CredentialSource, error) {
	return a.sourceFor(s, false)
}

// sourceFor answers s's token source for the scope's grant, or for the
// routing variable's when variables is set.
func (a *auth) sourceFor(s *config.Scope, variables bool) (forgeapi.CredentialSource, error) {
	if !a.app() {
		return patSource{a.gh.Token}, nil
	}
	if s.Kind == config.ScopeEnterprise {
		return nil, ErrEnterpriseNeedsToken
	}
	k := sourceKey{s.Key, s.InstallationID, variables}
	a.mu.Lock()
	defer a.mu.Unlock()
	if src, ok := a.byScope[k]; ok {
		return src, nil
	}
	in := selector(s)
	if s.InstallationID != 0 {
		in = creds.InstallationByID(s.InstallationID)
	}
	src, err := creds.NewAppSource(a.conn, a.gh.AppKey, creds.AppSourceConfig{Installation: in, Grant: grant(s, variables)}, a.opts...)
	if err != nil {
		return nil, fmt.Errorf("github: token source for %s: %w", s.Key, err)
	}
	a.byScope[k] = src
	return src, nil
}

// limit names the rate limit s's reads count against: the PAT's, or the
// account's one installation's, which all its sources share.
func (a *auth) limit(s *config.Scope) string {
	if !a.app() {
		return ""
	}
	return strings.ToLower(s.Owner)
}

func selector(s *config.Scope) creds.Installation {
	if s.Kind == config.ScopeRepo {
		return creds.InstallationOfRepo(s.Owner + "/" + s.Repo)
	}
	return creds.InstallationOfOrg(s.Owner)
}

// grant is s's REST grant, or its routing variable's: a repo scope's names
// its one repository, an organization scope's none.
func grant(s *config.Scope, variables bool) creds.AppGrant {
	switch {
	case s.Kind == config.ScopeRepo && variables:
		return creds.AppGrant{Permissions: repoVariableGrant, Repositories: []string{s.Repo}}
	case s.Kind == config.ScopeRepo:
		return creds.AppGrant{Permissions: repoGrant, Repositories: []string{s.Repo}}
	case variables:
		return creds.AppGrant{Permissions: orgVariableGrant}
	}
	return creds.AppGrant{Permissions: orgGrant}
}

// installation answers the App installation that serves s: its configured
// installation_id once GitHub answered it for s's account, or the account's
// installation as GitHub answers now, so a reinstall's new ID shows once
// the old one's tokens are refused. It answers 0 under a PAT. A failed
// lookup is counted as an installation error.
func (r *REST) installation(ctx context.Context, s *config.Scope) (int64, error) {
	if !r.auth.app() {
		return 0, nil
	}
	if s.InstallationID != 0 {
		return s.InstallationID, r.proveInstallation(ctx, s)
	}
	src, err := r.auth.source(s)
	if err != nil {
		return 0, err
	}
	app, ok := src.(*creds.AppSource)
	if !ok {
		return 0, fmt.Errorf("github: no App token source for %s", s.Key)
	}
	return r.resolve(ctx, s, app)
}

// proveInstallation checks once per source that s's configured
// installation_id is the one GitHub answers for its account.
func (r *REST) proveInstallation(ctx context.Context, s *config.Scope) error {
	if s.Kind == config.ScopeEnterprise {
		return ErrEnterpriseNeedsToken
	}
	k := sourceKey{s.Key, s.InstallationID, false}
	r.auth.mu.Lock()
	done := r.auth.proven[k]
	r.auth.mu.Unlock()
	if done {
		return nil
	}
	src, err := creds.NewAppSource(r.auth.conn, r.auth.gh.AppKey, creds.AppSourceConfig{Installation: selector(s), Grant: grant(s, false)}, r.auth.opts...)
	if err != nil {
		return fmt.Errorf("github: installation lookup for %s: %w", s.Key, err)
	}
	id, err := r.resolve(ctx, s, src)
	if err != nil {
		return err
	}
	if id != s.InstallationID {
		return fmt.Errorf("github: %s is served by installation %d, not the configured installation_id %d", s.Key, id, s.InstallationID)
	}
	r.auth.mu.Lock()
	r.auth.proven[k] = true
	r.auth.mu.Unlock()
	return nil
}

func (r *REST) resolve(ctx context.Context, s *config.Scope, src *creds.AppSource) (int64, error) {
	id, err := src.Resolve(ctx)
	if err != nil {
		r.failure(ctx, opInstallation)
		return 0, fmt.Errorf("github: installation for %s: %w", s.Key, err)
	}
	return id, nil
}

// patSource answers a personal access token, which never rotates.
type patSource struct{ token config.Secret }

func (p patSource) Token(context.Context) (string, error) { return p.token.Reveal(), nil }
func (patSource) Kind() forgeapi.CredKind                 { return forgeapi.CredKindStaticPAT }
func (patSource) State() forgeapi.CredState               { return forgeapi.CredValid }

var _ forgeapi.CredentialSource = patSource{}
