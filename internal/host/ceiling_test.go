package host

import (
	"errors"
	"log/slog"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// waitingAs is host lan with environment self, waiting for its first
// section.
func waitingAs(t *testing.T, self *config.Self) *supervisor {
	t.Helper()
	return newSupervisor(self, &Options{Log: slog.New(slog.DiscardHandler)}, filepath.Join(t.TempDir(), "section.json"))
}

// refusedAt answers the field paths of err, a 422 from PUT /v1/section.
func refusedAt(t *testing.T, err error) []string {
	t.Helper()
	sr, ok := errors.AsType[*hostapi.SectionRefusedError](err)
	if !ok {
		t.Fatalf("PUT /v1/section = %v, want a refusal", err)
	}
	out := make([]string, len(sr.Errors))
	for i, e := range sr.Errors {
		out[i] = e.Path
	}
	return out
}

// The host's own check is the guard: a section sent straight to its API,
// past any check the control plane makes, is refused as a whole when it
// exceeds the host's ceiling, and the host keeps waiting for one it
// accepts.
func TestTheHostRefusesASectionBeyondItsCeiling(t *testing.T) {
	for _, tc := range []struct {
		name, host, path string
		self             config.Self
	}{
		{"open", `{"egress": {"open": true}}`, "hosts.lan.egress.open", config.Self{}},
		{"internetOnAPinnedHost", `{}`, "hosts.lan.egress.internet", config.Self{Ceiling: config.Ceiling{NoInternet: true}}},
		{"ipv6", `{"egress": {"ipv6": true}}`, "hosts.lan.egress.ipv6", config.Self{}},
		{
			"privateIPv4Allow", `{"egress": {"allow": [{"to": "192.168.1.5", "ports": [5432]}]}}`, "hosts.lan.egress.allow[0].to",
			config.Self{Ceiling: config.Ceiling{Private: prefixes("10.0.0.0/8")}},
		},
		{
			"ipv6Allow", `{"egress": {"ipv6": true, "allow": [{"to": "fd00:2::5", "ports": [5432]}]}}`, "hosts.lan.egress.allow[0].to",
			config.Self{Ceiling: config.Ceiling{IPv6: true, Private: prefixes("fd00:1::/48")}},
		},
		{
			"allowInsideBlock6", `{"egress": {"ipv6": true, "allow": [{"to": "2001:db8:1:5::/80", "ports": [443]}]}}`, "hosts.lan.egress.allow[0].to",
			config.Self{Ceiling: config.Ceiling{IPv6: true, Private: prefixes("2001:db8:1::/48"), Block6: prefixes("2001:db8:1:5::/64")}},
		},
		{
			"publicAllowOnAPinnedHost", `{"egress": {"internet": false, "allow": [{"to": "8.8.4.4", "ports": [443]}]}}`, "hosts.lan.egress.allow[0].to",
			config.Self{Ceiling: config.Ceiling{NoInternet: true}},
		},
		{
			"publicResolverOnAPinnedHost", `{"egress": {"internet": false}, "dns": ["9.9.9.9"]}`, "hosts.lan.dns[0]",
			config.Self{Ceiling: config.Ceiling{NoInternet: true}},
		},
		{
			"coveredGlobalIPv6ResolverOnAPinnedHost", `{"egress": {"internet": false, "ipv6": true}, "dns": ["2606:4700:4700::1111"]}`, "hosts.lan.dns[0]",
			config.Self{Ceiling: config.Ceiling{NoInternet: true, IPv6: true, Private: prefixes("2606:4700::/32")}},
		},
		{"privateResolver", `{"dns": ["192.168.1.53"]}`, "hosts.lan.dns[0]", config.Self{}},
		{
			"resolverInsideBlock6", `{"egress": {"ipv6": true}, "dns": ["2001:db8:1:5::53"]}`, "hosts.lan.dns[0]",
			config.Self{Ceiling: config.Ceiling{IPv6: true, Private: prefixes("2001:db8:1::/48"), Block6: prefixes("2001:db8:1:5::/64")}},
		},
		{
			"theRegistryAuthFile", `{}`, "profiles.small.mounts[0].bind",
			config.Self{RegistryAuth: config.RegistryAuth{File: "/run/secrets/registry"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			self := tc.self
			self.Name = "lan"
			s := waitingAs(t, &self)
			small := `"small": {"runner_image": "base", "cpus": 1, "memory": "512MiB", "scopes": ["acme/app"], "mounts": [{"bind": "/run/secrets", "target": "/m", "read_only": true}]}`
			if tc.name != "theRegistryAuthFile" {
				small = smallJSON
			}
			_, err := apiOf(s).PutSection(t.Context(), sectionOf(1, tc.host, small))
			if got := refusedAt(t, err); !slices.Equal(got, []string{tc.path}) {
				t.Errorf("refused at %q, want %s alone", got, tc.path)
			}
			st := statusOf(t, s)
			if st.Section.State != hostapi.SectionWaiting || st.Section.Refused == nil || st.Section.Hash != "" {
				t.Errorf("section status = %+v, want still waiting, holding the refusal", st.Section)
			}
		})
	}
}

// A section the ceiling refuses leaves an up host on the section it runs,
// its rules untouched, and its status names the refusal beside the
// ceiling.
func TestAnUpHostKeepsItsSectionWhenItsCeilingRefusesOne(t *testing.T) {
	s, f, _ := upSupervisor(t, sectionOf(1, allowHost, smallJSON))
	before := statusOf(t, s)
	loads := len(f.kernel.commands())
	_, err := apiOf(s).PutSection(t.Context(), sectionOf(2, `{"egress": {"allow": [{"to": "192.168.1.5", "ports": [5432]}]}}`, smallJSON))
	if got := refusedAt(t, err); !slices.Equal(got, []string{"hosts.lan.egress.allow[0].to"}) {
		t.Errorf("refused at %q", got)
	}
	st := statusOf(t, s)
	t.Run("theRunningSection", func(t *testing.T) {
		if st.Section.State != hostapi.SectionRefused || st.Section.Hash != before.Section.Hash || st.PolicyHash != before.PolicyHash {
			t.Errorf("section = %+v, policy %s; want the running section %s kept, policy %s", st.Section, st.PolicyHash, before.Section.Hash, before.PolicyHash)
		}
	})
	t.Run("theRules", func(t *testing.T) {
		if n := len(f.kernel.commands()); n != loads {
			t.Errorf("%d kernel commands ran for a refused section", n-loads)
		}
	})
	t.Run("theCeiling", func(t *testing.T) {
		want := hostapi.Ceiling{AllowOpen: true, AllowInternet: true, AllowIPv6: true, AllowPrivate: prefixes("10.0.0.0/8")}
		if !reflect.DeepEqual(st.Ceiling, want) {
			t.Errorf("ceiling = %+v, want %+v", st.Ceiling, want)
		}
	})
}

// A host waiting for its first section reports its ceiling, with each
// value it read narrower, so the control plane checks the first save.
func TestAWaitingHostReportsItsCeiling(t *testing.T) {
	c := config.Ceiling{NoInternet: true, Block6: prefixes("2001:db8:1:5::/64"), Warnings: []string{`MANADE_ALLOW_OPEN: "yes" is neither true nor false, so it reads false`}}
	s := waitingAs(t, &config.Self{Name: "lan", Ceiling: c})
	want := hostapi.Ceiling{Block6: c.Block6, Warnings: c.Warnings}
	if got := statusOf(t, s).Ceiling; !reflect.DeepEqual(got, want) {
		t.Errorf("ceiling = %+v, want %+v", got, want)
	}
}

// MANADE_BLOCK6 reaches the rules the host loads, ahead of the section's
// allow entry for the /48 that holds it.
func TestTheHostDropsItsBlock6AheadOfTheSection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := v6Fixture(t, &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::7008")}})
		p := f.platform()
		p.Block6 = prefixes("2001:db8:1:5::/64")
		self := lanSelf()
		self.Ceiling.Private = append(self.Ceiling.Private, prefixes("2001:db8:1::/48")...)
		self.Ceiling.Block6 = p.Block6
		upAs(t, self, sectionOf(1, `{"egress": {"ipv6": true, "allow": [{"to": "2001:db8:1::/48", "ports": [443]}]}}`, smallJSON), p)
		for _, chain := range []string{"MANADE-FWD", "MANADE-OUT"} {
			rules := f.kernel.chain(true, "filter", chain)
			drop := slices.IndexFunc(rules, func(r string) bool {
				return strings.Contains(r, "-d 2001:db8:1:5::/64") && strings.HasSuffix(r, "-j DROP")
			})
			allow := slices.IndexFunc(rules, func(r string) bool {
				return strings.Contains(r, "-d 2001:db8:1::/48") && (strings.HasSuffix(r, "-j ACCEPT") || strings.HasSuffix(r, "-j RETURN"))
			})
			if drop < 0 || allow < 0 || drop > allow {
				t.Errorf("%s: MANADE_BLOCK6 drop at %d, allow accept at %d; want the drop first:\n%s", chain, drop, allow, strings.Join(rules, "\n"))
			}
		}
	})
}
