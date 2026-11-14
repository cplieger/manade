package resolver

import (
	"strings"
	"testing"
)

// meta is a slice of GitHub's published actions_inbound list, as GET /meta
// gave it on 2026-11-14, its bare wildcard entries among it.
var meta = Domains{
	Full: []string{"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "ghcr.io"},
	Wildcard: []string{
		"*.githubusercontent.com", "*.core.windows.net", "*.githubapp.com", "*.github.com", "github.com", "ghcr.io",
	},
}

func TestMatcher(t *testing.T) {
	m := newMatcher(meta)
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"codeload.github.com", true},
		{"CodeLoad.GitHub.com", true},
		{"codeload.github.com.", true},
		{"ghcr.io", true},
		{"results-receiver.actions.githubusercontent.com", true},
		{"pkg-containers.githubusercontent.com.", true},
		{"mystorage.blob.core.windows.net", true},
		{"githubusercontent.com", false},
		{"core.windows.net", false},
		{"www.ghcr.io", false},
		{"evilgithub.com", false},
		{"github.com.evil.example", false},
		{"registry-1.docker.io", false},
		{"", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.Match(tc.name); got != tc.want {
				t.Errorf("Match(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestBootstrapListsOnlyTheAPI(t *testing.T) {
	m := newMatcher(bootstrap)
	if !m.Match("api.github.com") {
		t.Error("the bootstrap list does not list api.github.com")
	}
	for _, n := range []string{"github.com", "codeload.github.com", "ghcr.io"} {
		if m.Match(n) {
			t.Errorf("the bootstrap list lists %s", n)
		}
	}
}

func TestEveryListListsTheAPI(t *testing.T) {
	if !newMatcher(Domains{Full: []string{"github.com"}}).Match("api.github.com") {
		t.Error("a list without api.github.com lost the name its next read needs")
	}
}

func TestDomainsCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Domains
		want string
	}{
		{"empty", Domains{}, "empty"},
		{"a full entry with a space", Domains{Full: []string{"git hub.com"}}, `"git hub.com"`},
		{"a wildcard in a full entry", Domains{Full: []string{"*.github.com"}}, `"*.github.com"`},
		{"a bare star", Domains{Wildcard: []string{"*"}}, `"*"`},
		{"an empty label", Domains{Full: []string{"github..com"}}, `"github..com"`},
		{"a label ending in a dash", Domains{Full: []string{"github-.com"}}, `"github-.com"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.d.check()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("check = %v, want a refusal naming %s", err, tc.want)
			}
		})
	}
	if err := meta.check(); err != nil {
		t.Errorf("check(meta) = %v", err)
	}
}
