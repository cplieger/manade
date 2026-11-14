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

func TestNameservers(t *testing.T) {
	h := section()
	if got := Nameservers(h); !slices.Equal(got, h.DNS) {
		t.Errorf("internet host: %v, want its dns", got)
	}
	h.Egress.Internet = false
	if got := Nameservers(h); !slices.Equal(got, []netip.Addr{addr("127.0.0.1")}) {
		t.Errorf("internet: false host: %v, want its own forwarder 127.0.0.1", got)
	}
}

func TestResolvConfWrittenInPlace(t *testing.T) {
	r := tree(t, map[string]string{ResolvConf: "# ExtServers: [host(192.168.1.1)]\nnameserver 127.0.0.11\noptions ndots:0\n"})
	before, err := os.Stat(r.Path(ResolvConf))
	if err != nil {
		t.Fatal(err)
	}
	orig, err := ReadOriginal(r)
	if err != nil || !slices.Equal(orig.Addrs, []netip.Addr{addr("192.168.1.1")}) {
		t.Fatalf("ReadOriginal = %+v, %v, want the ExtServers upstream", orig, err)
	}
	content := RenderResolvConf(Nameservers(section()))
	if string(content) != "nameserver 1.1.1.1\nnameserver 8.8.8.8\n" {
		t.Errorf("RenderResolvConf = %q", content)
	}
	if err := WriteResolvConf(r, content); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(r.Path(ResolvConf))
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
	servers := []netip.Addr{addr("1.1.1.1"), addr("8.8.8.8")}
	t.Run("the second resolver answers", func(t *testing.T) {
		var asked []string
		lookup := func(_ context.Context, s netip.Addr, name string) ([]netip.Addr, error) {
			asked = append(asked, s.String()+" "+name)
			if s == servers[0] {
				return nil, errors.New("i/o timeout")
			}
			return []netip.Addr{addr("140.82.112.6")}, nil
		}
		if err := CheckName(t.Context(), section(), servers, lookup); err != nil {
			t.Errorf("CheckName = %v", err)
		}
		if !slices.Equal(asked, []string{"1.1.1.1 api.github.com", "8.8.8.8 api.github.com"}) {
			t.Errorf("asked %q", asked)
		}
	})
	t.Run("none answers", func(t *testing.T) {
		lookup := func(_ context.Context, s netip.Addr, _ string) ([]netip.Addr, error) {
			if s == servers[0] {
				return nil, errors.New("i/o timeout")
			}
			return nil, nil
		}
		err := CheckName(t.Context(), section(), servers, lookup)
		for _, want := range []string{"hosts.lan.dns", "1.1.1.1: i/o timeout", "8.8.8.8: no address"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("CheckName = %v, want it to name %q", err, want)
			}
		}
	})
}

func TestCheckIPv6(t *testing.T) {
	global := []netip.Addr{addr("fd00:c8::2"), addr("2a01:e0a:1259:2710::b")}
	t.Run("every prerequisite", func(t *testing.T) {
		own, err := CheckIPv6(global, Routes{Default6: true}, true)
		if err != nil || own != pfx("2a01:e0a:1259:2710::/64") {
			t.Errorf("CheckIPv6 = %v, %v, want the global address's /64", own, err)
		}
	})
	t.Run("a ULA address alone gives no own /64", func(t *testing.T) {
		own, err := CheckIPv6(global[:1], Routes{Default6: true}, true)
		if err != nil || own.IsValid() {
			t.Errorf("CheckIPv6 = %v, %v, want no own /64", own, err)
		}
	})
	t.Run("the floor kernel names all three", func(t *testing.T) {
		_, err := CheckIPv6(nil, Routes{}, false)
		for _, want := range []string{"no IPv6 address", "no IPv6 default route", "no IPv6 nat table"} {
			if !errors.Is(err, ErrNoIPv6) || !strings.Contains(err.Error(), want) {
				t.Errorf("CheckIPv6 = %v, want ErrNoIPv6 naming %q", err, want)
			}
		}
	})
}
