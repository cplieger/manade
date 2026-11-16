package config

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func pfxs(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = pfx(s)
	}
	return out
}

// ceilingCase is a host part of host nas's settings under a ceiling, and
// the refusal it gets, or none.
type ceilingCase struct {
	name, host string
	// path and message are the one refusal; an empty path is an accepted
	// section.
	path, message string
	ceiling       Ceiling
}

// The host's own and learned /64 in these cases are 2001:db8:2:1::/64 and
// 2001:db8:2:2::/64, which no ceiling below lists.
var ceilingCases = []ceilingCase{
	{
		name: "openWithoutAllowOpen", host: `{"egress": {"open": true}}`,
		path: "hosts.nas.egress.open", message: "does not set MANADE_ALLOW_OPEN=true",
	},
	{
		name: "openOnAPinnedHost", host: `{"egress": {"open": true}}`, ceiling: Ceiling{Open: true, NoInternet: true},
		path: "hosts.nas.egress.open", message: "MANADE_ALLOW_INTERNET=false on host \"nas\"'s machine pins egress.internet to false",
	},
	{
		name: "internetOnAPinnedHost", host: `{}`, ceiling: Ceiling{NoInternet: true},
		path: "hosts.nas.egress.internet", message: "pins egress.internet to false",
	},
	{
		name: "ipv6WithoutAllowIPv6", host: `{"egress": {"ipv6": true}}`,
		path: "hosts.nas.egress.ipv6", message: "does not set MANADE_ALLOW_IPV6=true",
	},
	{
		name: "privateAllowOutsideAllowPrivate", host: `{"egress": {"allow": [{"to": "10.0.0.50", "ports": [5432]}]}}`,
		ceiling: Ceiling{Private: pfxs("192.168.0.0/16")},
		path:    "hosts.nas.egress.allow[0].to", message: "10.0.0.50 lies outside MANADE_ALLOW_PRIVATE (192.168.0.0/16)",
	},
	{
		name: "privateAllowWithNoAllowPrivate", host: `{"egress": {"allow": [{"to": "10.0.0.0/24", "ports": [5432]}]}}`,
		path: "hosts.nas.egress.allow[0].to", message: "MANADE_ALLOW_PRIVATE, which is empty",
	},
	{
		name: "ipv6AllowOutsideAllowPrivate", host: `{"egress": {"ipv6": true, "allow": [{"to": "fd00:2::5", "ports": [5432]}]}}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("fd00:1::/48")},
		path:    "hosts.nas.egress.allow[0].to", message: "fd00:2::5 lies outside MANADE_ALLOW_PRIVATE (fd00:1::/48)",
	},
	{
		name: "allowCoveringTheOwn64", host: `{"egress": {"ipv6": true, "allow": [{"to": "2001:db8:2:1::/64", "ports": [443]}]}}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("2001:db8:1::/48")},
		path:    "hosts.nas.egress.allow[0].to", message: "lies outside MANADE_ALLOW_PRIVATE",
	},
	{
		name: "allowCoveringTheLearned64", host: `{"egress": {"ipv6": true, "allow": [{"to": "2001:db8:2::/48", "ports": [443]}]}}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("2001:db8:1::/48")},
		path:    "hosts.nas.egress.allow[0].to", message: "lies outside MANADE_ALLOW_PRIVATE",
	},
	{
		name: "allowCoveringABlock6PrefixOutsideAllowPrivate", host: `{"egress": {"ipv6": true, "allow": [{"to": "2001:db8:1::/48", "ports": [443]}]}}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("2001:db8:1:7::/64"), Block6: pfxs("2001:db8:1:5::/64")},
		path:    "hosts.nas.egress.allow[0].to", message: "lies outside MANADE_ALLOW_PRIVATE",
	},
	{
		name: "allowWhollyInsideBlock6", host: `{"egress": {"ipv6": true, "allow": [{"to": "2001:db8:1:5::/80", "ports": [443]}]}}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("2001:db8:1::/48"), Block6: pfxs("2001:db8:1:5::/64")},
		path:    "hosts.nas.egress.allow[0].to", message: "lies wholly inside MANADE_BLOCK6",
	},
	{
		name: "publicAllowOnAPinnedHost", host: `{"egress": {"internet": false, "allow": [{"to": "8.8.4.4", "ports": [443]}]}}`,
		ceiling: Ceiling{NoInternet: true},
		path:    "hosts.nas.egress.allow[0].to", message: "8.8.4.4 is public, and MANADE_ALLOW_INTERNET=false",
	},
	{
		name: "publicResolverOnAPinnedHost", host: `{"egress": {"internet": false}, "dns": ["1.1.1.1", "9.9.9.9"]}`,
		ceiling: Ceiling{NoInternet: true},
		path:    "hosts.nas.dns[1]", message: "9.9.9.9 is a public resolver, and MANADE_ALLOW_INTERNET=false",
	},
	{
		name: "coveredGlobalIPv6ResolverOnAPinnedHost", host: `{"egress": {"internet": false, "ipv6": true}, "dns": ["2606:4700:4700::1111"]}`,
		ceiling: Ceiling{NoInternet: true, IPv6: true, Private: pfxs("2606:4700::/32")},
		path:    "hosts.nas.dns[0]", message: "is a public resolver",
	},
	{
		name: "privateResolverOutsideAllowPrivate", host: `{"dns": ["192.168.1.53"]}`,
		path: "hosts.nas.dns[0]", message: "192.168.1.53 lies outside MANADE_ALLOW_PRIVATE, which is empty",
	},
	{
		name: "ipv6ResolverOutsideAllowPrivate", host: `{"egress": {"ipv6": true}, "dns": ["fd00::53"]}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("fd00:1::/48")},
		path:    "hosts.nas.dns[0]", message: "fd00::53 lies outside MANADE_ALLOW_PRIVATE",
	},
	{
		name: "linkLocalResolver", host: `{"egress": {"ipv6": true}, "dns": ["fe80::1"]}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("fe80::/10")},
		path:    "hosts.nas.dns[0]", message: "outside loopback and link-local",
	},
	{
		name: "resolverInTheLearned64", host: `{"egress": {"ipv6": true}, "dns": ["2001:db8:2:2::53"]}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("fd00::/8")},
		path:    "hosts.nas.dns[0]", message: "lies outside MANADE_ALLOW_PRIVATE",
	},
	{
		name: "resolverInsideBlock6", host: `{"egress": {"ipv6": true}, "dns": ["2001:db8:1:5::53"]}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("2001:db8:1::/48"), Block6: pfxs("2001:db8:1:5::/64")},
		path:    "hosts.nas.dns[0]", message: "lies inside MANADE_BLOCK6",
	},
	{name: "theDefaults", host: `{}`},
	{name: "theDefaultsOnAPinnedHost", host: `{"egress": {"internet": false}}`, ceiling: Ceiling{NoInternet: true}},
	{
		name: "withinTheCeiling", host: `{"egress": {"ipv6": true, "allow": [{"to": "10.0.0.50", "ports": [5432]}, {"to": "fd00::5", "ports": [53], "protocol": "udp"}]}, "dns": ["10.0.0.53", "fd00::53", "1.1.1.1"]}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("10.0.0.0/8", "fd00::/8")},
	},
	{
		name: "theResolverOfACoveredLearned64", host: `{"egress": {"ipv6": true}, "dns": ["2001:db8:2:2::53"]}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("2001:db8:2:2::/64")},
	},
	{
		name: "allowCoveringABlock6PrefixInsideAllowPrivate", host: `{"egress": {"ipv6": true, "allow": [{"to": "2001:db8:1::/48", "ports": [443]}]}}`,
		ceiling: Ceiling{IPv6: true, Private: pfxs("2001:db8:1::/48"), Block6: pfxs("2001:db8:1:5::/64")},
	},
	{
		name: "block6LeavingOutTheHostsBlock6", host: `{"egress": {"ipv6": true, "block6": ["2001:db8:9::/48"]}}`,
		ceiling: Ceiling{IPv6: true, Block6: pfxs("2001:db8:1:5::/64")},
	},
	{
		name: "globalIPv6AllowOnAPinnedHost", host: `{"egress": {"internet": false, "ipv6": true, "allow": [{"to": "2001:db8:1::/48", "ports": [443]}]}}`,
		ceiling: Ceiling{NoInternet: true, IPv6: true, Private: pfxs("2001:db8:1::/48")},
	},
	{name: "anOpenHostsResolvers", host: `{"egress": {"open": true}, "dns": ["192.168.1.53"]}`, ceiling: Ceiling{Open: true}},
}

// sectionFor is host nas's section holding host part host and profile p.
func sectionFor(host string) string {
	return `{"hosts": {"nas": ` + host + `}, "profiles": {"p": {"scopes": ["acme/app"]}}}`
}

// documentFor is the settings document holding the same host and profile.
func documentFor(host string) string {
	return `{"scopes": [{"repo": "acme/app", "default_profile": "p"}], "hosts": {"nas": ` + host + `}, "profiles": {"p": {"hosts": ["nas"], "scopes": ["acme/app"]}}}`
}

// The host's own check of a section it was sent: each ceiling variable's
// refusals, with the rule's field error.
func TestParseSectionCeiling(t *testing.T) {
	t.Parallel()
	for _, tc := range ceilingCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			self := nasSelf()
			self.Ceiling = tc.ceiling
			_, err := ParseSection(self, []byte(sectionFor(tc.host)))
			checkRefusal(t, err, tc)
		})
	}
}

// A save checks each host against the ceiling it last reported, and gives
// the host's own field errors at the same paths: the fourth write path of
// the one parser.
func TestBuildChecksTheReportedCeiling(t *testing.T) {
	t.Parallel()
	for _, tc := range ceilingCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reported := map[string]*Ceiling{"nas": &tc.ceiling}
			err := build(&Config{}, []byte(documentFor(tc.host)), nil, reported)
			checkRefusal(t, err, tc)
			self := nasSelf()
			self.Ceiling = tc.ceiling
			_, serr := ParseSection(self, []byte(sectionFor(tc.host)))
			if got, want := paths(t, err), paths(t, serr); !slices.Equal(got, want) {
				t.Errorf("save paths = %q, the host's = %q", got, want)
			}
		})
	}
}

// A host that has never reported its ceiling is checked by the model
// alone; its first connection reports any refusal.
func TestBuildWithNoReportedCeiling(t *testing.T) {
	t.Parallel()
	for _, host := range []string{
		`{"egress": {"open": true}}`,
		`{"egress": {"ipv6": true, "allow": [{"to": "fd00::5", "ports": [53]}]}, "dns": ["192.168.1.53"]}`,
	} {
		if err := build(&Config{}, []byte(documentFor(host)), nil, nil); err != nil {
			t.Errorf("host %s: %v, want it accepted", host, err)
		}
		if err := build(&Config{}, []byte(documentFor(host)), nil, map[string]*Ceiling{"lan": {}}); err != nil {
			t.Errorf("host %s under another host's ceiling: %v, want it accepted", host, err)
		}
	}
}

// The registry credentials file is a protected path: its location comes
// from the host's environment, so no section mounts it into a job.
func TestParseSectionRefusesTheRegistryAuthFile(t *testing.T) {
	t.Parallel()
	self := nasSelf()
	self.RegistryAuth = RegistryAuth{File: "/run/secrets/registry"}
	for _, source := range []string{"/run/secrets/registry", "/run/secrets", "/run"} {
		section := `{"hosts": {"nas": {}}, "profiles": {"p": {"scopes": ["acme/app"], "mounts": [{"bind": "` + source + `", "target": "/m"}]}}}`
		_, err := ParseSection(self, []byte(section))
		if got := paths(t, err); !slices.Contains(got, "profiles.p.mounts[0].bind") {
			t.Errorf("mount of %s: paths = %q, want profiles.p.mounts[0].bind", source, got)
		}
	}
}

func checkRefusal(t *testing.T, err error, tc ceilingCase) {
	t.Helper()
	if tc.path == "" {
		if err != nil {
			t.Fatalf("err = %v, want the section accepted", err)
		}
		return
	}
	fe, ok := err.(FieldErrors)
	if !ok || len(fe) != 1 {
		t.Fatalf("err = %v, want one field error at %s", err, tc.path)
	}
	if fe[0].Path != tc.path || !strings.Contains(fe[0].Message, tc.message) {
		t.Errorf("refusal = %s: %s, want %s holding %q", fe[0].Path, fe[0].Message, tc.path, tc.message)
	}
}
