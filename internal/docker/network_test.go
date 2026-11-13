package docker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func jobSpec(ipv6 bool) *NetworkSpec {
	s := &NetworkSpec{Name: "manade", Bridge: "manade0", Subnet: netip.MustParsePrefix("172.18.0.0/16")}
	if ipv6 {
		s.Subnet6 = netip.MustParsePrefix("fd12:3456:789a:1::/64")
	}
	return s
}

// networkAnswer is GET /networks/{id} for a network matching jobSpec(ipv6).
func networkAnswer(ipv6 bool) map[string]any {
	cfg := []map[string]string{{"Subnet": "172.18.0.0/16", "Gateway": "172.18.0.1"}}
	if ipv6 {
		cfg = append([]map[string]string{{"Subnet": "fd12:3456:789a:1::/64"}}, cfg...)
	}
	return map[string]any{
		"Name": "manade", "Driver": "bridge", "EnableIPv6": ipv6,
		"IPAM":    map[string]any{"Config": cfg},
		"Options": map[string]string{optICC: "false", optBridgeName: "manade0"},
	}
}

func networkDaemon(t *testing.T, existing map[string]any) (*fakeDaemon, *Client) {
	t.Helper()
	d := newDaemon(t, "1.51")
	d.handle("GET /networks/{name}", func(w http.ResponseWriter, _ *http.Request) {
		if existing == nil {
			writeError(w, http.StatusNotFound, "network manade not found")
			return
		}
		writeJSON(w, http.StatusOK, existing)
	})
	d.handle("POST /networks/create", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "n1"})
	})
	return d, d.dial(t)
}

func TestEnsureNetworkCreates(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		name := "IPv4 only"
		if ipv6 {
			name = "with an IPv6 /64"
		}
		t.Run(name, func(t *testing.T) {
			d, c := networkDaemon(t, nil)
			if err := c.EnsureNetwork(t.Context(), jobSpec(ipv6)); err != nil {
				t.Fatalf("EnsureNetwork: %v", err)
			}
			var body networkWire
			if err := json.Unmarshal(d.seen(http.MethodPost, "/networks/create")[0].body, &body); err != nil {
				t.Fatal(err)
			}
			wantSubnets := 1
			if ipv6 {
				wantSubnets = 2
			}
			if body.Driver != "bridge" || body.Options[optICC] != "false" || body.Options[optBridgeName] != "manade0" ||
				body.EnableIPv6 != ipv6 || len(body.IPAM.Config) != wantSubnets || body.IPAM.Config[0].Subnet != "172.18.0.0/16" ||
				body.Labels[LabelManaged] != "true" {
				t.Errorf("create body = %+v", body)
			}
			if ipv6 && body.IPAM.Config[1].Subnet != "fd12:3456:789a:1::/64" {
				t.Errorf("IPv6 subnet = %+v", body.IPAM.Config)
			}
		})
	}
}

func TestEnsureNetworkChecksAnExistingOne(t *testing.T) {
	t.Run("a matching network is kept", func(t *testing.T) {
		for _, ipv6 := range []bool{false, true} {
			d, c := networkDaemon(t, networkAnswer(ipv6))
			if err := c.EnsureNetwork(t.Context(), jobSpec(ipv6)); err != nil || len(d.seen(http.MethodPost, "/networks/create")) != 0 {
				t.Errorf("ipv6 %v: EnsureNetwork = %v", ipv6, err)
			}
		}
	})
	cases := map[string]struct {
		edit func(map[string]any)
		want string
		ipv6 bool
	}{
		"another driver":    {edit: func(n map[string]any) { n["Driver"] = "macvlan" }, want: "driver"},
		"IPv6 not expected": {edit: func(n map[string]any) { n["EnableIPv6"] = true }, want: "IPv6"},
		"IPv6 missing":      {ipv6: true, edit: func(n map[string]any) { n["EnableIPv6"] = false }, want: "IPv6"},
		"another subnet": {edit: func(n map[string]any) {
			n["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": "10.9.0.0/24"}}}
		}, want: "subnets"},
		"an extra subnet":    {edit: func(n map[string]any) { n["IPAM"] = networkAnswer(true)["IPAM"] }, want: "subnets"},
		"another bridge":     {edit: func(n map[string]any) { n["Options"] = map[string]string{optICC: "false", optBridgeName: "br-x"} }, want: "bridge interface"},
		"ICC left on":        {edit: func(n map[string]any) { n["Options"] = map[string]string{optBridgeName: "manade0"} }, want: "inter-container"},
		"ICC true":           {edit: func(n map[string]any) { n["Options"] = map[string]string{optICC: "true", optBridgeName: "manade0"} }, want: "inter-container"},
		"ICC unparseable":    {edit: func(n map[string]any) { n["Options"] = map[string]string{optICC: "nope", optBridgeName: "manade0"} }, want: optICC},
		"a malformed subnet": {edit: func(n map[string]any) { n["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": "x"}}} }, want: "subnet"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			n := networkAnswer(tc.ipv6)
			tc.edit(n)
			d, c := networkDaemon(t, n)
			err := c.EnsureNetwork(t.Context(), jobSpec(tc.ipv6))
			if !errors.Is(err, errNetworkMismatch) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("EnsureNetwork = %v, want a mismatch naming %q", err, tc.want)
			}
			if len(d.seen(http.MethodPost, "/networks/create")) != 0 {
				t.Error("a mismatched network was created over")
			}
		})
	}
}

func TestEnsureNetworkPassesOtherErrors(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /networks/{name}", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusInternalServerError, "boom") })
	if err := d.dial(t).EnsureNetwork(t.Context(), jobSpec(false)); err == nil || errors.Is(err, errNetworkMismatch) {
		t.Errorf("EnsureNetwork = %v, want the daemon's error", err)
	}
}
