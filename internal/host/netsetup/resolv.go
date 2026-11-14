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

// ResolvConf is the host container's resolv.conf, which Docker bind-mounts:
// it is rewritten in place, since a rename onto a mount point fails.
const ResolvConf = "/etc/resolv.conf"

// ForwarderAddr is where an internet: false host serves DNS to its own
// stack, forwarding to its configured resolvers.
var ForwarderAddr = netip.MustParseAddr("127.0.0.1")

// GitHubAPI is the name the host resolves before its daemon starts.
const GitHubAPI = "api.github.com"

// Nameservers answers the resolvers h's resolv.conf names: its configured
// ones, or on an internet: false host its own forwarder.
func Nameservers(h *config.Host) []netip.Addr {
	if h.Egress.Internet {
		return h.DNS
	}
	return []netip.Addr{ForwarderAddr}
}

// RenderResolvConf answers the resolv.conf naming servers.
func RenderResolvConf(servers []netip.Addr) []byte {
	var b bytes.Buffer
	for _, s := range servers {
		b.WriteString("nameserver " + s.String() + "\n")
	}
	return b.Bytes()
}

// ReadOriginal reads the resolvers the container's resolv.conf names before
// the host rewrites it.
func ReadOriginal(r platform.Root) (egress.Resolvers, error) {
	b, err := os.ReadFile(r.Path(ResolvConf))
	if err != nil {
		return egress.Resolvers{}, fmt.Errorf("reading %s: %w", ResolvConf, err)
	}
	return egress.ParseResolvConf(b), nil
}

// WriteResolvConf writes content over the resolv.conf in place.
func WriteResolvConf(r platform.Root, content []byte) error {
	f, err := os.OpenFile(r.Path(ResolvConf), os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("writing %s: %w", ResolvConf, err)
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", ResolvConf, err)
	}
	return nil
}

// ResolvConfHolds reports whether the resolv.conf still holds content.
func ResolvConfHolds(r platform.Root, content []byte) (bool, error) {
	b, err := os.ReadFile(r.Path(ResolvConf))
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", ResolvConf, err)
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

// CheckName resolves GitHubAPI through servers, in turn, until one answers
// with an address; it fails naming h's dns field and each resolver's error.
func CheckName(ctx context.Context, h *config.Host, servers []netip.Addr, lookup Lookup) error {
	var fails []string
	for _, s := range servers {
		addrs, err := lookup(ctx, s, GitHubAPI)
		if err == nil && len(addrs) > 0 {
			return nil
		}
		if err == nil {
			err = errors.New("no address")
		}
		fails = append(fails, s.String()+": "+err.Error())
	}
	return fmt.Errorf("netsetup: %s did not resolve through hosts.%s.dns (%s); list resolvers the host can reach on port 53",
		GitHubAPI, h.Name, strings.Join(fails, "; "))
}
