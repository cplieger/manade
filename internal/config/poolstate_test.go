package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// twoScopes serves acme/app and acme/web, on the one host, with the scope
// rows' default_profile members app and web and the profiles' members small
// and build.
func twoScopes(app, web, small, build string) string {
	return `{"scopes": [{"repo": "acme/app", "default_profile": ` + app + `}, {"repo": "acme/web", "default_profile": ` + web + `}],
		"hosts": {"default": {}},
		"profiles": {"small": {` + small + `}, "build": {` + build + `}}}`
}

func buildJSON(prev *Config, d string) error { return build(&Config{}, []byte(d), prev, nil) }

func TestPoolStateParses(t *testing.T) {
	t.Parallel()
	cfg := mustBuildJSON(t, &Config{}, twoScopes(`"small"`, `"build", "state": "stopping"`, `"retiring": ["acme/web"]`, `"deleting": true, "retiring": ["acme/web"]`))
	if s := cfg.Scopes; s[0].Stopping || !s[1].Stopping {
		t.Errorf("scopes = %+v, want acme/web stopping alone", s)
	}
	small, build := profile(t, cfg, "small"), profile(t, cfg, "build")
	if !slices.Equal(small.Retiring, []string{"acme/web"}) || small.Deleting {
		t.Errorf("small = %+v, want leaving acme/web", small)
	}
	if !build.Deleting {
		t.Errorf("build = %+v, want deleting", build)
	}
	t.Run("usable", func(t *testing.T) {
		t.Parallel()
		if !small.usable("acme/app") || small.usable("ACME/Web") || build.usable("acme/app") {
			t.Errorf("usable: small app %v, small web %v, build app %v; want true, false, false",
				small.usable("acme/app"), small.usable("ACME/Web"), build.usable("acme/app"))
		}
	})
}

func TestPoolStateRefusals(t *testing.T) {
	t.Parallel()
	org := func(profiles string) string {
		return `{"scopes": [{"repo": "acme/app", "default_profile": "build"}, {"org": "acme", "default_profile": "small"}],
			"hosts": {"default": {}}, "profiles": {` + profiles + `}}`
	}
	for _, tc := range []struct{ name, doc, want string }{
		{"deletingWord", twoScopes(`"small"`, `"build"`, `"deleting": "soon"`, ""), "profiles.small.deleting: want true or false"},
		{"retiringRepeated", twoScopes(`"small"`, `"build"`, `"retiring": ["acme/web", "ACME/web"]`, ""), `profiles.small.retiring[1]: repeats scope "ACME/web"`},
		{"retiringEmpty", twoScopes(`"small"`, `"build"`, `"retiring": [""]`, ""), "profiles.small.retiring[0]: want a scope key"},
		{"defaultMissing", twoScopes(`"medium"`, `"build"`, "", ""), `scopes[0].default_profile: names no profile "medium"`},
		{"defaultDeleting", twoScopes(`"small"`, `"build"`, "", `"deleting": true`), `scopes[1].default_profile: "build" is being deleted; choose another default profile`},
		{"defaultRetiring", twoScopes(`"small"`, `"small"`, `"retiring": ["acme/web"]`, ""), `scopes[1].default_profile: "small" is this scope's default profile but "acme/web" may not use it; choose another`},
		{"defaultNotAllowed", twoScopes(`"small"`, `"small"`, `"scopes": ["acme/app"]`, ""), `scopes[1].default_profile: "small" is this scope's default profile but "acme/web" may not use it`},
		{"onlyUsable", twoScopes(`"small"`, `"build"`, `"scopes": ["acme/app"]`, `"retiring": ["acme/web"]`), `scopes[1]: "build" is the only profile "acme/web" may use: turn the scope off, or let it use another profile`},
		{"orgDefaultNotAllowed", org(`"small": {"scopes": ["acme/app"]}, "build": {}`), `scopes[1].default_profile: "small" is this scope's default profile but "acme" may not use it; choose another`},
		{"orgOnlyUsable", org(`"small": {"retiring": ["acme"]}, "build": {"scopes": ["acme/app"]}`), `scopes[1]: "small" is the only profile "acme" may use: turn the scope off, or let it use another profile`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wantErr(t, buildJSON(nil, tc.doc), tc.want)
		})
	}
}

// A stopping scope routes no new job, so its default profile may be one it
// is leaving or one being deleted.
func TestStoppingScopeKeepsItsDefault(t *testing.T) {
	t.Parallel()
	mustBuildJSON(t, &Config{}, twoScopes(`"small"`, `"build", "state": "stopping"`, `"scopes": ["acme/app"]`, `"deleting": true, "retiring": ["acme/web"]`))
}

