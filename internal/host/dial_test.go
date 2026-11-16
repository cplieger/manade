package host

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cplieger/manade/internal/egress"
)

func socketMark(t *testing.T, c net.Conn) int {
	t.Helper()
	sc, ok := c.(syscall.Conn)
	if !ok {
		t.Fatalf("%T has no raw connection", c)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var mark int
	var gerr error
	if err := raw.Control(func(fd uintptr) { mark, gerr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return mark
}

// The control plane's dialer marks a TCP socket with the mark the
// exception accepts match. Setting it needs CAP_NET_ADMIN, which the
// privileged host holds; an unprivileged run skips this case and keeps the
// Unix one.
func TestControlPlaneDialerMarksTCP(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c, err := controlPlaneDialer().DialContext(t.Context(), "tcp", ln.Addr().String())
	if errors.Is(err, syscall.EPERM) {
		t.Skip("setting SO_MARK needs CAP_NET_ADMIN")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if got := socketMark(t, c); got != egress.Mark {
		t.Errorf("SO_MARK = %#x, want %#x", got, egress.Mark)
	}
}

func TestControlPlaneDialerLeavesAUnixSocketUnmarked(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "hosts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c, err := controlPlaneDialer().DialContext(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if got := socketMark(t, c); got != 0 {
		t.Errorf("SO_MARK = %#x, want none", got)
	}
}

// No production code but the control plane's dialer sets a socket mark or
// names egress.Mark outside the rules that match it, so the exception stays
// the supervisor's alone.
func TestOnlyTheControlPlaneDialerMarksASocket(t *testing.T) {
	t.Parallel()
	var found []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && (sel.Sel.Name == "SO_MARK" || pkg.Name == "egress" && sel.Sel.Name == "Mark") {
					found = append(found, filepath.ToSlash(path)+": "+pkg.Name+"."+sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"../../internal/host/dial.go: syscall.SO_MARK", "../../internal/host/dial.go: egress.Mark"}
	if strings.Join(found, "\n") != strings.Join(want, "\n") {
		t.Errorf("marks set at:\n%s\nwant only:\n%s", strings.Join(found, "\n"), strings.Join(want, "\n"))
	}
}
