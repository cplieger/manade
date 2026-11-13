package hostapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"

	"github.com/cplieger/manade/internal/config"
)

// The files of a TLS_DIR: the pool's CA, and each party's certificate and
// key under its own name, OrchestratorFiles for the orchestrator's.
const (
	CAFile            = "ca.pem"
	OrchestratorFiles = "orchestrator"
)

// CertFile is the certificate file name of the party named name.
func CertFile(name string) string { return name + ".pem" }

// KeyFile is the key file of the party named name.
func KeyFile(name string) string { return name + "-key.pem" }

// ServerTLS loads host name's TLS files from dir: a server accepting only a
// client certificate the pool's CA signed for client authentication with
// OrchestratorName as a DNS name.
func ServerTLS(dir, name string) (*tls.Config, error) {
	if err := config.CheckHostName(name); err != nil {
		return nil, fmt.Errorf("TLS_DIR: host %q: %w", name, err)
	}
	pool, cert, err := loadPair(dir, name, x509.ExtKeyUsageServerAuth, name)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return checkPeer(&cs, x509.ExtKeyUsageClientAuth, OrchestratorName)
		},
	}, nil
}

// ClientTLS loads the orchestrator's TLS files from dir. Each host's link
// takes a Clone with ServerName set to the host's config name.
func ClientTLS(dir string) (*tls.Config, error) {
	pool, cert, err := loadPair(dir, OrchestratorFiles, x509.ExtKeyUsageClientAuth, OrchestratorName)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return checkPeer(&cs, x509.ExtKeyUsageServerAuth, cs.ServerName)
		},
	}, nil
}

// loadPair reads dir's CA and the pair under file, whose certificate must
// carry usage and dnsName.
func loadPair(dir, file string, usage x509.ExtKeyUsage, dnsName string) (*x509.CertPool, tls.Certificate, error) {
	caPEM, err := os.ReadFile(filepath.Join(dir, CAFile))
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("TLS_DIR: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, tls.Certificate{}, fmt.Errorf("TLS_DIR: %s holds no certificate", filepath.Join(dir, CAFile))
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile(file)), filepath.Join(dir, KeyFile(file)))
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("TLS_DIR: %w", err)
	}
	if err := checkLeaf(cert.Leaf, usage, dnsName); err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("TLS_DIR: %s: %w", CertFile(file), err)
	}
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("TLS_DIR: %s is not signed by %s: %w", CertFile(file), CAFile, err)
	}
	return pool, cert, nil
}

// checkPeer requires a verified chain whose leaf names usage itself and
// carries dnsName. Go's own checks take a certificate with no extended key
// usage as good for any, and check no name on a client certificate.
func checkPeer(cs *tls.ConnectionState, usage x509.ExtKeyUsage, dnsName string) error {
	if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
		return errors.New("the peer's certificate is not verified")
	}
	return checkLeaf(cs.PeerCertificates[0], usage, dnsName)
}

func checkLeaf(leaf *x509.Certificate, usage x509.ExtKeyUsage, dnsName string) error {
	if !slices.Contains(leaf.ExtKeyUsage, usage) {
		return fmt.Errorf("the certificate is not for %s", usageName(usage))
	}
	if dnsName == "" || !slices.Contains(leaf.DNSNames, dnsName) {
		return fmt.Errorf("the certificate is not for %q", dnsName)
	}
	return nil
}

func usageName(u x509.ExtKeyUsage) string {
	if u == x509.ExtKeyUsageClientAuth {
		return "client authentication"
	}
	return "server authentication"
}

// ListenUnix serves the API socket at path with mode 0600, owned by uid,
// whatever the process's umask. The socket is bound inside a fresh 0700
// directory beside path, which no other uid can traverse to connect, and
// renamed onto path once its owner and mode are set; on a failure neither is
// left. Closing the listener removes path while it is still this socket.
func ListenUnix(path string, uid int) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("host API socket: %w", err)
	}
	// Short names: the bind path, longer than path, must fit sun_path's
	// 108 bytes (unix(7)).
	private, err := os.MkdirTemp(dir, ".")
	if err != nil {
		return nil, fmt.Errorf("host API socket: %w", err)
	}
	defer func() { _ = os.RemoveAll(private) }()
	tmp := filepath.Join(private, "s")
	ul, err := listen(tmp)
	if err != nil {
		return nil, fmt.Errorf("host API socket: %w", err)
	}
	ul.SetUnlinkOnClose(false)
	err = errors.Join(os.Chmod(tmp, 0o600), os.Lchown(tmp, uid, -1))
	if err == nil {
		err = os.Rename(tmp, path)
	}
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Lstat(path)
	}
	if err != nil {
		_ = ul.Close()
		return nil, fmt.Errorf("host API socket: %w", err)
	}
	return &unixListener{UnixListener: ul, path: path, fi: fi}, nil
}

// listen binds a Unix socket at path, a seam for observing the bind.
var listen = func(path string) (*net.UnixListener, error) {
	return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
}

type unixListener struct {
	*net.UnixListener
	fi   os.FileInfo
	path string
}

// Close removes path only while it is still this socket: a later listener
// may have renamed its own onto it.
func (l *unixListener) Close() error {
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

// ListenTLS serves the API on addr with cfg, ServerTLS's config.
func ListenTLS(ctx context.Context, addr string, cfg *tls.Config) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("MANADE_API_LISTEN: %w", err)
	}
	return tls.NewListener(ln, cfg), nil
}
