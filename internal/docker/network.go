package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
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

// NetworkSpec is one job network: a bridge with IPv6 off, one IPv4 subnet
// and a fixed bridge interface name, which the host firewall keys on.
type NetworkSpec struct {
	Subnet netip.Prefix
	Name   string
	Bridge string
	// ICC turns inter-container traffic on.
	ICC bool
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
// that it has the spec's driver, subnet, bridge name, ICC setting and no
// IPv6; a difference fails with errNetworkMismatch, because jobs on it would
// sit on a network the config does not describe. The firewall rules key on
// the bridge name and cover IPv4 only, so another name or IPv6 also puts
// jobs outside them.
func (c *Client) EnsureNetwork(ctx context.Context, spec NetworkSpec) error {
	var got networkWire
	err := c.call(ctx, "network", http.MethodGet, "/networks/"+spec.Name, nil, nil, &got, jsonLimit)
	if errors.Is(err, errNotFound) {
		return c.createNetwork(ctx, spec)
	}
	if err != nil {
		return err
	}
	return checkNetwork(&got, spec)
}

func (c *Client) createNetwork(ctx context.Context, spec NetworkSpec) error {
	body := networkWire{
		Name:   spec.Name,
		Driver: "bridge",
		Options: map[string]string{
			optICC:        strconv.FormatBool(spec.ICC),
			optBridgeName: spec.Bridge,
		},
		Labels: c.poolLabels(nil),
	}
	body.IPAM.Config = []ipamConfig{{Subnet: spec.Subnet.String()}}
	return c.call(ctx, "network", http.MethodPost, "/networks/create", nil, body, nil, 0)
}

func checkNetwork(got *networkWire, spec NetworkSpec) error {
	mismatch := func(format string, args ...any) error {
		return fmt.Errorf("%w: network %s %s", errNetworkMismatch, spec.Name, fmt.Sprintf(format, args...))
	}
	if got.Driver != "bridge" {
		return mismatch("uses driver %q, not bridge", got.Driver)
	}
	if got.EnableIPv6 {
		return mismatch("has IPv6 on")
	}
	if len(got.IPAM.Config) != 1 {
		return mismatch("has %d subnets, not 1", len(got.IPAM.Config))
	}
	if sn, err := netip.ParsePrefix(got.IPAM.Config[0].Subnet); err != nil || sn.Masked() != spec.Subnet.Masked() {
		return mismatch("has subnet %q, not %s", got.IPAM.Config[0].Subnet, spec.Subnet)
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
	if icc != spec.ICC {
		return mismatch("has %s %t, not %t", optICC, icc, spec.ICC)
	}
	return nil
}
