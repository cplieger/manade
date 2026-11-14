package netsetup

import (
	"testing"

	"github.com/cplieger/manade/internal/host/platform"
)

func TestTurnIPv6Off(t *testing.T) {
	t.Run("both switches set and read back off", func(t *testing.T) {
		r := tree(t, map[string]string{disableIPv6All: "0\n", disableIPv6Default: "0\n"})
		if off, _ := IPv6Off(r); off {
			t.Fatal("IPv6Off before = true, want false")
		}
		if err := TurnIPv6Off(r); err != nil {
			t.Fatalf("TurnIPv6Off: %v", err)
		}
		if a, d := readFile(t, r, disableIPv6All), readFile(t, r, disableIPv6Default); a != "1\n" || d != "1\n" {
			t.Errorf("all = %q, default = %q, want 1 and 1", a, d)
		}
		if off, err := IPv6Off(r); !off || err != nil {
			t.Errorf("IPv6Off = %t, %v, want true", off, err)
		}
	})
	t.Run("default alone back on reads as on", func(t *testing.T) {
		r := tree(t, map[string]string{disableIPv6All: "1\n", disableIPv6Default: "0\n"})
		if off, _ := IPv6Off(r); off {
			t.Error("IPv6Off = true with default at 0")
		}
	})
	t.Run("a kernel without IPv6 has it off", func(t *testing.T) {
		r := tree(t, map[string]string{"/proc/sys/net/core/somaxconn": "4096\n"})
		if err := TurnIPv6Off(r); err != nil {
			t.Errorf("TurnIPv6Off = %v, want nil", err)
		}
		if off, err := IPv6Off(r); !off || err != nil {
			t.Errorf("IPv6Off = %t, %v, want true", off, err)
		}
	})
}

func TestBridgeNF6(t *testing.T) {
	t.Run("set and read", func(t *testing.T) {
		r := tree(t, map[string]string{platform.BridgeNFCall6: "0\n"})
		if on, _ := BridgeNF6On(r); on {
			t.Fatal("BridgeNF6On = true at 0")
		}
		if err := SetBridgeNF6(r); err != nil {
			t.Fatalf("SetBridgeNF6: %v", err)
		}
		if on, err := BridgeNF6On(r); !on || err != nil {
			t.Errorf("BridgeNF6On = %t, %v, want true", on, err)
		}
	})
	t.Run("absent is never created", func(t *testing.T) {
		r := tree(t, map[string]string{"/proc/sys/net/core/somaxconn": "4096\n"})
		if err := SetBridgeNF6(r); err == nil {
			t.Error("SetBridgeNF6 created a sysctl the namespace lacks")
		}
	})
}

func TestEnableIPv6Forwarding(t *testing.T) {
	r := tree(t, map[string]string{forwardingIPv6: "0\n"})
	if err := EnableIPv6Forwarding(r); err != nil || readFile(t, r, forwardingIPv6) != "1\n" {
		t.Errorf("EnableIPv6Forwarding = %v, forwarding %q", err, readFile(t, r, forwardingIPv6))
	}
}
