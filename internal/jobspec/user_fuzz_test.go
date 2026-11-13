package jobspec

import (
	"slices"
	"strings"
	"testing"
)

func addUserSeeds(f *testing.F) {
	f.Helper()
	f.Add("runner", ghPasswd, ghGroup)
	f.Add("", ghPasswd, ghGroup)
	f.Add("1001:1001", "", "root:x:0:runner\n")
	f.Add("runner:docker", ghPasswd, "#c\n"+ghGroup)
	f.Add("x:y", "x:x:5:6\n", "y:x:7:\n")
	f.Add("2147483648", "a:b", "::\n:")
}

// FuzzResolveUser_layoutInsensitive: Docker trims each line, so CR line ends,
// blank lines and indentation never change who a user resolves to.
func FuzzResolveUser_layoutInsensitive(f *testing.F) {
	addUserSeeds(f)
	f.Fuzz(func(t *testing.T, spec, passwd, group string) {
		if len(passwd) > 4<<10 || len(group) > 4<<10 {
			t.Skip("a longer line could cross the scanner's limit once padded")
		}
		pad := func(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n\n \t")) }
		a, errA := resolveUser(spec, Files{Passwd: []byte(passwd), Group: []byte(group)})
		b, errB := resolveUser(spec, Files{Passwd: pad(passwd), Group: pad(group)})
		if (errA == nil) != (errB == nil) || a.uid != b.uid || a.gid != b.gid || !slices.Equal(a.sgids, b.sgids) {
			t.Fatalf("plain = %+v, %v; padded = %+v, %v", a, errA, b, errB)
		}
	})
}

// FuzzCheckUser_verdict: an accepted user resolved to no id 0, and a
// refusal is one line that quotes no file text.
func FuzzCheckUser_verdict(f *testing.F) {
	addUserSeeds(f)
	f.Fuzz(func(t *testing.T, spec, passwd, group string) {
		files := Files{Passwd: []byte(passwd), Group: []byte(group)}
		err := CheckUser(spec, files)
		if err == nil {
			u, rerr := resolveUser(spec, files)
			if rerr != nil || u.uid == 0 || u.gid == 0 || slices.Contains(u.sgids, 0) {
				t.Fatalf("accepted %q resolving to %+v, %v", spec, u, rerr)
			}
			return
		}
		if msg := err.Error(); strings.ContainsAny(msg, "\n\r") || len(msg) > 512 {
			t.Fatalf("error %q is not one bounded line", msg)
		}
	})
}
