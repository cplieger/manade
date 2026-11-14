package netsetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/platform"
)

// resolvConf is the host container's resolv.conf, which Docker bind-mounts:
// it is rewritten in place, since a rename onto a mount point fails.
const resolvConf = "/etc/resolv.conf"

// ForwarderAddr is where the host serves DNS to its own stack, forwarding
// to its configured resolvers.
var ForwarderAddr = netip.MustParseAddr("127.0.0.1")

// ResolvConf answers the resolv.conf the host writes at its start and keeps
// for its life: its own forwarder alone. A running job's embedded resolver
// keeps the upstreams its container started with, so a reload that changed
// this file would leave running jobs on the old ones.
func ResolvConf() []byte { return []byte("nameserver " + ForwarderAddr.String() + "\n") }

// ReadOriginal reads the resolvers the container's resolv.conf names before
// the host rewrites it.
func ReadOriginal(r platform.Root) (egress.Resolvers, error) {
	b, err := os.ReadFile(r.Path(resolvConf))
	if err != nil {
		return egress.Resolvers{}, fmt.Errorf("reading %s: %w", resolvConf, err)
	}
	return egress.ParseResolvConf(b), nil
}

// WriteResolvConf writes content over the resolv.conf in place.
func WriteResolvConf(r platform.Root, content []byte) error {
	f, err := os.OpenFile(r.Path(resolvConf), os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("writing %s: %w", resolvConf, err)
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", resolvConf, err)
	}
	return nil
}

// ResolvConfHolds reports whether the resolv.conf still holds content.
func ResolvConfHolds(r platform.Root, content []byte) (bool, error) {
	b, err := os.ReadFile(r.Path(resolvConf))
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", resolvConf, err)
	}
	return bytes.Equal(b, content), nil
}

// Lookup resolves name through the one resolver server.
type Lookup func(ctx context.Context, server netip.Addr, name string) ([]netip.Addr, error)

// LookupVia is the Lookup that asks server on port 53, bounded by
// egress.LookupTimeout.
func LookupVia(ctx context.Context, server netip.Addr, name string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, egress.LookupTimeout)
	defer cancel()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, netip.AddrPortFrom(server, 53).String())
	}}
	return r.LookupNetIP(ctx, "ip", name)
}

// CheckName resolves egress.GitHubAPI through the host's forwarder, which
// must be serving; it fails naming h's dns field, the resolvers it lists
// and the lookup's error.
func CheckName(ctx context.Context, h *config.Host, lookup Lookup) error {
	addrs, err := lookup(ctx, ForwarderAddr, egress.GitHubAPI)
	if err == nil && len(addrs) > 0 {
		return nil
	}
	if err == nil {
		err = errors.New("no address")
	}
	dns := make([]string, len(h.DNS))
	for i, a := range h.DNS {
		dns[i] = a.String()
	}
	return fmt.Errorf("netsetup: %s did not resolve through the host's resolver, which forwards to hosts.%s.dns (%s): %w; list resolvers the host can reach on port 53",
		egress.GitHubAPI, h.Name, strings.Join(dns, ", "), err)
}
