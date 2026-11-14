// Package netsetup prepares a runner host's own network namespace before its
// daemon starts: the IPv6 sysctls, the routes and addresses it reads, the job
// subnets it picks, its resolv.conf and the name check through its resolvers.
// Every reader takes a platform.Root, System in production and a fixture tree
// in tests.
package netsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/cplieger/manade/internal/host/platform"
)

// The IPv6 sysctls of the host's own namespace.
const (
	ipv6Dir            = "/proc/sys/net/ipv6"
	disableIPv6All     = ipv6Dir + "/conf/all/disable_ipv6"
	disableIPv6Default = ipv6Dir + "/conf/default/disable_ipv6"
	forwardingIPv6     = ipv6Dir + "/conf/all/forwarding"
)

// read answers the sysctl at p, trimmed, and whether it exists.
func read(r platform.Root, p string) (v string, ok bool, err error) {
	b, err := os.ReadFile(r.Path(p))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(b)), true, nil
}

// write sets the sysctl at p, which must exist: /proc/sys refuses a new file.
func write(r platform.Root, p, v string) error {
	f, err := os.OpenFile(r.Path(p), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("setting %s: %w", p, err)
	}
	_, err = f.WriteString(v + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("setting %s: %w", p, err)
	}
	return nil
}

// TurnIPv6Off turns IPv6 off in the namespace, for its interfaces and for
// any it creates later, the job bridge among them. A kernel without IPv6 has
// it off already.
func TurnIPv6Off(r platform.Root) error {
	if _, ok, err := read(r, disableIPv6All); err != nil || !ok {
		return err
	}
	for _, p := range []string{disableIPv6All, disableIPv6Default} {
		if err := write(r, p, "1"); err != nil {
			return err
		}
	}
	return nil
}

// IPv6Off reports whether IPv6 is off in the namespace as TurnIPv6Off leaves
// it.
func IPv6Off(r platform.Root) (bool, error) {
	for _, p := range []string{disableIPv6All, disableIPv6Default} {
		v, ok, err := read(r, p)
		if err != nil {
			return false, err
		}
		if ok && v != "1" {
			return false, nil
		}
	}
	return true, nil
}

// EnableIPv6Forwarding turns IPv6 forwarding on, which the job network's
// masquerade needs and a daemon run with --ip6tables=false does not set.
func EnableIPv6Forwarding(r platform.Root) error { return write(r, forwardingIPv6, "1") }

// SetBridgeNF6 sets bridge-nf-call-ip6tables to 1, so bridged IPv6 between
// jobs crosses the forward chain; the caller sets it only where the
// namespace has it (platform.BridgeNF).
func SetBridgeNF6(r platform.Root) error { return write(r, platform.BridgeNFCall6, "1") }

// BridgeNF6On reports whether bridge-nf-call-ip6tables reads 1.
func BridgeNF6On(r platform.Root) (bool, error) {
	v, ok, err := read(r, platform.BridgeNFCall6)
	return ok && v == "1", err
}