func TestNewProfileUnderADeletingName(t *testing.T) {
	t.Parallel()
	prev := mustBuildJSON(t, &Config{}, twoScopes(`"small"`, `"small"`, "", `"deleting": true, "retiring": ["acme/web"]`))
	for _, tc := range []struct {
		name, build string
		refused     bool
	}{
		{"recreated", `"cpus": 4`, true},
		{"stillDeleting", `"deleting": true, "retiring": ["acme/web"]`, false},
		{"retirementCleared", `"deleting": true`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := buildJSON(prev, twoScopes(`"small"`, `"small"`, "", tc.build))
			if !tc.refused {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			wantErr(t, err, `profiles.build: "build" is still being deleted; choose another name, or wait until it is gone`)
		})
	}
	// The parser refuses only a deleting name that returns as an ordinary
	// profile; whether a save may omit a deleting profile is the save's rule.
	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		d := `{"scopes": [{"repo": "acme/app", "default_profile": "small"}], "hosts": {"default": {}}, "profiles": {"small": {}}}`
		if err := buildJSON(prev, d); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("firstDocument", func(t *testing.T) {
		t.Parallel()
		if err := buildJSON(nil, twoScopes(`"small"`, `"small"`, "", `"cpus": 4`)); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMaxLifetimeCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw     string
		refused bool
	}{
		{"120h", false},
		{"119h59m", false},
		{"120h1s", true},
		{"200h", true},
	} {
		t.Run(strings.NewReplacer(".", "dot").Replace(tc.raw), func(t *testing.T) {
			t.Parallel()
			d := object(jsonScopes, jsonHost, jsonProfile(`"max_lifetime": "`+tc.raw+`"`))
			cfg := &Config{}
			err := build(cfg, []byte(d), nil, nil)
			if !tc.refused {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if got := profile(t, cfg, "p").MaxLifetime; got > 5*24*time.Hour {
					t.Errorf("max_lifetime = %v", got)
				}
				return
			}
			wantErr(t, err, "profiles.p.max_lifetime: want at most 120h (5 days), GitHub's limit on a job on a self-hosted runner")
		})
	}
}

func TestHostAccepting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"default", "", true},
		{"drained", `, "accepting": false`, false},
		{"resumed", `, "accepting": true`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := object(jsonScopes, `"hosts": {"default": {"capacity": {"cpus": 16, "memory": "64GiB"}`+tc.raw+`}}`, jsonProfile(""))
			cfg := mustBuildJSON(t, &Config{}, d)
			if h, _ := cfg.Host("default"); h.Accepting != tc.want {
				t.Errorf("accepting = %v, want %v", h.Accepting, tc.want)
			}
		})
	}
	t.Run("word", func(t *testing.T) {
		t.Parallel()
		d := object(jsonScopes, `"hosts": {"default": {"accepting": "no"}}`, jsonProfile(""))
		wantErr(t, buildJSON(nil, d), `hosts.default.accepting: want true or false`)
	})
}

// A scale set serves the scopes its profile may use and those it is
// leaving, whose jobs still run; a profile being deleted serves only what
// it is leaving.
func TestProfileServes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		p       Profile
		app, wb bool
	}{
		{"everyScope", Profile{}, true, true},
		{"listed", Profile{Scopes: []string{"acme/app"}}, true, false},
		{"leavingListed", Profile{Retiring: []string{"ACME/Web"}}, true, true},
		{"leavingUnlisted", Profile{Scopes: []string{"acme/app"}, Retiring: []string{"acme/web"}}, true, true},
		{"deleting", Profile{Deleting: true, Retiring: []string{"acme/web"}}, false, true},
		{"deleted", Profile{Deleting: true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if a, w := tc.p.Serves("acme/app"), tc.p.Serves("acme/web"); a != tc.app || w != tc.wb {
				t.Errorf("serves app %v, web %v; want %v, %v", a, w, tc.app, tc.wb)
			}
		})
	}
}

// A profile being deleted only loses scopes: a save cannot give it one.
func TestDeletingProfileTakesNoNewScope(t *testing.T) {
	t.Parallel()
	prev := mustBuildJSON(t, &Config{}, twoScopes(`"small"`, `"small"`, "", `"deleting": true, "retiring": ["acme/web"]`))
	err := buildJSON(prev, twoScopes(`"small"`, `"small"`, "", `"deleting": true, "retiring": ["acme/web", "acme/app"]`))
	wantErr(t, err, `profiles.build.retiring[1]: "build" is being deleted, so no scope may start using it`)
	if err := buildJSON(prev, twoScopes(`"small"`, `"small"`, "", `"deleting": true, "retiring": ["ACME/web"]`)); err != nil {
		t.Errorf("the same scope in another case: %v", err)
	}
}
