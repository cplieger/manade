package hostlink

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// ListenSocket listens for local hosts at path with mode 0600, owned by the
// process, whatever its umask. The socket is bound inside a fresh 0700
// directory beside path and renamed onto path once its mode is set, which
// also replaces a socket a stopped control plane left there; on a failure
// neither is left. Closing the listener removes path while it is still this
// socket.
func ListenSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("hosts socket: %w", err)
	}
	// Short names: the bind path, longer than path, must fit sun_path's
	// 108 bytes (unix(7)).
	private, err := os.MkdirTemp(dir, ".")
	if err != nil {
		return nil, fmt.Errorf("hosts socket: %w", err)
	}
	defer func() { _ = os.RemoveAll(private) }()
	tmp := filepath.Join(private, "s")
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("hosts socket: %w", err)
	}
	ul.SetUnlinkOnClose(false)
	err = os.Chmod(tmp, 0o600)
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Lstat(tmp)
	}
	// The rename is the last step that can fail: nothing removes path after it.
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = ul.Close()
		return nil, fmt.Errorf("hosts socket: %w", err)
	}
	return &socketListener{UnixListener: ul, path: path, fi: fi}, nil
}

type socketListener struct {
	*net.UnixListener
	fi   os.FileInfo
	path string
}

// Close removes path only while it is still this socket: a later listener
// may have renamed its own onto it.
func (l *socketListener) Close() error {
	err := l.UnixListener.Close()
	cur, serr := os.Lstat(l.path)
	switch {
	case errors.Is(serr, os.ErrNotExist):
	case serr != nil:
		err = errors.Join(err, serr)
	case os.SameFile(cur, l.fi):
		if rerr := os.Remove(l.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
	}
	return err
}
