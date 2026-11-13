package egress

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// written reports whether a word of data, split at spaces, brackets and
// the ExtServers marker, is a, its zone left out.
func written(data []byte, a netip.Addr) bool {
	text := strings.ReplaceAll(string(data), "# ExtServers:", " ")
	words := strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(" \t\r\n\v\f()[]", r) })
	return slices.ContainsFunc(words, func(w string) bool {
		w, _, _ = strings.Cut(w, "%")
		b, err := netip.ParseAddr(w)
		return err == nil && b == a
	})
}

func FuzzParseResolvConf_addrsFromTheFile(f *testing.F) {
	f.Add([]byte("nameserver 127.0.0.11\noptions ndots:0\n"))
	f.Add([]byte("nameserver 127.0.0.11\n# ExtServers: [host(192.168.1.87) 8.8.8.8]\n"))
	f.Add([]byte("nameserver fd00::53\nnameserver 10.0.0.53\nnameserver 10.0.0.53\n"))
	f.Add([]byte("# ExtServers: [host(::1) host( 1.1.1.1 ) [x]]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		got := ParseResolvConf(data)
		if got.Unknown != (len(got.Addrs) == 0) {
			t.Fatalf("Unknown = %v with %d addresses", got.Unknown, len(got.Addrs))
		}
		for i, a := range got.Addrs {
			if a == dockerResolver {
				t.Fatalf("Docker's embedded resolver %s is listed", a)
			}
			if slices.Contains(got.Addrs[:i], a) {
				t.Fatalf("%s is listed twice", a)
			}
			if !written(data, a) {
				t.Fatalf("%s is not written in the file", a)
			}
		}
	})
}
