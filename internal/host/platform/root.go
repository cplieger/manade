// Package platform reads what a runner host's kernel and container give it:
// its cgroup limits and figures, the storage driver its data root can hold,
// the rule backend, the bridge sysctls and the container's own mounts. Every
// reader takes a Root, System in production and a fixture tree in tests.
package platform

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Root is the directory the readers take as "/".
type Root string

// System is the host container's own filesystem.
const System Root = "/"

// Path answers p, an absolute path, under r.
func (r Root) Path(p string) string { return filepath.Join(string(r), p) }

// read answers the file at p under r, its trailing newline trimmed.
func (r Root) read(p string) (string, error) {
	b, err := os.ReadFile(r.Path(p))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// lookup is read with a missing file answered as not found, not an error.
func (r Root) lookup(p string) (s string, ok bool, err error) {
	s, err = r.read(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	return s, err == nil, err
}

func (r Root) exists(p string) bool {
	_, err := os.Stat(r.Path(p))
	return err == nil
}
