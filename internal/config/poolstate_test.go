package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// twoScopes serves acme/app on small and acme/web on build, both profiles on
// the one host; state and body edit the scope rows and the profiles.
func twoScopes(app, web, small, build string) string {
	return "scopes:\n  - {repo: acme/app, default_profile: " + app + "}\n  - {repo: acme/web, default_profile: " + web + "}\n" +
		"hosts:\n  default: {}\n" +
		"profiles:\n  small: {" + small + "}\n  build: {" + build + "}\n"
}

func TestPoolStateParses(t *testing.T) {
	t.Parallel()
	cfg := mustBuild(t, &Config{}, twoScopes("small", "build, state: stopping", "retiring: [acme/web]", "deleting: true, retiring: [acme/web]"))
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
	for _, tc := range []struct{ name, doc, want string }{
		{"deletingWord", twoScopes("small", "build", "deleting: soon", ""), "profiles.small.deleting: want true or false"},
		{"retiringRepeated", twoScopes("small", "build", "retiring: [acme/web, ACME/web]", ""), `profiles.small.retiring[1]: repeats scope "ACME/web"`},
		{"retiringEmpty", twoScopes("small", "build", `retiring: [""]`, ""), "profiles.small.retiring[0]: want a scope key"},
		{"defaultMissing", twoScopes("medium", "build", "", ""), `scopes[0].default_profile: names no profile "medium"`},
		{"defaultDeleting", twoScopes("small", "build", "", "deleting: true"), `scopes[1].default_profile: "build" is being deleted; choose another default profile`},
		{"defaultRetiring", twoScopes("small", "small", "retiring: [acme/web]", ""), `scopes[1].default_profile: "small" is this repository's default profile but "acme/web" may not use it; choose another`},
		{"defaultNotAllowed", twoScopes("small", "small", "scopes: [acme/app]", ""), `scopes[1].default_profile: "small" is this repository's default profile but "acme/web" may not use it`},
		{"onlyUsable", twoScopes("small", "build", "scopes: [acme/app]", "retiring: [acme/web]"), `scopes[1]: "build" is the only profile "acme/web" may use: turn the repository off, or let it use another profile`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wantErr(t, buildDoc(&Config{}, tc.doc), tc.want)
		})
	}
}

// A stopping scope routes no new job, so its default profile may be one it
// is leaving or one being deleted.
func TestStoppingScopeKeepsItsDefault(t *testing.T) {
	t.Parallel()
	mustBuild(t, &Config{}, twoScopes("small", "build, state: stopping", "scopes: [acme/app]", "deleting: true, retiring: [acme/web]"))
}

func TestNewProfileUnderADeletingName(t *testing.T) {
	t.Parallel()
	prev := mustBuild(t, &Config{}, twoScopes("small", "small", "", "deleting: true, retiring: [acme/web]"))
	for _, tc := range []struct {
		name, build string
		refused     bool
	}{
		{"recreated", "cpus: 4", true},
		{"stillDeleting", "deleting: true, retiring: [acme/web]", false},
		{"retirementCleared", "deleting: true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := build(&Config{}, doc(twoScopes("small", "small", "", tc.build)), prev)
			if !tc.refused {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			wantErr(t, err, `profiles.build: "build" is still being deleted; choose another name, or wait until it is gone`)
		})
	}
	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		d := "scopes:\n  - {repo: acme/app, default_profile: small}\nhosts:\n  default: {}\nprofiles:\n  small: {}\n"
		if err := build(&Config{}, doc(d), prev); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("firstDocument", func(t *testing.T) {
		t.Parallel()
		mustBuild(t, &Config{}, twoScopes("small", "small", "", "cpus: 4"))
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
			err := buildDoc(&Config{}, withProfile("max_lifetime: "+tc.raw))
			if !tc.refused {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if got := profile(t, mustBuild(t, &Config{}, withProfile("max_lifetime: "+tc.raw)), "p").MaxLifetime; got > 5*24*time.Hour {
					t.Errorf("max_lifetime = %v", got)
				}
				return
			}
			wantErr(t, err, "profiles.p.max_lifetime: want at most 120h (5 days), GitHub's limit on a job on a self-hosted runner")
		})
	}
}
