package github

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/obs"
)

const setLayout = `scopes:
  - repo: acme/app
  - org: acme
    runner_group: CI
profiles:
  small: {}
  big: {scopes: [acme/app]}
  wide: {scopes: [acme/app]}
  fresh: {scopes: [acme/app]}
`

// ids maps each scale set's profile to its ID.
func ids(sets []ScaleSet) map[string]int {
	out := map[string]int{}
	for _, ss := range sets {
		out[ss.Profile] = ss.ID
	}
	return out
}

func TestEnsure(t *testing.T) {
	f := newFake(t)
	cfg := load(t, false, setLayout)
	b := newBroker(t, f, "acme/app", "acme")
	b.groups["CI"] = 7
	// Each of sets 1, 4 and 5 differs in one way only.
	b.set(1, fakeSet{name: "small", labels: []string{"small", "extra"}, group: 1, disableUpdate: true})
	b.set(2, fakeSet{name: "old", labels: []string{"old"}, group: 1, disableUpdate: true})
	b.set(3, fakeSet{name: "small", labels: []string{"small"}, group: 7, disableUpdate: true})
	b.set(4, fakeSet{name: "big", labels: []string{"big"}, group: 1})
	b.set(5, fakeSet{name: "wide", labels: []string{"other"}, group: 1, disableUpdate: true})
	s, _, m, rec := newTestScaleSets(t, f, cfg, &clock{t: time.Now()})

	repo, err := s.Ensure(t.Context(), scope(t, cfg, "acme/app"))
	if err != nil {
		t.Fatalf("Ensure(acme/app) = %v", err)
	}
	org, err := s.Ensure(t.Context(), scope(t, cfg, "acme"))
	if err != nil {
		t.Fatalf("Ensure(acme) = %v", err)
	}

	t.Run("repoScope", func(t *testing.T) {
		got := ids(repo)
		if got["small"] != 1 || got["big"] != 4 || got["wide"] != 5 || got["fresh"] != 101 || len(got) != 4 {
			t.Errorf("scale sets = %v, want 1, 4 and 5 kept and fresh created as 101", got)
		}
		for _, ss := range repo {
			if ss.Scope != "acme/app" {
				t.Errorf("scale set %+v names another scope", ss)
			}
		}
	})
	t.Run("orgScopeUsesItsRunnerGroup", func(t *testing.T) {
		if got := ids(org); got["small"] != 3 || len(got) != 1 {
			t.Errorf("scale sets = %v, want only small, found in group 7; the others are not allowed there", got)
		}
	})
	t.Run("createdAndUpdatedToOneLabel", func(t *testing.T) {
		b.do(func(b *broker) {
			for id, name := range map[int]string{1: "small", 3: "small", 4: "big", 5: "wide", 101: "fresh"} {
				st := b.sets[id]
				if st.name != name || !slices.Equal(st.labels, []string{name}) || !st.disableUpdate {
					t.Errorf("set %d = %+v, want %s with only its own label and runner updates off", id, st, name)
				}
			}
			if b.sets[101].group != 1 {
				t.Errorf("fresh was created in group %d, want the repository's Default, 1", b.sets[101].group)
			}
			if len(b.patches) != 3 {
				t.Errorf("updates = %v, want sets 1, 4 and 5 only", b.patches)
			}
		})
	})
	t.Run("orphanReportedNeverDeleted", func(t *testing.T) {
		b.do(func(b *broker) {
			if _, ok := b.sets[2]; !ok || b.deletes != 0 {
				t.Error("an orphaned scale set was deleted")
			}
		})
		if n := rec.CountLevel(slog.LevelWarn, "config does not name"); n != 1 {
			t.Errorf("orphan WARN lines = %d, want 1", n)
		}
		if v := sample(t, m, `manade_scale_sets_orphaned{scope="acme/app"}`); v != 1 {
			t.Errorf("orphaned{acme/app} = %v, want 1", v)
		}
		if v := sample(t, m, `manade_scale_sets_orphaned{scope="acme"}`); v != 0 {
			t.Errorf("orphaned{acme} = %v, want 0", v)
		}
	})
	t.Run("unknownGroupFails", func(t *testing.T) {
		delete(b.groups, "CI")
		if _, err := s.Ensure(t.Context(), scope(t, cfg, "acme")); err == nil {
			t.Fatal("Ensure succeeded without the runner group")
		}
		if v := sample(t, m, `manade_github_errors_total{op="scaleset"}`); v != 1 {
			t.Errorf("scaleset errors = %v, want 1", v)
		}
	})
}

func TestJIT(t *testing.T) {
	f := newFake(t)
	cfg := load(t, false, setLayout)
	newBroker(t, f, "acme/app")
	s, _, m, _ := newTestScaleSets(t, f, cfg, &clock{t: time.Now()})
	ss := ScaleSet{obs.ScaleSet{Scope: "acme/app", Profile: "small"}, 9}
	j, err := s.JIT(t.Context(), ss, "manade-small-1")
	if err != nil || j.Config != "jit-manade-small-1" || j.RunnerID != 501 {
		t.Errorf("JIT = %+v, %v, want the config for the named runner and its registration ID", j, err)
	}
	f.route("/svc/_apis/runtime/runnerscalesets/9/generatejitconfig", reply(http.StatusForbidden, `{}`))
	if _, err := s.JIT(t.Context(), ss, "manade-small-2"); err == nil {
		t.Error("a refused JIT request succeeded")
	}
	if v := sample(t, m, `manade_github_errors_total{op="jit"}`); v != 1 {
		t.Errorf("jit errors = %v, want 1", v)
	}
}

