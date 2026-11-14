package netsetup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"strings"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/manade/internal/config"
)

// dockerPools are Docker's default local address pools in the order its
// allocator takes them
// (https://github.com/moby/moby/blob/docker-v29.0.0/daemon/libnetwork/ipamutils/utils.go).
var dockerPools = func() []netip.Prefix {
	var out []netip.Prefix
	for i := 17; i <= 31; i++ {
		out = append(out, netip.PrefixFrom(netip.AddrFrom4([4]byte{172, byte(i), 0, 0}), 16))
	}
	for i := 0; i < 256; i += 16 {
		out = append(out, netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(i), 0}), 20))
	}
	return out
}()

// ErrNoSubnet is a job subnet that cannot be picked or a configured one
// that clashes with the namespace's routes.
var ErrNoSubnet = errors.New("netsetup: job subnet")

// Conflicts answers what of h's section the job subnet s takes from the
// host: a resolver inside it, whose lookups the bridge would capture, and an
// allow entry it overlaps, which jobs could no longer reach. Each names its
// field.
func Conflicts(s netip.Prefix, h *config.Host) []string {
	at := "hosts." + h.Name
	var out []string
	for i, r := range h.DNS {
		if s.Contains(r) {
			out = append(out, fmt.Sprintf("%s contains the resolver %s of %s.dns[%d]", s, r, at, i))
		}
	}
	for i, a := range h.Egress.Allow {
		if s.Overlaps(a.To) {
			out = append(out, fmt.Sprintf("%s overlaps %s of %s.egress.allow[%d]", s, a.To, at, i))
		}
	}
	return out
}

// PickSubnet answers the first of Docker's default pools that overlaps no
// on-link route and no resolver or allow entry of h. The daemon's own pick
// reserves only its resolv.conf's nameservers and the on-link routes, and
// an allow entry is never among them.
func PickSubnet(h *config.Host, onLink []netip.Prefix) (netip.Prefix, error) {
	for _, p := range dockerPools {
		if len(Conflicts(p, h)) == 0 && overlapping(p, onLink) == "" {
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("%w: every pool of 172.17.0.0/16 to 172.31.0.0/16 and 192.168.0.0/16 in /20s overlaps an on-link route, a resolver or an allow entry; set hosts.%s.network.subnet",
		ErrNoSubnet, h.Name)
}

// CheckSubnet refuses a configured job subnet that overlaps an on-link route
// of the namespace, which the bridge would shadow.
func CheckSubnet(h *config.Host, onLink []netip.Prefix) error {
	if r := overlapping(h.Subnet, onLink); r != "" {
		return fmt.Errorf("%w: hosts.%s.network.subnet %s overlaps the on-link route %s of the host's network; pick another subnet",
			ErrNoSubnet, h.Name, h.Subnet, r)
	}
	return nil
}

func overlapping(p netip.Prefix, routes []netip.Prefix) string {
	for _, r := range routes {
		if r.Addr().Is4() == p.Addr().Is4() && r.Overlaps(p) {
			return r.String()
		}
	}
	return ""
}

// job6Limit caps the saved job prefix's file.
const job6Limit = 256

// JobPrefix6 answers the job network's IPv6 /64: the one saved at path, so
// the network keeps its prefix across restarts, unless it overlaps an
// on-link route; otherwise a new random ULA /64 (RFC 4193), saved
// atomically. random is crypto/rand's Reader in production.
func JobPrefix6(ctx context.Context, path string, onLink []netip.Prefix, random io.Reader) (netip.Prefix, error) {
	b, err := filesafe.ReadBounded(ctx, path, job6Limit)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return netip.Prefix{}, fmt.Errorf("the saved job IPv6 prefix: %w", err)
	}
	if p, perr := netip.ParsePrefix(strings.TrimSpace(string(b))); err == nil && perr == nil && isULA64(p) && overlapping(p, onLink) == "" {
		return p, nil
	}
	if random == nil {
		random = rand.Reader
	}
	for range 8 {
		var a [16]byte
		a[0] = 0xfd
		if _, err := io.ReadFull(random, a[1:6]); err != nil {
			return netip.Prefix{}, fmt.Errorf("drawing a job IPv6 prefix: %w", err)
		}
		p := netip.PrefixFrom(netip.AddrFrom16(a), 64)
		if overlapping(p, onLink) != "" {
			continue
		}
		if _, err := filesafe.WriteFile(ctx, path, []byte(p.String()+"\n"), filesafe.WithMode(0o600), filesafe.WithMkdirAll(true)); err != nil {
			return netip.Prefix{}, fmt.Errorf("saving the job IPv6 prefix: %w", err)
		}
		return p, nil
	}
	return netip.Prefix{}, fmt.Errorf("%w: 8 random ULA /64s each overlapped an on-link route", ErrNoSubnet)
}

func isULA64(p netip.Prefix) bool {
	a := p.Addr()
	return a.Is6() && !a.Is4In6() && a.As16()[0] == 0xfd && p.Bits() == 64 && p == p.Masked()
}
