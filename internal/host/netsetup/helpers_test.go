package netsetup

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/platform"
)

// tree writes files, each path absolute under the answered Root.
func tree(t *testing.T, files map[string]string) platform.Root {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return platform.Root(dir)
}

func readFile(t *testing.T, r platform.Root, p string) string {
	t.Helper()
	b, err := os.ReadFile(r.Path(p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// section is a host section named "lan" with the default resolvers.
func section(allow ...string) *config.Host {
	h := &config.Host{Name: "lan", DNS: config.DefaultDNS, Egress: config.Egress{Internet: true}}
	for _, a := range allow {
		h.Egress.Allow = append(h.Egress.Allow, config.Allow{To: pfx(a), Protocol: config.ProtocolTCP})
	}
	return h
}
