package jobspec

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/config"
)

// The fixtures follow GitHub's runner image: adduser gives runner uid and gid
// 1001, and its Dockerfile adds runner to sudo (27) and docker (123).
const (
	ghPasswd = "root:x:0:0:root:/root:/bin/bash\nrunner:x:1001:1001:,,,:/home/runner:/bin/bash\n"
	ghGroup  = "root:x:0:\nsudo:x:27:runner\nrunner:x:1001:\ndocker:x:123:runner\n"
)

func TestResolveUserGitHubRunner(t *testing.T) {
	u, err := resolveUser("runner", Files{Passwd: []byte(ghPasswd), Group: []byte(ghGroup)})
	if err != nil || u.uid != 1001 || u.gid != 1001 || !slices.Equal(u.sgids, []int{27, 123}) {
		t.Errorf("resolveUser(runner) = %+v, %v, want 1001:1001 with groups [27 123]", u, err)
	}
	if err := CheckUser("runner", Files{Passwd: []byte(ghPasswd), Group: []byte(ghGroup)}); err != nil {
		t.Errorf("CheckUser(runner) = %v, want nil", err)
	}
}

func TestCheckUser(t *testing.T) {
	tests := []struct {
		name, user, passwd, group string
		ok                        bool
	}{
		{"emptyUserIsRoot", "", ghPasswd, ghGroup, false},
		{"emptyUserNoFiles", "", "", "", false},
		{"rootName", "root", ghPasswd, ghGroup, false},
		{"uidZero", "0", ghPasswd, ghGroup, false},
		{"gidZero", "1001:0", ghPasswd, ghGroup, false},
		{"rootGroupName", "runner:root", ghPasswd, ghGroup, false},
		{"runnerIsUidZeroInPasswd", "runner", "runner:x:0:0::/home/runner:/bin/bash\n", ghGroup, false},
		{"bareUidNoPasswdLineRunsAsGidZero", "1001", "root:x:0:0:root:/root:/bin/bash\n", ghGroup, false},
		{"bareUidPasswdGidZero", "1001", "runner:x:1001:0::/home/runner:/bin/bash\n", ghGroup, false},
		{"memberOfGroupZero", "runner", ghPasswd, "root:x:0:runner\nrunner:x:1001:\n", false},
		{"unknownName", "ghost", ghPasswd, ghGroup, false},
		{"unknownGroupName", "runner:ghosts", ghPasswd, ghGroup, false},
		{"nameWithoutPasswd", "runner", "", ghGroup, false},
		{"uidOverRange", "2147483648:1001", ghPasswd, ghGroup, false},
		{"negativeUid", "-1", ghPasswd, ghGroup, false},
		{"passwdLineOverScannerLimit", "runner", "x:" + strings.Repeat("a", 70<<10) + "\n" + ghPasswd, ghGroup, false},
		{"firstMatchWinsRoot", "runner", "runner:x:0:0::/:/bin/sh\n" + ghPasswd, ghGroup, false},

		{"runner", "runner", ghPasswd, ghGroup, true},
		{"bareUidResolvesThroughPasswd", "1001", ghPasswd, ghGroup, true},
		{"firstMatchWinsRunner", "runner", ghPasswd + "runner:x:0:0::/:/bin/sh\n", ghGroup, true},
		// An explicit group takes no supplementary groups, so runner's
		// membership of group 0 does not apply.
		{"explicitGroupSkipsSupplementary", "1001:1001", ghPasswd, "root:x:0:runner\n", true},
		{"numericWithoutFiles", "1001:1001", "", "", true},
		{"namedGroup", "runner:docker", ghPasswd, ghGroup, true},
		{"numericGroupWithoutEntry", "runner:5000", ghPasswd, ghGroup, true},
		{"groupCommentSkipped", "runner", ghPasswd, "#root:x:0:runner\nrunner:x:1001:\n", true},
		{"crlfAndIndent", "runner", strings.ReplaceAll(ghPasswd, "\n", "\r\n  "), strings.ReplaceAll(ghGroup, "\n", "\r\n\t"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckUser(tt.user, Files{Passwd: []byte(tt.passwd), Group: []byte(tt.group)})
			if tt.ok && err != nil {
				t.Errorf("CheckUser(%q) = %v, want nil", tt.user, err)
			}
			if !tt.ok && !errors.Is(err, ErrRootUser) {
				t.Errorf("CheckUser(%q) = %v, want ErrRootUser", tt.user, err)
			}
		})
	}
}

func TestResolveUserMissingFileIsEmptyFile(t *testing.T) {
	for _, spec := range []string{"", "runner", "1001", "1001:1001", "runner:docker"} {
		a, errA := resolveUser(spec, Files{})
		b, errB := resolveUser(spec, Files{Passwd: []byte{}, Group: []byte{}})
		if (errA == nil) != (errB == nil) || a.uid != b.uid || a.gid != b.gid || !slices.Equal(a.sgids, b.sgids) {
			t.Errorf("spec %q: missing files = %+v, %v; empty files = %+v, %v", spec, a, errA, b, errB)
		}
	}
}

func TestCheckUserErrorQuotesNoFileText(t *testing.T) {
	err := CheckUser("ghost\nforged: line", Files{Passwd: []byte("secretname:x:1:1::/:/bin/sh\n")})
	if err == nil || strings.ContainsAny(err.Error(), "\n\r") || strings.Contains(err.Error(), "secretname") {
		t.Errorf("CheckUser error = %q, want one line without file text", err)
	}
}

func TestNeedsUserCheck(t *testing.T) {
	tests := []struct {
		name string
		p    config.Profile
		want bool
	}{
		{"image user", config.Profile{}, true},
		{"profile user", config.Profile{User: "1001:1001"}, false},
		{"allow root", config.Profile{AllowRoot: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsUserCheck(&tt.p); got != tt.want {
				t.Errorf("NeedsUserCheck = %v, want %v", got, tt.want)
			}
		})
	}
}
