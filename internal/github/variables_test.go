package github

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"testing"
)

const varPath = "/repos/acme/app/actions/variables"

// The routing variable is written under its own grant naming the one
// repository, while the scope's own grant stays read-only.
func TestVariableWriteMintsItsOwnGrant(t *testing.T) {
	f := newFake(t)
	cfg, gh := load(t, true, appLayout)
	r, _, _ := newTestREST(t, f, gh)
	f.app(42, "/repos/acme/app/installation")
	f.route("/repos/acme/app", jsonOK(`{"private":true}`))
	f.route(varPath, reply(http.StatusCreated, `{}`))
	app := scope(t, cfg, "acme/app")
	if ok, err := r.Private(t.Context(), app); !ok || err != nil {
		t.Fatalf("Private = %v, %v", ok, err)
	}
	if err := r.SetVariable(t.Context(), app, "MANADE_RUNNER", "small"); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	got := mints(t, f, mint42)
	if len(got) != 2 {
		t.Fatalf("mints = %+v, want the scope's and the variable's", got)
	}
	if !maps.Equal(got[0].Permissions, repoGrant) || !slices.Equal(got[0].Repositories, []string{"app"}) {
		t.Errorf("scope grant = %+v, want read-only over app", got[0])
	}
	if want := map[string]string{"metadata": "read", "actions_variables": "write"}; !maps.Equal(got[1].Permissions, want) || !slices.Equal(got[1].Repositories, []string{"app"}) {
		t.Errorf("variable grant = %+v, want %v over app alone", got[1], want)
	}
	t.Run("updateThenCreate", func(t *testing.T) {
		patch, post := f.seen(varPath+"/MANADE_RUNNER"), f.seen(varPath)
		if len(patch) != 1 || len(post) != 1 {
			t.Fatalf("PATCH %d, POST %d; want an update answered 404 and then one create", len(patch), len(post))
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(post[0].body), &body); err != nil || !maps.Equal(body, map[string]string{"name": "MANADE_RUNNER", "value": "small"}) {
			t.Errorf("create body = %s, %v", post[0].body, err)
		}
	})
}

// An organization's routing variable is one organization variable, visible
// to its private repositories, under the organization's variable grant.
func TestOrganizationVariableIsPrivate(t *testing.T) {
	f := newFake(t)
	cfg, gh := load(t, true, appLayout)
	r, _, _ := newTestREST(t, f, gh)
	f.app(42, "/orgs/acme/installation")
	f.route("/orgs/acme/actions/variables/MANADE_RUNNER", reply(http.StatusNoContent, ""))
	if err := r.SetVariable(t.Context(), scope(t, cfg, "acme"), "MANADE_RUNNER", "wide"); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	got := mints(t, f, mint42)
	if want := map[string]string{"metadata": "read", "organization_actions_variables": "write"}; len(got) != 1 || !maps.Equal(got[0].Permissions, want) || got[0].Repositories != nil {
		t.Errorf("mints = %+v, want one of %v naming no repository", got, want)
	}
	c := f.seen("/orgs/acme/actions/variables/MANADE_RUNNER")
	var body map[string]string
	if len(c) != 1 || json.Unmarshal([]byte(c[0].body), &body) != nil || body["visibility"] != "private" || body["value"] != "wide" {
		t.Errorf("update = %+v, want value wide with private visibility", c)
	}
}

func TestVariableReadAndDelete(t *testing.T) {
	f := newFake(t)
	cfg, gh := load(t, false, appLayout)
	r, m, _ := newTestREST(t, f, gh)
	app, web := scope(t, cfg, "acme/app"), scope(t, cfg, "acme/web")
	f.route(varPath+"/MANADE_RUNNER", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		jsonOK(`{"name":"MANADE_RUNNER","value":"large"}`)(w, req)
	})
	t.Run("read", func(t *testing.T) {
		if v, found, err := r.Variable(t.Context(), app, "MANADE_RUNNER"); v != "large" || !found || err != nil {
			t.Errorf("Variable = %q, %v, %v; want large", v, found, err)
		}
		if v, found, err := r.Variable(t.Context(), web, "MANADE_RUNNER"); v != "" || found || err != nil {
			t.Errorf("Variable of an unset variable = %q, %v, %v; want not found", v, found, err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		if err := r.DeleteVariable(t.Context(), app, "MANADE_RUNNER"); err != nil {
			t.Errorf("DeleteVariable: %v", err)
		}
		if err := r.DeleteVariable(t.Context(), web, "MANADE_RUNNER"); err != nil {
			t.Errorf("DeleteVariable of an unset variable = %v, want deleted", err)
		}
		if c := f.seen(varPath + "/MANADE_RUNNER"); c[len(c)-1].header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("delete sent %q, want the token", c[len(c)-1].header.Get("Authorization"))
		}
	})
	if n := sample(t, m, `manade_github_errors_total{op="variable"}`); n != 0 {
		t.Errorf("variable errors = %v, want 0", n)
	}
}

// A credential refused the permission reads ErrVariablesForbidden, so the
// user is told to set the variable by hand; a rate limit is no refusal.
func TestVariableForbidden(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		f := newFake(t)
		cfg, gh := load(t, false, appLayout)
		r, m, _ := newTestREST(t, f, gh)
		f.route(varPath+"/MANADE_RUNNER", reply(http.StatusForbidden, `{"message":"Resource not accessible by personal access token"}`))
		if err := r.SetVariable(t.Context(), scope(t, cfg, "acme/app"), "MANADE_RUNNER", "small"); !errors.Is(err, ErrVariablesForbidden) {
			t.Errorf("SetVariable = %v, want ErrVariablesForbidden", err)
		}
		if n := sample(t, m, `manade_github_errors_total{op="variable"}`); n != 1 {
			t.Errorf("variable errors = %v, want 1", n)
		}
	})
	t.Run("rateLimit", func(t *testing.T) {
		f := newFake(t)
		cfg, gh := load(t, false, appLayout)
		r, _, _ := newTestREST(t, f, gh)
		f.route(varPath+"/MANADE_RUNNER", reply(http.StatusForbidden, `{"message":"API rate limit exceeded"}`, "X-RateLimit-Remaining", "0"))
		if err := r.DeleteVariable(t.Context(), scope(t, cfg, "acme/app"), "MANADE_RUNNER"); err == nil || errors.Is(err, ErrVariablesForbidden) {
			t.Errorf("DeleteVariable = %v, want a failure that is no refusal", err)
		}
	})
	t.Run("appWithoutThePermission", func(t *testing.T) {
		f := newFake(t)
		cfg, gh := load(t, true, appLayout)
		r, _, _ := newTestREST(t, f, gh)
		f.app(42, "/repos/acme/app/installation")
		f.route(mint42, reply(http.StatusUnprocessableEntity, `{"message":"The permissions requested are not granted to this installation."}`))
		if _, _, err := r.Variable(t.Context(), scope(t, cfg, "acme/app"), "MANADE_RUNNER"); !errors.Is(err, ErrVariablesForbidden) {
			t.Errorf("Variable = %v, want ErrVariablesForbidden", err)
		}
	})
}
