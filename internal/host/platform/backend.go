package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cplieger/manade/internal/egress"
)

// LegacyDir holds the image's legacy iptables binaries under their plain
// names. The image's default iptables is the nf_tables variant, which fails
// on a kernel without nf_tables, so on that backend this directory comes
// first in the supervisor's and dockerd's PATH.
const LegacyDir = "/usr/local/sbin/.iptables-legacy"

// ErrNoBackend is a kernel with neither nf_tables nor ip_tables.
var ErrNoBackend = errors.New("platform: the kernel offers no rule backend")

// Command runs one program to completion with stdin, which may be nil, and
// answers its standard output; its error carries the tail of the program's
// standard error.
type Command func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// Exec is the Command that runs the real program.
func Exec(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tail(errOut.String()))
	}
	return out.Bytes(), nil
}

// tail answers the last 512 bytes of s, trimmed.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		s = s[len(s)-512:]
	}
	return s
}

// Kernel is the rule backend the kernel offers and the tables it lacks.
type Kernel struct {
	Backend egress.Backend
	// RawOff is a kernel without the iptables raw table, so dockerd runs
	// with Docker's raw rules off.
	RawOff bool
	// IPv6NAT is the kernel's IPv6 nat table, which egress.ipv6 needs.
	IPv6NAT bool
}

// Binary answers the path of an iptables-family program on k's backend.
func (k *Kernel) Binary(name string) string {
	if k.Backend == egress.IPTables {
		return filepath.Join(LegacyDir, name)
	}
	return name
}

// ProbeKernel picks the backend, nftables where `nft list tables` succeeds,
// else iptables where the legacy binaries list the filter table, and probes
// the raw and IPv6 nat tables through that backend's binaries.
func ProbeKernel(ctx context.Context, run Command) (*Kernel, error) {
	k := &Kernel{}
	ok := func(name string, args ...string) error {
		_, err := run(ctx, nil, name, args...)
		return err
	}
	nftErr := ok("nft", "list", "tables")
	if nftErr == nil {
		k.Backend = egress.NFTables
	} else {
		legacy := &Kernel{Backend: egress.IPTables}
		if err := ok(legacy.Binary("iptables"), "-w", "-t", "filter", "-n", "-L"); err != nil {
			return nil, fmt.Errorf("%w: nftables: %w; legacy iptables: %w", ErrNoBackend, nftErr, err)
		}
		k.Backend = egress.IPTables
	}
	k.RawOff = ok(k.Binary("iptables"), "-w", "-t", "raw", "-n", "-L") != nil
	k.IPv6NAT = ok(k.Binary("ip6tables"), "-w", "-t", "nat", "-n", "-L") == nil
	return k, nil
}

// The bridge netfilter sysctls. Before Linux 5.3 they exist only in the
// machine's initial network namespace.
const (
	BridgeNFCall4 = "/proc/sys/net/bridge/bridge-nf-call-iptables"
	BridgeNFCall6 = "/proc/sys/net/bridge/bridge-nf-call-ip6tables"
)

// BridgeNF reports which bridge netfilter sysctls the host's own network
// namespace has.
func BridgeNF(r Root) (v4, v6 bool) { return r.exists(BridgeNFCall4), r.exists(BridgeNFCall6) }
