package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/egress"
)

// fakeKernel answers each command line it is given in ok with success and
// every other with an error, and records the lines.
type fakeKernel struct {
	ok  map[string]bool
	ran []string
}

func (f *fakeKernel) run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	f.ran = append(f.ran, line)
	if f.ok[line] {
		return nil, nil
	}
	return nil, errors.New("refused: " + line)
}

const (
	nftList   = "nft list tables"
	legacy    = LegacyDir + "/iptables -w -t filter -n -L"
	legacyRaw = LegacyDir + "/iptables -w -t raw -n -L"
	legacy6   = LegacyDir + "/ip6tables -w -t nat -n -L"
	nftRaw    = "iptables -w -t raw -n -L"
	nft6      = "ip6tables -w -t nat -n -L"
)

func TestProbeKernel(t *testing.T) {
	cases := map[string]struct {
		ok   []string
		want Kernel
	}{
		"nftables":     {ok: []string{nftList, nftRaw, nft6}, want: Kernel{Backend: egress.NFTables, IPv6NAT: true}},
		"legacyFloor":  {ok: []string{legacy}, want: Kernel{Backend: egress.IPTables, RawOff: true}},
		"legacyAllOn":  {ok: []string{legacy, legacyRaw, legacy6}, want: Kernel{Backend: egress.IPTables, IPv6NAT: true}},
		"nftNoIPv6NAT": {ok: []string{nftList, nftRaw}, want: Kernel{Backend: egress.NFTables}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeKernel{ok: map[string]bool{}}
			for _, l := range tc.ok {
				f.ok[l] = true
			}
			k, err := ProbeKernel(t.Context(), f.run)
			if err != nil {
				t.Fatal(err)
			}
			if *k != tc.want {
				t.Errorf("kernel = %+v, want %+v (ran %q)", *k, tc.want, f.ran)
			}
		})
	}
}

func TestProbeKernelRefusesNeither(t *testing.T) {
	f := &fakeKernel{ok: map[string]bool{}}
	_, err := ProbeKernel(t.Context(), f.run)
	if !errors.Is(err, errNoBackend) || !strings.Contains(err.Error(), "nft list tables") || !strings.Contains(err.Error(), LegacyDir) {
		t.Errorf("err = %v, want ErrNoBackend naming both attempts", err)
	}
}

func TestKernelBinary(t *testing.T) {
	if got := (&Kernel{Backend: egress.IPTables}).Binary("ip6tables"); got != LegacyDir+"/ip6tables" {
		t.Errorf("iptables backend's ip6tables = %q", got)
	}
	if got := (&Kernel{Backend: egress.NFTables}).Binary("iptables"); got != "iptables" {
		t.Errorf("nftables backend's iptables = %q", got)
	}
}

func TestExec(t *testing.T) {
	t.Run("a failure names its standard error", func(t *testing.T) {
		_, err := Exec(t.Context(), nil, "sh", "-c", "echo boom >&2; exit 3")
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v, want the program's standard error", err)
		}
	})
	t.Run("stdin in, standard output out", func(t *testing.T) {
		out, err := Exec(t.Context(), []byte("payload"), "sh", "-c", "cat; echo noise >&2")
		if err != nil || string(out) != "payload" {
			t.Errorf("Exec = %q, %v, want the stdin echoed and no stderr", out, err)
		}
	})
}

func TestBridgeNF(t *testing.T) {
	v4, v6 := BridgeNF(tree(t, map[string]string{bridgeNFCall4: "1\n"}))
	if !v4 || v6 {
		t.Errorf("BridgeNF = %t, %t, want true, false", v4, v6)
	}
}
