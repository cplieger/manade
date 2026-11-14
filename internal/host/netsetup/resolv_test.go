package netsetup

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestResolvConfWrittenInPlace(t *testing.T) {
	r := tree(t, map[string]string{resolvConf: "# ExtServers: [host(192.168.1.1)]\nnameserver 127.0.0.11\noptions ndots:0\n"})
	before, err := os.Stat(r.Path(resolvConf))
	if err != nil {
		t.Fatal(err)
	}
	orig, err := ReadOriginal(r)
	if err != nil || !slices.Equal(orig.Addrs, []netip.Addr{addr("192.168.1.1")}) {
		t.Fatalf("ReadOriginal = %+v, %v, want the ExtServers upstream", orig, err)
	}
	content := ResolvConf()
	if string(content) != "nameserver 127.0.0.1\n" {
		t.Errorf("ResolvConf = %q, want the host's own forwarder alone", content)
	}
	if err := WriteResolvConf(r, content); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(r.Path(resolvConf))
	if err != nil {
		t.Fatal(err)
	}
	if before.Sys().(*syscall.Stat_t).Ino != after.Sys().(*syscall.Stat_t).Ino {
		t.Error("the file was replaced, not rewritten: a bind-mounted resolv.conf refuses a rename")
	}
	if holds, err := ResolvConfHolds(r, content); !holds || err != nil {
		t.Errorf("ResolvConfHolds = %t, %v after the write", holds, err)
	}
	if holds, _ := ResolvConfHolds(r, []byte("nameserver 9.9.9.9\n")); holds {
		t.Error("ResolvConfHolds matched other content")
	}
}

func TestCheckName(t *testing.T) {
	t.Run("asks the host's forwarder", func(t *testing.T) {
		var asked []string
		lookup := func(_ context.Context, s netip.Addr, name string) ([]netip.Addr, error) {
			asked = append(asked, s.String()+" "+name)
			return []netip.Addr{addr("140.82.112.6")}, nil
		}
		if err := CheckName(t.Context(), section(), lookup); err != nil {
			t.Errorf("CheckName = %v", err)
		}
		if !slices.Equal(asked, []string{"127.0.0.1 api.github.com"}) {
			t.Errorf("asked %q", asked)
		}
	})
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"a failed lookup": {err: errors.New("i/o timeout"), want: "i/o timeout"},
		"no address":      {want: "no address"},
	} {
		t.Run(name, func(t *testing.T) {
			lookup := func(context.Context, netip.Addr, string) ([]netip.Addr, error) { return nil, tc.err }
			err := CheckName(t.Context(), section(), lookup)
			for _, want := range []string{"hosts.lan.dns", "1.1.1.1, 8.8.8.8", tc.want} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("CheckName = %v, want it to name %q", err, want)
				}
			}
		})
	}
}

func TestCheckIPv6(t *testing.T) {
	global := []netip.Addr{addr("fd00:c8::2"), addr("2a01:e0a:1259:2710::b")}
	t.Run("every prerequisite", func(t *testing.T) {
		own, err := CheckIPv6(global, Routes{Gateway6: addr("fd00:c8::1")}, true)
		if err != nil || own != pfx("2a01:e0a:1259:2710::/64") {
			t.Errorf("CheckIPv6 = %v, %v, want the global address's /64", own, err)
		}
	})
	t.Run("a ULA address alone gives no own /64", func(t *testing.T) {
		own, err := CheckIPv6(global[:1], Routes{Gateway6: addr("fd00:c8::1")}, true)
		if err != nil || own.IsValid() {
			t.Errorf("CheckIPv6 = %v, %v, want no own /64", own, err)
		}
	})
	t.Run("the floor kernel names all three", func(t *testing.T) {
		_, err := CheckIPv6(nil, Routes{}, false)
		for _, want := range []string{"no IPv6 address", "no IPv6 default route", "no IPv6 nat table"} {
			if !errors.Is(err, errNoIPv6) || !strings.Contains(err.Error(), want) {
				t.Errorf("CheckIPv6 = %v, want ErrNoIPv6 naming %q", err, want)
			}
		}
	})
}