func TestRemoveRunner(t *testing.T) {
	f := newFake(t)
	cfg := load(t, false, setLayout)
	b := newBroker(t, f, "acme/app")
	s, _, m, _ := newTestScaleSets(t, f, cfg, &clock{t: time.Now()})
	b.do(func(b *broker) {
		b.remove[2] = http.StatusNotFound
		b.remove[3] = http.StatusBadRequest
		b.remove[4] = http.StatusForbidden
	})
	for _, tc := range []struct {
		want   error
		name   string
		id     int64
		errors float64
		fails  bool
	}{
		{name: "removed", id: 1},
		{name: "alreadyGone", id: 2},
		{name: "busy", id: 3, want: ErrRunnerBusy},
		{name: "refused", id: 4, fails: true, errors: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.RemoveRunner(t.Context(), "acme/app", tc.id)
			switch {
			case tc.fails && (err == nil || errors.Is(err, ErrRunnerBusy)):
				t.Errorf("RemoveRunner = %v, want a failure that keeps the container", err)
			case !tc.fails && !errors.Is(err, tc.want):
				t.Errorf("RemoveRunner = %v, want %v", err, tc.want)
			}
			if v := sample(t, m, `manade_github_errors_total{op="scaleset"}`); v != tc.errors {
				t.Errorf("scaleset errors = %v, want %v", v, tc.errors)
			}
		})
	}
}

const reinstallLayout = `scopes:
  - repo: acme/app
  - repo: solo/tool
    installation_id: 77
profiles:
  small: {}
`

// A reinstall gives the installation a new ID, which the scope's client
// cannot follow on its own: Recheck rebuilds it, and its session reopens.
func TestRecheckRebuildsAfterReinstall(t *testing.T) {
	f := newFake(t)
	cfg := load(t, true, reinstallLayout)
	b := newBroker(t, f, "acme/app", "solo/tool")
	for _, id := range []string{"42", "43", "77"} {
		f.route("/api/v3/app/installations/"+id+"/access_tokens", reply(http.StatusCreated,
			`{"token":"ghs_scaleset`+id+`","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`))
	}
	f.app(42, "/repos/acme/app/installation")
	c := &clock{t: time.Now()}
	clockSleep(t, c)
	s, r, _, rec := newTestScaleSets(t, f, cfg, c)
	current := int64(42)
	s.resolve = func(ctx context.Context, sc *config.Scope) (int64, error) {
		if sc.Key == "acme/app" {
			return current, nil
		}
		return r.Installation(ctx, sc)
	}
	if _, err := s.JIT(t.Context(), ScaleSet{obs.ScaleSet{Scope: "solo/tool", Profile: "small"}, 5}, "r"); err != nil {
		t.Fatalf("JIT(solo/tool) = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := &handler{capacity: 1}
	h.after = func(n int) {
		switch n {
		case 1:
			current = 43
			if err := s.Recheck(ctx); err != nil {
				t.Errorf("Recheck = %v", err)
			}
		case 2:
			cancel()
		}
	}
	s.Listen(ctx, ScaleSet{obs.ScaleSet{Scope: "acme/app", Profile: "small"}, 1}, h)

	t.Run("mintsWithTheAppJWT", func(t *testing.T) {
		calls := f.seen("/api/v3/app/installations/42/access_tokens")
		if len(calls) != 1 {
			t.Fatalf("installation 42 mints = %d, want 1", len(calls))
		}
		auth := calls[0].header.Get("Authorization")
		if jwt, ok := strings.CutPrefix(auth, "Bearer "); !ok || strings.Count(jwt, ".") != 2 || strings.Contains(auth, "PRIVATE KEY") {
			t.Errorf("mint authorization = %q, want a signed JWT and never the key", auth)
		}
	})
	t.Run("rebuiltOnTheNewInstallation", func(t *testing.T) {
		if n := len(f.seen("/api/v3/app/installations/43/access_tokens")); n != 1 {
			t.Errorf("installation 43 mints = %d, want 1 from the rebuilt client", n)
		}
		if len(h.opened) != 2 {
			t.Errorf("sessions opened = %d, want 2: the first and the reopened one", len(h.opened))
		}
		b.do(func(b *broker) {
			if b.closes != 2 {
				t.Errorf("closes = %d, want the old session's and the shutdown's", b.closes)
			}
		})
		if n := rec.CountLevel(slog.LevelInfo, "installation changed"); n != 1 {
			t.Errorf("reinstall INFO lines = %d, want 1", n)
		}
	})
	t.Run("configuredIDKept", func(t *testing.T) {
		if n := len(f.seen("/api/v3/app/installations/77/access_tokens")); n != 1 {
			t.Errorf("installation 77 mints = %d, want the one before Recheck", n)
		}
		if n := len(f.seen("/repos/solo/tool/installation")); n != 0 {
			t.Errorf("solo/tool lookups = %d, want none: its installation_id is configured", n)
		}
	})
}
