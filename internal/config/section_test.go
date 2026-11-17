package config

import (
	"slices"
	"testing"
)

// nasSelf is the environment of host nas, whose ceiling lists 10.0.0.0/8.
func nasSelf() *Self {
	return &Self{Name: "nas", Ceiling: Ceiling{Private: pfxs("10.0.0.0/8")}}
}

func TestParseSection(t *testing.T) {
	t.Parallel()
	const d = `{"hosts": {"nas": {"dns": ["9.9.9.9"], "egress": {"allow": [{"to": "10.0.0.50", "ports": [5432]}]}, "network": {"subnet": "10.250.1.0/24"}}},
		"profiles": {"p": {"scopes": ["acme/app"], "cpus": 1, "memory": "512MiB"}, "q": {"cpus": 2, "memory": "1GiB", "scopes": ["acme/app"]}},
		"images": {"refresh": "30m"}, "pool": {"host_address": "cp.example:7464"}}`
	cfg, err := ParseSection(nasSelf(), []byte(d))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].Name != "nas" {
		t.Fatalf("hosts = %+v, want nas alone", cfg.Hosts)
	}
	h := &cfg.Hosts[0]
	t.Run("theHost", func(t *testing.T) {
		if h.Subnet.String() != "10.250.1.0/24" || len(h.Egress.Allow) != 1 || h.DNS[0].String() != "9.9.9.9" || h.ImageRefresh.Minutes() != 30 {
			t.Errorf("host = %+v", h)
		}
	})
	t.Run("eachProfileServedByTheHost", func(t *testing.T) {
		if len(cfg.Profiles) != 2 {
			t.Fatalf("profiles = %+v", cfg.Profiles)
		}
		for _, p := range cfg.Profiles {
			if !slices.Equal(p.Hosts, []string{"nas"}) {
				t.Errorf("profile %s hosts = %q, want [nas]", p.Name, p.Hosts)
			}
		}
	})
	t.Run("self", func(t *testing.T) {
		if cfg.Self == nil || cfg.Self.Name != "nas" {
			t.Errorf("self = %+v", cfg.Self)
		}
	})
}

// A host just joined serves no profile yet, and its section says so.
func TestParseSectionWithNoProfile(t *testing.T) {
	t.Parallel()
	cfg, err := ParseSection(nasSelf(), []byte(`{"hosts": {"nas": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Profiles) != 0 || len(cfg.Hosts) != 1 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestParseSectionRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, doc, path string }{
		{"noOwnHost", `{"hosts": {}}`, "hosts"},
		{"anotherHost", `{"hosts": {"nas": {}, "lan": {}}}`, "hosts.lan"},
		{"capacity", `{"hosts": {"nas": {"capacity": {"cpus": 2, "memory": "4GiB"}}}}`, "hosts.nas.capacity"},
		{"accepting", `{"hosts": {"nas": {"accepting": false}}}`, "hosts.nas.accepting"},
		{"profileHosts", `{"hosts": {"nas": {}}, "profiles": {"p": {"hosts": ["nas"]}}}`, "profiles.p.hosts"},
		{"profileDeleting", `{"hosts": {"nas": {}}, "profiles": {"p": {"deleting": true}}}`, "profiles.p.deleting"},
		{"profileRetiring", `{"hosts": {"nas": {}}, "profiles": {"p": {"retiring": ["acme/app"]}}}`, "profiles.p.retiring"},
		{"scopes", `{"hosts": {"nas": {}}, "scopes": []}`, "scopes"},
		{"queue", `{"hosts": {"nas": {}}, "queue": {}}`, "queue"},
		{"addressNoPort", `{"hosts": {"nas": {}}, "pool": {"host_address": "cp.example"}}`, "pool.host_address"},
		{"addressNoHost", `{"hosts": {"nas": {}}, "pool": {"host_address": ":7464"}}`, "pool.host_address"},
		{"addressPortZero", `{"hosts": {"nas": {}}, "pool": {"host_address": "cp.example:0"}}`, "pool.host_address"},
		{"notAnObject", `[]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseSection(nasSelf(), []byte(tc.doc))
			if got := paths(t, err); !slices.Contains(got, tc.path) {
				t.Errorf("paths = %q, want %q among them", got, tc.path)
			}
		})
	}
}

// A section refuses what the settings document refuses for the same host and
// profile, at the same paths: both run the one parser.
func TestParseSectionMatchesTheDocument(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, host, profile string }{
		{"privilegedOnAClosedHost", `{}`, `"privileged": true, "scopes": ["acme/app"]`},
		{"allowOutsidePrivate", `{"egress": {"allow": [{"to": "8.8.8.8"}]}}`, `"scopes": ["acme/app"]`},
		{"dockerHubOffTheInternet", `{"egress": {"internet": false}}`, `"image": "library/ubuntu:24.04", "scopes": ["acme/app"]`},
		{"aProtectedMount", `{}`, `"mounts": [{"bind": "/run/manade/x", "target": "/m"}], "scopes": ["acme/app"]`},
		{"aBadSize", `{}`, `"memory": "lots"`},
		{"ipv6ResolverWithoutIPv6", `{"dns": ["2606:4700:4700::1111"]}`, `"cpus": 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			section := `{"hosts": {"nas": ` + tc.host + `}, "profiles": {"p": {` + tc.profile + `}}}`
			document := `{"scopes": [{"repo": "acme/app", "default_profile": "p"}], "hosts": {"nas": ` + tc.host + `}, "profiles": {"p": {"hosts": ["nas"], ` + tc.profile + `}}}`
			self := nasSelf()
			_, serr := ParseSection(self, []byte(section))
			derr := build(&Config{Self: self}, []byte(document), nil, map[string]*Ceiling{"nas": &self.Ceiling})
			got, want := paths(t, serr), paths(t, derr)
			if len(want) == 0 {
				t.Fatal("setup: the document is accepted")
			}
			if !slices.Equal(got, want) {
				t.Errorf("section paths = %q, document paths = %q", got, want)
			}
		})
	}
}
