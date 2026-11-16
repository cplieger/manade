package host

import (
	"context"
	"net"
	"syscall"

	"github.com/cplieger/manade/internal/egress"
)

// controlPlaneDialer dials the host's channel to the control plane. Each
// TCP socket it opens carries egress.Mark, which only the output chain's
// exception accepts match, so no other socket of the host passes them; a
// Unix socket needs no exception and is left unmarked.
func controlPlaneDialer() *net.Dialer {
	return &net.Dialer{Control: markTCP}
}

// dialSocket dials the control plane's socket at path.
func dialSocket(path string) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		return controlPlaneDialer().DialContext(ctx, "unix", path)
	}
}

func markTCP(network, _ string, c syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" {
		return nil
	}
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, egress.Mark)
	}); err != nil {
		return err
	}
	return serr
}
