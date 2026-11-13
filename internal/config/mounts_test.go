package config

import (
	"slices"
	"strings"
	"testing"
)

// The protected paths of a host that is not open, with CONFIG_FILE
// /config/config.yaml, the default TLS_DIR and registry_auth
// /registry/auth.json.
var protectedPaths = []string{
	"/run/docker.sock", "/var/run/docker.sock", "/run/docker", "/var/lib/docker",
	"/run/manade", "/tls", "/config", "/registry/auth.json", "/tmp", "/proc", "/sys", "/dev",
}

func mountDoc(lines, mounts string) string {
	return oneHost("    registry_auth: /registry/auth.json\n"+lines, ", mounts: ["+mounts+"]")
}

func buildAt(t *testing.T, cfg *Config, doc string) error {
	t.Helper()
	cfg.File = "/config/config.yaml"
	return build(cfg, []byte(doc))
}

func TestProtectedPaths(t *testing.T) {
	t.Parallel()
	for _, prot := range protectedPaths {
		parent := prot[:strings.LastIndex(prot, "/")]
		if parent == "" {
			parent = "/"
		}
		for _, tc := range []struct{ name, source string }{
			{"equal", prot},
			{"under", prot + "/x"},
			{"ancestor", parent},
		} {
			t.Run(strings.ReplaceAll(prot, "/", "_")+"_"+tc.name, func(t *testing.T) {
				t.Parallel()
				mount := "{bind: " + tc.source + ", target: /m, read_only: true}"
				err := buildAt(t, tokenEnv(), mountDoc("", mount))
				wantErr(t, err, `profiles.p.mounts[0].bind: "`+tc.source+`" reaches `, `which host "h" protects; mount another path, or give the profile an open host`)
			})
		}
	}
}

func TestProtectedPathsAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, lines, mounts string }{
		{"siblingPrefixes", "", "{bind: /var/lib/docker2, target: /a}, {bind: /devices, target: /b}, {bind: /tmpfiles, target: /c}, {bind: /configs, target: /d}"},
		{"siblingOfTheFile", "", "{bind: /registry/other, target: /a}"},
		{"volumeNamedLikeAPath", "", "{volume: docker, target: /var/lib/docker}"},
		{"targetIsNotChecked", "", "{bind: /srv/cache, target: /var/lib/docker}"},
		{"openHost", "    egress: {open: true}\n", "{bind: /var/run/docker.sock, target: /var/run/docker.sock}, {bind: /config, target: /c}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := buildAt(t, tokenEnv(), mountDoc(tc.lines, tc.mounts)); err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A host checks its own TLS_DIR; the orchestrator, which cannot see it,
// checks the default.
func TestProtectedTLSDir(t *testing.T) {
	t.Parallel()
	self := func() *Config { return &Config{Self: &Self{Name: "h", TLSDir: "/certs"}} }
	t.Run("hostsOwn", func(t *testing.T) {
		t.Parallel()
		err := buildAt(t, self(), mountDoc("", "{bind: /certs, target: /c}"))
		wantErr(t, err, `profiles.p.mounts[0].bind: "/certs" reaches /certs`)
	})
	t.Run("hostsDefaultFree", func(t *testing.T) {
		t.Parallel()
		if err := buildAt(t, self(), mountDoc("", "{bind: /tls, target: /c}")); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("orchestratorDefault", func(t *testing.T) {
		t.Parallel()
		err := buildAt(t, tokenEnv(), mountDoc("", "{bind: /tls, target: /c}"))
		wantErr(t, err, `"/tls" reaches /tls`)
	})
}

func TestMountNesting(t *testing.T) {
	t.Parallel()
	twoProfiles := func(hostsQ, mountsP, mountsQ string) string {
		return scopes + `hosts:
  h: {capacity: {cpus: 2, memory: 4GiB}}
  g: {capacity: {cpus: 2, memory: 4GiB}}
profiles:
  p: {hosts: [h], scopes: [acme/app], mounts: [` + mountsP + `]}
  q: {hosts: [` + hostsQ + `], scopes: [acme/app], mounts: [` + mountsQ + `]}
`
	}
	t.Run("oneProfile", func(t *testing.T) {
		t.Parallel()
		doc := oneHost("", ", mounts: [{bind: /srv/data/a, target: /a, read_only: true}, {bind: /srv/data, target: /d}]")
		wantErr(t, buildAt(t, tokenEnv(), doc),
			`profiles.p.mounts[1].bind: "/srv/data" is writable to jobs and an ancestor of "/srv/data/a", the source of profiles.p.mounts[0].bind, on host "h"`)
	})
	t.Run("twoProfiles", func(t *testing.T) {
		t.Parallel()
		doc := twoProfiles("h", "{bind: /srv, target: /s}", "{bind: /srv/data, target: /d, read_only: true}")
		wantErr(t, buildAt(t, tokenEnv(), doc),
			`profiles.p.mounts[0].bind: "/srv" is writable to jobs and an ancestor of "/srv/data", the source of profiles.q.mounts[0].bind, on host "h"`)
	})
	for _, tc := range []struct{ name, hostsQ, p, q string }{
		{"otherHost", "g", "{bind: /srv, target: /s}", "{bind: /srv/data, target: /d}"},
		{"readOnlyAncestor", "h", "{bind: /srv, target: /s, read_only: true}", "{bind: /srv/data, target: /d}"},
		{"equalWritable", "h", "{bind: /srv/data, target: /s}", "{bind: /srv/data, target: /d}"},
		{"siblingPrefix", "h", "{bind: /srv/data, target: /s}", "{bind: /srv/database, target: /d}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := buildAt(t, tokenEnv(), twoProfiles(tc.hostsQ, tc.p, tc.q)); err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("openHost", func(t *testing.T) {
		t.Parallel()
		doc := oneHost("    egress: {open: true}\n", ", mounts: [{bind: /srv, target: /s}, {bind: /srv/data, target: /d}]")
		if err := buildAt(t, tokenEnv(), doc); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reportedOnce", func(t *testing.T) {
		t.Parallel()
		doc := scopes + `hosts:
  h: {capacity: {cpus: 2, memory: 4GiB}}
  g: {capacity: {cpus: 2, memory: 4GiB}}
profiles:
  p: {hosts: [h, g], scopes: [acme/app], mounts: [{bind: /srv, target: /s}, {bind: /srv/data, target: /d}]}
`
		err := buildAt(t, tokenEnv(), doc)
		wantErr(t, err, "is writable to jobs")
		if n := strings.Count(err.Error(), "is writable to jobs"); n != 1 {
			t.Errorf("err = %v, want the pair once, got %d", err, n)
		}
	})
}

func TestBindSources(t *testing.T) {
	t.Parallel()
	cfg := &Config{Profiles: []Profile{
		{Name: "p", Hosts: []string{"h"}, Mounts: []Mount{
			{Source: "cache", Target: "/c"},
			{Source: "/srv/a", Target: "/a", Bind: true, ReadOnly: true},
			{Source: "/srv/b", Target: "/b", Bind: true},
		}},
		{Name: "q", Hosts: []string{"g"}, Mounts: []Mount{{Source: "/srv/q", Target: "/q", Bind: true}}},
		{Name: "r", Hosts: []string{"g", "h"}, Mounts: []Mount{{Source: "/srv/r", Target: "/r", Bind: true}}},
	}}
	want := []BindSource{
		{Profile: "p", Index: 1, Mount: Mount{Source: "/srv/a", Target: "/a", Bind: true, ReadOnly: true}},
		{Profile: "p", Index: 2, Mount: Mount{Source: "/srv/b", Target: "/b", Bind: true}},
		{Profile: "r", Index: 0, Mount: Mount{Source: "/srv/r", Target: "/r", Bind: true}},
	}
	if got := cfg.BindSources("h"); !slices.Equal(got, want) {
		t.Errorf("BindSources(h) = %+v, want %+v", got, want)
	}
	if got := cfg.BindSources("none"); len(got) != 0 {
		t.Errorf("BindSources(none) = %+v, want none", got)
	}
}
