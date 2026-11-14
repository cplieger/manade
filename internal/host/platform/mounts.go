package platform

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	// Root is the path of the filesystem mounted at Point.
	Root  string
	Point string
	// Options are the mount's own options; Super the filesystem's, after
	// the " - " separator.
	Options string
	FSType  string
	Source  string
	Super   string
}

// HasSuper reports whether opt is among m's filesystem options.
func (m *Mount) HasSuper(opt string) bool { return hasOption(m.Super, opt) }

func hasOption(opts, opt string) bool { return slices.Contains(strings.Split(opts, ","), opt) }

// Mountinfo reads /proc/self/mountinfo under r.
func Mountinfo(r Root) ([]Mount, error) {
	s, err := r.read("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return parseMountinfo(s)
}

// parseMountinfo parses the format of proc(5): six fields, optional fields
// ended by "-", then the filesystem type, the source and the super options.
func parseMountinfo(s string) ([]Mount, error) {
	var out []Mount
	for n, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		sep := slices.Index(f, "-")
		if sep < 6 || len(f) < sep+4 {
			return nil, fmt.Errorf("mountinfo line %d: not the proc(5) format: %q", n+1, line)
		}
		out = append(out, Mount{
			Root: unescape(f[3]), Point: unescape(f[4]), Options: f[5],
			FSType: f[sep+1], Source: unescape(f[sep+2]), Super: f[sep+3],
		})
	}
	return out, nil
}

// unescape undoes the kernel's octal escapes of space, tab, newline and
// backslash in a mountinfo path (fs/proc_namespace.c, mangle).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// containerFiles are the files Docker bind-mounts into every container,
// which a profile mount source may never lie under.
var containerFiles = []string{"/etc/resolv.conf", "/etc/hosts", "/etc/hostname"}

// StartMounts answers the mount points the user's compose file gave the
// host container: every point in ms but the root filesystem and Docker's
// per-container files. It holds only when ms was read before the host made
// any mount of its own, since mountinfo cannot tell the two apart.
func StartMounts(ms []Mount) []string {
	var out []string
	for i := range ms {
		p := ms[i].Point
		if p == "/" || slices.Contains(containerFiles, p) || slices.Contains(out, p) {
			continue
		}
		out = append(out, p)
	}
	return out
}
