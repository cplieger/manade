package jobspec

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/r5/config"
)

// The files the user check reads from a probe container, and the cap on
// each read.
const (
	PasswdPath    = "/etc/passwd"
	GroupPath     = "/etc/group"
	UserFileLimit = 1 << 20
)

// ErrRootUser is an image whose user resolves to uid 0, gid 0 or a
// supplementary gid of 0, or does not resolve through its own files.
var ErrRootUser = errors.New("jobspec: the image's user is root or does not resolve")

// Files are an image's /etc/passwd and /etc/group; nil is a missing file,
// which resolves exactly as an empty one does.
type Files struct {
	Passwd []byte
	Group  []byte
}

// NeedsUserCheck reports whether profile p runs as its image's own user and
// so needs CheckUser before it is served. A profile's numeric uid:gid user
// gets no supplementary groups, and allow_root is the deliberate grant.
func NeedsUserCheck(p *config.Profile) bool { return p.User == "" && !p.AllowRoot }

// CheckUser resolves imageUser, the image's USER, through its files as Docker
// does, and fails with ErrRootUser unless uid, gid and every supplementary
// gid are non-zero. The error quotes imageUser sanitized, and no file text.
func CheckUser(imageUser string, f Files) error {
	shown := textsafe.SanitizeSingleLineBounded(imageUser, 32)
	u, err := resolveUser(imageUser, f)
	if err != nil {
		return fmt.Errorf("%w: image user %q: %w", ErrRootUser, shown, err)
	}
	in0 := ""
	if slices.Contains(u.sgids, 0) {
		in0 = " in group 0"
	}
	if u.uid == 0 || u.gid == 0 || in0 != "" {
		return fmt.Errorf("%w: image user %q resolves to %d:%d%s; set a numeric non-root user or allow_root: true",
			ErrRootUser, shown, u.uid, u.gid, in0)
	}
	return nil
}

// The rest of this file is ported from GetExecUser in
// https://github.com/moby/sys/blob/main/user/user.go (Apache-2.0, see
// THIRD_PARTY_NOTICES.md) with zero defaults, as Docker calls it, so the
// check resolves exactly the user the runner will run as.

type execUser struct {
	sgids    []int
	uid, gid int
}

// maxID is upstream's bound on a uid or gid, for 32-bit systems.
const maxID = 1<<31 - 1

// idArg is one side of a user spec: a name, or a number that is never
// looked up as a name.
type idArg struct {
	name    string
	id      int
	numeric bool
}

func (a *idArg) matches(name string, id int) bool {
	if a.numeric {
		return id == a.id
	}
	return name == a.name
}

func parseIDArg(s string) (*idArg, error) {
	if s == "" {
		return nil, nil //nolint:nilnil // nil is an absent side of the spec, as upstream has it.
	}
	id, err := strconv.Atoi(s)
	switch {
	case errors.Is(err, strconv.ErrSyntax):
		return &idArg{name: s}, nil
	case err != nil || id < 0 || id > maxID:
		return nil, fmt.Errorf("ids must be in range 0-%d", maxID)
	}
	return &idArg{name: s, id: id, numeric: true}, nil
}

type passwdEntry struct {
	name     string
	uid, gid int
}

type groupEntry struct {
	name    string
	members []string
	gid     int
}

func resolveUser(spec string, f Files) (execUser, error) {
	usr, grp, _ := strings.Cut(spec, ":")
	ua, err := parseIDArg(usr)
	if err != nil {
		return execUser{}, err
	}
	ga, err := parseIDArg(grp)
	if err != nil {
		return execUser{}, err
	}
	u, matched, err := lookupUser(ua, f.Passwd)
	if err != nil || ga == nil && matched == "" {
		return u, err
	}
	return lookupGroups(u, matched, ga, f.Group)
}

// lookupUser answers the user and the passwd name it matched, if any. With no
// user given, uid 0 is the default it looks up.
func lookupUser(ua *idArg, passwd []byte) (u execUser, matched string, err error) {
	var users []passwdEntry
	err = eachLine(passwd, bufio.MaxScanTokenSize, false, func(fl [][]byte) {
		e := passwdEntry{name: field(fl, 0), uid: atoi(field(fl, 2)), gid: atoi(field(fl, 3))}
		if ua == nil && e.uid == 0 || ua != nil && ua.matches(e.name, e.uid) {
			users = append(users, e)
		}
	})
	switch {
	case err != nil:
		return u, "", fmt.Errorf("reading passwd: %w", err)
	case len(users) > 0:
		return execUser{uid: users[0].uid, gid: users[0].gid}, users[0].name, nil
	case ua != nil && !ua.numeric:
		return u, "", errors.New("no matching entries in passwd file")
	case ua != nil:
		u.uid = ua.id
	}
	return u, "", nil
}

// lookupGroups sets the explicit group ga, or else the supplementary groups
// listing the matched name.
func lookupGroups(u execUser, matched string, ga *idArg, group []byte) (execUser, error) {
	var groups []groupEntry
	err := eachLine(group, 1<<20, true, func(fl [][]byte) {
		e := groupEntry{name: field(fl, 0), gid: atoi(field(fl, 2))}
		if l := field(fl, 3); l != "" {
			e.members = strings.Split(l, ",")
		}
		if ga == nil && slices.Contains(e.members, matched) || ga != nil && ga.matches(e.name, e.gid) {
			groups = append(groups, e)
		}
	})
	switch {
	case err != nil:
		return u, fmt.Errorf("reading group: %w", err)
	case ga != nil && len(groups) > 0:
		u.gid = groups[0].gid
	case ga != nil && !ga.numeric:
		return u, errors.New("no matching entries in group file")
	case ga != nil:
		u.gid = ga.id
	default:
		for _, g := range groups {
			u.sgids = append(u.sgids, g.gid)
		}
	}
	return u, nil
}

// eachLine hands fn the colon-separated fields of each trimmed, non-empty
// line; comments skips lines starting with '#', as upstream does for
// /etc/group only.
func eachLine(data []byte, maxLine int, comments bool, fn func([][]byte)) error {
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(nil, maxLine)
	for s.Scan() {
		line := bytes.TrimSpace(s.Bytes())
		if len(line) == 0 || comments && line[0] == '#' {
			continue
		}
		fn(bytes.Split(line, []byte(":")))
	}
	return s.Err()
}

func field(fields [][]byte, i int) string {
	if i < len(fields) {
		return string(fields[i])
	}
	return ""
}

// atoi reads a numeric field as upstream does: a malformed one is 0.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
