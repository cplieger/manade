package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// scopes serves one repository on profile p, the profile every fixture
// defines.
const scopes = `scopes:
  - {repo: acme/app, default_profile: p}
`

// header is a document without profiles: one scope and one host, default.
const header = scopes + "hosts:\n  default: {capacity: {cpus: 16, memory: 64GiB}}\n"

// doc answers fixture y, written in YAML flow notation for brevity, as the
// JSON document the parser reads. The section rules' tables use it, since
// they test what a value means, not how it is spelt; the decode and the
// pool's state are tested on JSON literals. A fixture that is not YAML is a
// test bug.
func doc(y string) []byte {
	var v any
	if err := yaml.Unmarshal([]byte(y), &v); err != nil {
		panic("fixture: " + err.Error())
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic("fixture: " + err.Error())
	}
	return b
}

// buildDoc builds fixture y into cfg with no previous model.
func buildDoc(cfg *Config, y string) error { return build(cfg, doc(y), nil, nil) }

// withProfile builds header plus one profile "p" whose body is the flow
// mapping's contents.
func withProfile(body string) string {
	return header + "profiles:\n  p: {" + body + "}\n"
}

// withOpenProfile is withProfile with the one host, default, open.
func withOpenProfile(body string) string {
	return scopes + "hosts:\n  default:\n    capacity: {cpus: 16, memory: 64GiB}\n    egress: {open: true}\n" +
		"profiles:\n  p: {" + body + "}\n"
}

// object joins top-level members into one JSON document.
func object(members ...string) string { return "{" + strings.Join(members, ", ") + "}" }

// jsonScopes serves acme/app on profile p.
const jsonScopes = `"scopes": [{"repo": "acme/app", "default_profile": "p"}]`

// jsonHost is the one host, default.
const jsonHost = `"hosts": {"default": {"capacity": {"cpus": 16, "memory": "64GiB"}}}`

// jsonProfile is profile p holding body's members.
func jsonProfile(body string) string { return `"profiles": {"p": {` + body + `}}` }

// mustBuildJSON builds JSON document d into cfg with no previous model.
func mustBuildJSON(t *testing.T, cfg *Config, d string) *Config {
	t.Helper()
	if err := build(cfg, []byte(d), nil, nil); err != nil {
		t.Fatalf("build: %v", err)
	}
	return cfg
}

func mustBuild(t *testing.T, cfg *Config, y string) *Config {
	t.Helper()
	if err := buildDoc(cfg, y); err != nil {
		t.Fatalf("build: %v", err)
	}
	return cfg
}

// wantErr asserts err holds every substring.
func wantErr(t *testing.T, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %q", subs)
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("err = %q, want it to contain %q", err, s)
		}
	}
}

// paths answers the field paths err refuses, in order, or nil for none.
func paths(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		return nil
	}
	var fe FieldErrors
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want field errors", err)
	}
	out := make([]string, len(fe))
	for i, e := range fe {
		out[i] = e.Path
	}
	return out
}

func profile(t *testing.T, cfg *Config, name string) *Profile {
	t.Helper()
	p, ok := cfg.Profile(name)
	if !ok {
		t.Fatalf("no profile %q", name)
	}
	return p
}

func host(t *testing.T, cfg *Config, name string) *Host {
	t.Helper()
	h, ok := cfg.Host(name)
	if !ok {
		t.Fatalf("no host %q", name)
	}
	return h
}
