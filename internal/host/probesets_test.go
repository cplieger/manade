package host

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestProbeSets(t *testing.T) {
	k, _, g, _ := guardFixture(t)
	p := &probeSets{rules: g.rules, guard: func() *guard { return g }}
	src := netip.MustParseAddr("172.18.0.2")
	if err := p.SetSources(t.Context(), []netip.Addr{src}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTarget(t.Context(), netip.MustParseAddr("192.168.1.1")); err != nil {
		t.Fatal(err)
	}
	if err := p.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Run("each change goes through ipset restore", func(t *testing.T) {
		k.mu.Lock()
		files := slices.Clone(k.ipsetFiles)
		k.mu.Unlock()
		last := files[len(files)-3:]
		if !strings.Contains(last[0], "add manade-probe4 172.18.0.2") || !strings.Contains(last[1], "add manade-target4 192.168.1.1") ||
			last[2] != "flush manade-probe4\nflush manade-target4\n" {
			t.Errorf("payloads = %q", last)
		}
	})
	t.Run("the counters are read with each rule's counts", func(t *testing.T) {
		if _, err := p.Counts(t.Context()); err != nil {
			t.Fatal(err)
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		if !slices.ContainsFunc(k.ran, func(l string) bool { return strings.HasSuffix(l, "iptables-save -c") }) {
			t.Errorf("ran %q, want iptables-save -c", k.ran)
		}
	})
}
