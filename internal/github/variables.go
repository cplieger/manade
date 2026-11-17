package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cplieger/forgeapi/v3"
	"github.com/cplieger/manade/internal/config"
)

// ErrVariablesForbidden reports a credential that may not read or write the
// scope's Actions variables: an App installed without Variables write, or a
// token without it. The user sets the variable by hand (§9.4).
var ErrVariablesForbidden = errors.New("github: the credential may not write the scope's Actions variables")

// variablesURL is the Actions variables of s, a repository or an
// organization scope.
func (r *REST) variablesURL(s *config.Scope) string {
	return r.api + accountPath(s) + "/actions/variables"
}

// Variable reads Actions variable name of s, a repository or organization
// scope, under the variable grant: its value, and found false when it is
// not set. A failed read is counted as a variable error.
func (r *REST) Variable(ctx context.Context, s *config.Scope, name string) (value string, found bool, err error) {
	a, err := r.variable(ctx, s, http.MethodGet, "/"+url.PathEscape(name), nil)
	if err == nil && a.status == http.StatusNotFound {
		return "", false, nil
	}
	if err == nil {
		err = refusal(&a)
	}
	var v struct {
		Value *string `json:"value"`
	}
	if err == nil {
		if err = json.Unmarshal(a.body, &v); err == nil && v.Value == nil {
			err = errors.New("github: the variable answer carries no value")
		}
	}
	if err != nil {
		r.failure(ctx, OpVariable)
		return "", false, r.forbidden(err)
	}
	return *v.Value, true, nil
}

// SetVariable sets Actions variable name of s to value: an update, or a
// create when it is not set. An organization's variable is visible to its
// private repositories only. A failed write is counted as a variable error.
func (r *REST) SetVariable(ctx context.Context, s *config.Scope, name, value string) error {
	body := map[string]string{"name": name, "value": value}
	if s.Kind == config.ScopeOrg {
		body["visibility"] = "private"
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("github: encoding a variable: %w", err)
	}
	a, err := r.variable(ctx, s, http.MethodPatch, "/"+url.PathEscape(name), data)
	if err == nil && a.status == http.StatusNotFound {
		a, err = r.variable(ctx, s, http.MethodPost, "", data)
	}
	if err == nil {
		err = refusal(&a)
	}
	if err != nil {
		r.failure(ctx, OpVariable)
		return r.forbidden(err)
	}
	return nil
}

// DeleteVariable deletes Actions variable name of s; one not set counts as
// deleted. A failed delete is counted as a variable error.
func (r *REST) DeleteVariable(ctx context.Context, s *config.Scope, name string) error {
	a, err := r.variable(ctx, s, http.MethodDelete, "/"+url.PathEscape(name), nil)
	if err == nil && a.status != http.StatusNotFound {
		err = refusal(&a)
	}
	if err != nil {
		r.failure(ctx, OpVariable)
		return r.forbidden(err)
	}
	return nil
}

// variable sends one request to s's variables under the variable grant.
func (r *REST) variable(ctx context.Context, s *config.Scope, method, path string, body []byte) (answer, error) {
	src, err := r.auth.sourceFor(s, true)
	if err != nil {
		return answer{}, err
	}
	return r.request(ctx, r.plain, src, method, r.variablesURL(s)+path, body)
}

// refusal is a's error, ErrVariablesForbidden for a 403 that is no rate
// limit.
func refusal(a *answer) error {
	err := a.err()
	if a.status == http.StatusForbidden && !rateLimited(a) {
		return fmt.Errorf("%w: %w", ErrVariablesForbidden, err)
	}
	return err
}

// forbidden marks a refused mint of the variable grant as
// ErrVariablesForbidden: GitHub answers 403, or 422 when the installation
// lacks a permission the grant asks for
// (https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app).
func (*REST) forbidden(err error) error {
	var fe *forgeapi.Error
	if errors.Is(err, ErrVariablesForbidden) || !errors.As(err, &fe) ||
		fe.Kind != forgeapi.KindForbidden && fe.Status != http.StatusUnprocessableEntity {
		return err
	}
	return fmt.Errorf("%w: %w", ErrVariablesForbidden, err)
}
