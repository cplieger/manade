package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/cplieger/forgeapi/v2"
	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/obs"
)

// The REST grants per scope kind. Administration is a repository
// permission; an organization's runner groups answer to Self-hosted runners.
const read = "read"

var (
	repoGrant = map[string]string{"metadata": read, "administration": read, "actions": read}
	orgGrant  = map[string]string{"metadata": read, "organization_self_hosted_runners": read}
)

// auth holds each scope's REST token source. Under a PAT every scope shares
// one. Under an App, each repo scope has its own, whose grant names only its
// repository: GitHub refuses a whole mint with a 422, which forgeapi holds
// as terminal, when one named repository is gone from the installation. An
// organization scope's grant names no repository.
type auth struct {
	byScope map[string]forgeapi.CredentialSource
	// limit names the rate limit each scope's reads count against: the PAT's,
	// or the account's one installation's, which all its sources share.
	limit map[string]string
	// lookup builds a source that resolves s's installation by its own
	// selector, for checking a configured installation_id. Nil under a PAT.
	lookup func(s *config.Scope) (*creds.AppSource, error)
	scopes []config.Scope
}

func newAuth(cfg *config.Config, conn *forgeapi.Connection, opts []forgeapi.Option) (*auth, error) {
	a := &auth{byScope: map[string]forgeapi.CredentialSource{}, limit: map[string]string{}, scopes: cfg.Scopes}
	if cfg.GitHub.AppKey == nil {
		pat := patSource{cfg.GitHub.Token}
		for i := range cfg.Scopes {
			a.byScope[cfg.Scopes[i].Key] = pat
		}
		return a, nil
	}
	a.lookup = func(s *config.Scope) (*creds.AppSource, error) {
		return creds.NewAppSource(*conn, cfg.GitHub.AppKey, creds.AppSourceConfig{Installation: selector(s), Grant: grant(s)}, opts...)
	}
	for i := range cfg.Scopes {
		s := &cfg.Scopes[i]
		a.limit[s.Key] = strings.ToLower(s.Owner)
		// A configured ID names the account's one installation;
		// CheckInstallations proves it.
		in := selector(s)
		if s.InstallationID != 0 {
			in = creds.InstallationByID(s.InstallationID)
		}
		src, err := creds.NewAppSource(*conn, cfg.GitHub.AppKey, creds.AppSourceConfig{Installation: in, Grant: grant(s)}, opts...)
		if err != nil {
			return nil, fmt.Errorf("github: token source for %s: %w", s.Key, err)
		}
		a.byScope[s.Key] = src
	}
	return a, nil
}

func selector(s *config.Scope) creds.Installation {
	if s.Kind == config.ScopeRepo {
		return creds.InstallationOfRepo(s.Owner + "/" + s.Repo)
	}
	return creds.InstallationOfOrg(s.Owner)
}

// grant is s's REST grant: a repo scope's names its one repository, an
// organization scope's none.
func grant(s *config.Scope) creds.AppGrant {
	if s.Kind == config.ScopeRepo {
		return creds.AppGrant{Permissions: repoGrant, Repositories: []string{s.Repo}}
	}
	return creds.AppGrant{Permissions: orgGrant}
}

func (a *auth) source(s *config.Scope) forgeapi.CredentialSource { return a.byScope[s.Key] }

// installation answers the App installation that serves s: its configured
// installation_id, or its account's installation as GitHub answers now, so a
// reinstall's new ID shows once the old one's tokens are refused. It answers
// 0 under a PAT. A failure is counted as an installation error.
func (r *REST) installation(ctx context.Context, s *config.Scope) (int64, error) {
	if s.InstallationID != 0 || r.auth.lookup == nil {
		return s.InstallationID, nil
	}
	src, ok := r.auth.source(s).(*creds.AppSource)
	if !ok {
		return 0, fmt.Errorf("github: no App token source for %s", s.Key)
	}
	id, err := src.Resolve(ctx)
	if err != nil {
		r.failure(ctx, obs.GitHubInstallation)
		return 0, fmt.Errorf("github: installation for %s: %w", s.Key, err)
	}
	return id, nil
}

// CheckInstallations proves each configured installation_id owns its scope,
// by looking the scope's installation up as GitHub answers it. It sends
// nothing under a PAT or when no scope sets an ID.
func (r *REST) CheckInstallations(ctx context.Context) error {
	if r.auth.lookup == nil {
		return nil
	}
	for i := range r.auth.scopes {
		s := &r.auth.scopes[i]
		if s.InstallationID == 0 {
			continue
		}
		src, err := r.auth.lookup(s)
		if err != nil {
			return fmt.Errorf("github: installation lookup for %s: %w", s.Key, err)
		}
		id, err := src.Resolve(ctx)
		if err != nil {
			r.failure(ctx, obs.GitHubInstallation)
			return fmt.Errorf("github: installation for %s: %w", s.Key, err)
		}
		if id != s.InstallationID {
			return fmt.Errorf("github: %s is served by installation %d, not the configured installation_id %d", s.Key, id, s.InstallationID)
		}
	}
	return nil
}

// patSource answers a personal access token, which never rotates.
type patSource struct{ token config.Secret }

func (p patSource) Token(context.Context) (string, error) { return p.token.Reveal(), nil }
func (patSource) Kind() forgeapi.CredKind                 { return forgeapi.CredKindStaticPAT }
func (patSource) State() forgeapi.CredState               { return forgeapi.CredValid }

var _ forgeapi.CredentialSource = patSource{}
