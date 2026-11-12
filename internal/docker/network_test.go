package docker

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func isolatedSpec() NetworkSpec {
	return NetworkSpec{Name: "manade-isolated", Bridge: "manade0", Subnet: netip.MustParsePrefix("172.30.0.0/24")}
}

// networkAnswer is GET /networks/{id} for a network matching isolatedSpec.
func networkAnswer() map[string]any {
	return map[string]any{
		"Name": "manade-isolated", "Driver": "bridge", "EnableIPv6": false,
		"IPAM":    map[string]any{"Driver": "default", "Config": []map[string]string{{"Subnet": "172.30.0.0/24", "Gateway": "172.30.0.1"}}},
		"Options": map[string]string{optICC: "false", optBridgeName: "manade0"},
	}
}

func networkDaemon(t *testing.T, existing map[string]any) (*fakeDaemon, *Client) {
	t.Helper()
	d := newDaemon(t, "1.51")
	d.handle("GET /networks/{name}", func(w http.ResponseWriter, _ *http.Request) {
		if existing == nil {
			writeError(w, http.StatusNotFound, "network manade-isolated not found")
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
	d, c := networkDaemon(t, nil)
	if err := c.EnsureNetwork(t.Context(), isolatedSpec()); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	reqs := d.seen(http.MethodPost, "/networks/create")
	if len(reqs) != 1 {
		t.Fatalf("creates = %d, want 1", len(reqs))
	}
	var body struct {
		Options    map[string]string
		Labels     map[string]string
		Name       string
		Driver     string
		IPAM       struct{ Config []struct{ Subnet string } }
		EnableIPv6 *bool
	}
	if err := json.Unmarshal(reqs[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "manade-isolated" || body.Driver != "bridge" {
		t.Errorf("Name, Driver = %q, %q", body.Name, body.Driver)
	}
	if want := map[string]string{optICC: "false", optBridgeName: "manade0"}; !maps.Equal(body.Options, want) {
		t.Errorf("Options = %v, want %v", body.Options, want)
	}
	if len(body.IPAM.Config) != 1 || body.IPAM.Config[0].Subnet != "172.30.0.0/24" {
		t.Errorf("IPAM = %+v", body.IPAM)
	}
	if body.EnableIPv6 != nil && *body.EnableIPv6 {
		t.Error("the network was created with IPv6 on")
	}
	if !maps.Equal(body.Labels, ownedLabels()) {
		t.Errorf("Labels = %v, want the pool labels", body.Labels)
	}
	t.Run("bridgeNetworkTurnsICCOn", func(t *testing.T) {
		d, c := networkDaemon(t, nil)
		spec := NetworkSpec{Name: "manade-bridge", Bridge: "manade1", Subnet: netip.MustParsePrefix("172.30.1.0/24"), ICC: true}
		if err := c.EnsureNetwork(t.Context(), spec); err != nil {
			t.Fatalf("EnsureNetwork: %v", err)
		}
		if b := string(d.seen(http.MethodPost, "/networks/create")[0].body); !strings.Contains(b, `"`+optICC+`":"true"`) {
			t.Errorf("create body = %s, want ICC true", b)
		}
	})
}

func TestEnsureNetworkChecksAnExistingOne(t *testing.T) {
	t.Run("aMatchingNetworkIsKept", func(t *testing.T) {
		d, c := networkDaemon(t, networkAnswer())
		if err := c.EnsureNetwork(t.Context(), isolatedSpec()); err != nil {
			t.Errorf("EnsureNetwork = %v", err)
		}
		if n := len(d.seen(http.MethodPost, "/networks/create")); n != 0 {
			t.Errorf("creates = %d, want 0", n)
		}
	})
	t.Run("aZeroICCOptionReadsAsOff", func(t *testing.T) {
		got := networkAnswer()
		got["Options"] = map[string]string{optICC: "0", optBridgeName: "manade0"}
		if _, c := networkDaemon(t, got); c.EnsureNetwork(t.Context(), isolatedSpec()) != nil {
			t.Error("ICC 0 was not read as off")
		}
	})
	for name, change := range map[string]func(map[string]any){
		"driver": func(n map[string]any) { n["Driver"] = "macvlan" },
		"ipv6":   func(n map[string]any) { n["EnableIPv6"] = true },
		"subnet": func(n map[string]any) {
			n["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": "172.31.0.0/24"}}}
		},
		"twoSubnets": func(n map[string]any) {
			n["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": "172.30.0.0/24"}, {"Subnet": "fd00::/64"}}}
		},
		"bridgeName":  func(n map[string]any) { n["Options"] = map[string]string{optICC: "false", optBridgeName: "br-1234"} },
		"iccDefault":  func(n map[string]any) { n["Options"] = map[string]string{optBridgeName: "manade0"} },
		"iccOn":       func(n map[string]any) { n["Options"] = map[string]string{optICC: "true", optBridgeName: "manade0"} },
		"iccGarbage":  func(n map[string]any) { n["Options"] = map[string]string{optICC: "maybe", optBridgeName: "manade0"} },
		"emptySubnet": func(n map[string]any) { n["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": ""}}} },
	} {
		t.Run(name, func(t *testing.T) {
			got := networkAnswer()
			change(got)
			_, c := networkDaemon(t, got)
			if err := c.EnsureNetwork(t.Context(), isolatedSpec()); !errors.Is(err, ErrNetworkMismatch) || !strings.Contains(err.Error(), "manade-isolated") {
				t.Errorf("EnsureNetwork = %v, want ErrNetworkMismatch naming the network", err)
			}
		})
	}
}

func TestEnsureNetworkPassesOtherErrors(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /networks/{name}", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusInternalServerError, "boom") })
	if err := d.dial(t).EnsureNetwork(t.Context(), isolatedSpec()); err == nil || errors.Is(err, ErrNetworkMismatch) {
		t.Errorf("EnsureNetwork = %v, want the daemon's error", err)
	}
	if n := len(d.seen(http.MethodPost, "/networks/create")); n != 0 {
		t.Errorf("creates after a failed read = %d, want 0", n)
	}
}
