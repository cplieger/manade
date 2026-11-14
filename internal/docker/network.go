package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
)

// The bridge driver's options a job network sets.
const (
	optICC        = "com.docker.network.bridge.enable_icc"
	optBridgeName = "com.docker.network.bridge.name"
)

// errNetworkMismatch is returned by EnsureNetwork for an existing network
// whose shape differs from the spec.
var errNetworkMismatch = errors.New("docker: the network exists with a different shape")

// NetworkSpec is the job network: a bridge with inter-container traffic off,
// one IPv4 subnet, an IPv6 /64 when Subnet6 is set, and a bridge interface
// named Bridge, which the egress rules match.
type NetworkSpec struct {
	Subnet  netip.Prefix
	Subnet6 netip.Prefix
	Name    string
	Bridge  string
}

type networkWire struct {
	Options map[string]string
	Labels  map[string]string `json:",omitempty"`
	Name    string
	Driver  string
	IPAM    struct {
		Config []ipamConfig
	}
	EnableIPv6 bool
}

type ipamConfig struct{ Subnet string }

// EnsureNetwork creates the network when it is missing, and otherwise checks
// that it has the spec's driver, subnets, bridge name and inter-container
// traffic off. A network of another shape that carries LabelManaged is
// removed and created again, so a host whose job subnet moved starts on the
// new one; the daemon refuses the removal while a container is attached. One
// without the label fails with errNetworkMismatch, since a job on a bridge of
// another name, or with IPv6 the rules do not expect, would pass every rule
// that matches the bridge.
func (c *Client) EnsureNetwork(ctx context.Context, spec *NetworkSpec) error {
	var got networkWire
	err := c.call(ctx, "network", http.MethodGet, "/networks/"+url.PathEscape(spec.Name), nil, nil, &got, jsonLimit)
	if errors.Is(err, errNotFound) {
		return c.createNetwork(ctx, spec)
	}
	if err != nil {
		return err
	}
	mismatch := checkNetwork(&got, spec)
	if mismatch == nil || got.Labels[LabelManaged] != managedValue {
		return mismatch
	}
	if err := c.call(ctx, "network", http.MethodDelete, "/networks/"+url.PathEscape(spec.Name), nil, nil, nil, 0); err != nil && !errors.Is(err, errNotFound) {
		return fmt.Errorf("%w; removing it: %w", mismatch, err)
	}
	return c.createNetwork(ctx, spec)
}

func (c *Client) createNetwork(ctx context.Context, spec *NetworkSpec) error {
	body := networkWire{
		Name:       spec.Name,
		Driver:     "bridge",
		Options:    map[string]string{optICC: "false", optBridgeName: spec.Bridge},
		Labels:     c.managedLabels(nil),
		EnableIPv6: spec.Subnet6.IsValid(),
	}
	body.IPAM.Config = []ipamConfig{{Subnet: spec.Subnet.String()}}
	if spec.Subnet6.IsValid() {
		body.IPAM.Config = append(body.IPAM.Config, ipamConfig{Subnet: spec.Subnet6.String()})
	}
	return c.call(ctx, "network", http.MethodPost, "/networks/create", nil, body, nil, 0)
}

func checkNetwork(got *networkWire, spec *NetworkSpec) error {
	mismatch := func(format string, args ...any) error {
		return fmt.Errorf("%w: network %s %s", errNetworkMismatch, spec.Name, fmt.Sprintf(format, args...))
	}
	if got.Driver != "bridge" {
		return mismatch("uses driver %q, not bridge", got.Driver)
	}
	if got.EnableIPv6 != spec.Subnet6.IsValid() {
		return mismatch("has IPv6 %t, not %t", got.EnableIPv6, spec.Subnet6.IsValid())
	}
	want := []netip.Prefix{spec.Subnet.Masked()}
	if spec.Subnet6.IsValid() {
		want = append(want, spec.Subnet6.Masked())
	}
	var have []netip.Prefix
	for _, cfg := range got.IPAM.Config {
		sn, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil {
			return mismatch("has subnet %q", cfg.Subnet)
		}
		have = append(have, sn.Masked())
	}
	slices.SortFunc(have, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	slices.SortFunc(want, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	if !slices.Equal(have, want) {
		return mismatch("has subnets %v, not %v", have, want)
	}
	if b := got.Options[optBridgeName]; b != spec.Bridge {
		return mismatch("has bridge interface %q, not %s", b, spec.Bridge)
	}
	// The bridge driver reads the option with strconv.ParseBool and
	// defaults to on.
	icc := true
	if v, ok := got.Options[optICC]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return mismatch("has %s %q", optICC, v)
		}
		icc = b
	}
	if icc {
		return mismatch("has inter-container traffic on")
	}
	return nil
}
