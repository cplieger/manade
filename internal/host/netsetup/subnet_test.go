package netsetup

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/config"
)

func TestPickSubnet(t *testing.T) {
	withResolver := func(r string) *config.Host {
		h := section()
		h.DNS = []netip.Addr{addr(r)}
		return h
	}
	cases := map[string]struct {
		h      *config.Host
		want   string
		onLink []string
	}{
		"the first pool":                      {h: section(), want: "172.17.0.0/16"},
		"an allow entry inside 172.17.0.0/16": {h: section("172.17.4.10/32"), want: "172.18.0.0/16"},
		"a default route and an on-link /16":  {h: section(), onLink: []string{"172.17.0.0/16"}, want: "172.18.0.0/16"},
		"an on-link /12 covers every /16":     {h: section(), onLink: []string{"172.16.0.0/12"}, want: "192.168.0.0/20"},
		"an IPv6 route never blocks a pool":   {h: section(), onLink: []string{"fd00:c8::/64"}, want: "172.17.0.0/16"},
		"a resolver inside 172.17.0.0/16":     {h: withResolver("172.17.0.53"), want: "172.18.0.0/16"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var routes []netip.Prefix
			for _, r := range tc.onLink {
				routes = append(routes, pfx(r))
			}
			got, err := PickSubnet(tc.h, routes)
			if err != nil || got != pfx(tc.want) {
				t.Errorf("PickSubnet = %v, %v, want %s", got, err, tc.want)
			}
		})
	}
	t.Run("every pool taken", func(t *testing.T) {
		_, err := PickSubnet(section(), []netip.Prefix{pfx("172.16.0.0/12"), pfx("192.168.0.0/16")})
		if !errors.Is(err, errNoSubnet) || !strings.Contains(err.Error(), "hosts.lan.network.subnet") {
			t.Errorf("PickSubnet = %v, want a refusal naming hosts.lan.network.subnet", err)
		}
	})
}

func TestConflicts(t *testing.T) {
	h := section("10.0.0.0/8", "172.18.5.0/24")
	h.DNS = []netip.Addr{addr("172.18.0.53"), addr("1.1.1.1")}
	got := Conflicts(pfx("172.18.0.0/16"), h)
	if len(got) != 2 || !strings.Contains(got[0], "hosts.lan.dns[0]") || !strings.Contains(got[1], "hosts.lan.egress.allow[1]") {
		t.Errorf("Conflicts = %q, want the resolver dns[0] and allow[1]", got)
	}
	t.Run("an allow entry containing the subnet", func(t *testing.T) {
		if got := Conflicts(pfx("172.18.0.0/16"), section("172.16.0.0/12")); len(got) != 1 || !strings.Contains(got[0], "172.16.0.0/12") {
			t.Errorf("Conflicts = %q, want the containing entry", got)
		}
	})
}

func TestCheckSubnet(t *testing.T) {
	h := section()
	h.Subnet = pfx("172.20.0.0/24")
	err := CheckSubnet(h, []netip.Prefix{pfx("fd00::/64"), pfx("172.20.0.0/16")})
	if !errors.Is(err, errNoSubnet) || !strings.Contains(err.Error(), "172.20.0.0/24") || !strings.Contains(err.Error(), "172.20.0.0/16") {
		t.Errorf("CheckSubnet = %v, want a refusal naming both", err)
	}
	if err := CheckSubnet(h, []netip.Prefix{pfx("172.21.0.0/16")}); err != nil {
		t.Errorf("CheckSubnet clear of the routes = %v", err)
	}
}

func TestJobPrefix6(t *testing.T) {
	draw := func(b ...byte) *bytes.Reader { return bytes.NewReader(b) }
	t.Run("drawn, saved and kept", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "manade", "job-ipv6")
		got, err := JobPrefix6(t.Context(), path, nil, draw(0x12, 0x34, 0x56, 0x78, 0x9a))
		if err != nil || got != pfx("fd12:3456:789a::/64") {
			t.Fatalf("JobPrefix6 = %v, %v, want fd12:3456:789a::/64 from the drawn bytes", got, err)
		}
		again, err := JobPrefix6(t.Context(), path, nil, draw())
		if err != nil || again != got {
			t.Errorf("second JobPrefix6 = %v, %v, want the saved %v", again, err, got)
		}
	})
	t.Run("a saved prefix now on-link is drawn again", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "job-ipv6")
		if err := os.WriteFile(path, []byte("fd12:3456:789a::/64\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := JobPrefix6(t.Context(), path, []netip.Prefix{pfx("fd12:3456:789a::/48")}, draw(0xaa, 0xbb, 0xcc, 0xdd, 0xee))
		if err != nil || got != pfx("fdaa:bbcc:ddee::/64") {
			t.Errorf("JobPrefix6 = %v, %v, want a new draw", got, err)
		}
	})
	t.Run("a saved prefix outside fd00::/8 is drawn again", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "job-ipv6")
		if err := os.WriteFile(path, []byte("2001:db8::/64\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := JobPrefix6(t.Context(), path, nil, draw(1, 2, 3, 4, 5))
		if err != nil || got != pfx("fd01:203:405::/64") {
			t.Errorf("JobPrefix6 = %v, %v, want a new draw", got, err)
		}
	})
	t.Run("a failed draw", func(t *testing.T) {
		if _, err := JobPrefix6(t.Context(), filepath.Join(t.TempDir(), "p"), nil, draw(1)); err == nil {
			t.Error("JobPrefix6 took a short draw")
		}
	})
}
