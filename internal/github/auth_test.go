package github

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const appLayout = `scopes:
  - repo: acme/app
  - repo: acme/web
  - org: acme
  - repo: solo/tool
    installation_id: 77
profiles:
  small: {scopes: [acme/app]}
`

const mint42 = "/app/installations/42/access_tokens"

// tokenRequest is a token request's grant as forgeapi sends it.
type tokenRequest struct {
	Permissions  map[string]string `json:"permissions"`
	Repositories []string          `json:"repositories"`
}

// mints decodes the token requests sent to path.
func mints(t *testing.T, f *fakeGitHub, path string) []tokenRequest {
	t.Helper()
	var out []tokenRequest
	for _, c := range f.seen(path) {
		var m tokenRequest
		if err := json.Unmarshal([]byte(c.body), &m); err != nil {
			t.Fatalf("token request %q: %v", c.body, err)
		}
		out = append(out, m)
	}
	return out
}

func TestAppGrantPerScope(t *testing.T) {
	f := newFake(t)
	cfg := load(t, true, appLayout)
	r, _, _ := newTestREST(t, f, cfg)
	f.app(42, "/repos/acme/app/installation", "/repos/acme/web/installation", "/orgs/acme/installation")
	f.route("/repos/acme/web", jsonOK(`{"private":true}`))
	f.route("/repos/acme/app", jsonOK(`{"private":true}`))
	f.route("/orgs/acme/actions/runner-groups", jsonOK(`{"runner_groups":[{"name":"Default","allows_public_repositories":false}]}`))
	for _, key := range []string{"acme/web", "acme/app", "acme"} {
		if ok, err := r.Private(t.Context(), scope(t, cfg, key)); !ok || err != nil {
			t.Fatalf("Private(%s) = %v, %v", key, ok, err)
		}
	}
	got := mints(t, f, mint42)
	if len(got) != 3 {
		t.Fatalf("mints = %+v, want one per repo scope and one for the organization", got)
	}
	repoPerms := map[string]string{"metadata": "read", "administration": "read", "actions": "read"}
	for i, want := range []string{"web", "app"} {
		if !maps.Equal(got[i].Permissions, repoPerms) || !slices.Equal(got[i].Repositories, []string{want}) {
			t.Errorf("repo-scope grant %d = %+v, want metadata, administration and actions read over %s alone", i, got[i], want)
		}
	}
	if org := got[2]; !maps.Equal(org.Permissions, map[string]string{"metadata": "read", "organization_self_hosted_runners": "read"}) || org.Repositories != nil {
		t.Errorf("org-scope grant = %+v, want metadata and self-hosted runners read with no repository list", org)
	}
	if c := f.seen("/repos/acme/web"); len(c) != 1 || !strings.HasPrefix(c[0].header.Get("Authorization"), "Bearer ghs_minted") {
		t.Errorf("read = %+v, want it sent with the minted token", c)
	}
}

// GitHub refuses a whole mint when one named repository is gone from the
// installation, so a gone repository must not take its siblings with it.
func TestAppGoneRepositoryStopsOnlyItsScope(t *testing.T) {
	f := newFake(t)
	cfg := load(t, true, appLayout)
	r, _, _ := newTestREST(t, f, cfg)
	f.app(42, "/repos/acme/app/installation", "/repos/acme/web/installation")
	minted := f.routes[mint42]
	f.route(mint42, func(w http.ResponseWriter, req *http.Request) {
		var m tokenRequest
		_ = json.NewDecoder(req.Body).Decode(&m)
		if slices.Contains(m.Repositories, "web") {
			reply(http.StatusUnprocessableEntity, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`)(w, req)
			return
		}
		minted(w, req)
	})
	f.route("/repos/acme/app", jsonOK(`{"private":true}`))
	if ok, err := r.Private(t.Context(), scope(t, cfg, "acme/web")); ok || err == nil {
		t.Errorf("Private(acme/web) = %v, %v; want the refused mint to prove nothing", ok, err)
	}
	if ok, err := r.Private(t.Context(), scope(t, cfg, "acme/app")); !ok || err != nil {
		t.Errorf("Private(acme/app) = %v, %v; want its own grant to mint and read", ok, err)
	}
}

func TestInstallation(t *testing.T) {
	t.Run("resolvedPerScope", func(t *testing.T) {
		f := newFake(t)
		cfg := load(t, true, appLayout)
		r, _, _ := newTestREST(t, f, cfg)
		f.app(42, "/repos/acme/app/installation", "/repos/acme/web/installation", "/orgs/acme/installation")
		for key, want := range map[string]int64{"acme/web": 42, "acme": 42, "solo/tool": 77} {
			if id, err := r.Installation(t.Context(), scope(t, cfg, key)); id != want || err != nil {
				t.Errorf("Installation(%s) = %d, %v; want %d", key, id, err, want)
			}
		}
		if len(f.seen("/repos/solo/tool/installation")) != 0 {
			t.Error("a configured installation_id was looked up")
		}
	})
	t.Run("configuredIDChecked", func(t *testing.T) {
		for owner, fails := range map[int64]bool{77: false, 78: true} {
			f := newFake(t)
			r, _, _ := newTestREST(t, f, load(t, true, appLayout))
			f.route("/repos/solo/tool/installation", jsonOK(fmt.Sprintf(`{"id":%d}`, owner)))
			if err := r.CheckInstallations(t.Context()); (err != nil) != fails {
				t.Errorf("CheckInstallations with the scope on %d = %v, want error %v", owner, err, fails)
			}
		}
	})
	t.Run("lookupFails", func(t *testing.T) {
		f := newFake(t)
		cfg := load(t, true, appLayout)
		r, m, _ := newTestREST(t, f, cfg)
		if _, err := r.Installation(t.Context(), scope(t, cfg, "acme/app")); err == nil {
			t.Error("Installation answered with no installation on GitHub")
		}
		if got := sample(t, m, `manade_github_errors_total{op="installation"}`); got != 1 {
			t.Errorf("installation errors = %v, want 1", got)
		}
	})
	t.Run("patSendsNothing", func(t *testing.T) {
		f := newFake(t)
		cfg := load(t, false, patLayout)
		r, _, _ := newTestREST(t, f, cfg)
		if id, err := r.Installation(t.Context(), scope(t, cfg, "acme/app")); id != 0 || err != nil || r.CheckInstallations(t.Context()) != nil {
			t.Errorf("Installation under a PAT = %d, %v; want 0", id, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("calls = %+v, want none", f.calls)
		}
	})
}

func TestRefusedMintFundsNothing(t *testing.T) {
	f := newFake(t)
	cfg := load(t, true, appLayout)
	r, _, _ := newTestREST(t, f, cfg)
	f.route("/repos/acme/app/installation", jsonOK(`{"id":42}`))
	f.route(mint42, reply(http.StatusUnprocessableEntity, `{"message":"permission not granted"}`))
	if ok, err := r.Private(t.Context(), scope(t, cfg, "acme/app")); ok || err == nil {
		t.Errorf("Private = %v, %v; want a refused mint to prove nothing", ok, err)
	}
	s := NewSweep(cfg, r)
	for _, q := range s.sweep(t.Context(), true) {
		if q.Complete {
			t.Errorf("sweep read %s whole without a token", q.Scope)
		}
	}
}
