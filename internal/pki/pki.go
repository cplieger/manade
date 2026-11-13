// Package pki issues a pool's TLS files: one CA, a server-only certificate
// per host carrying its config name, and one client-only certificate for the
// orchestrator carrying hostapi.OrchestratorName. Each party's files land in
// a directory of their own that is a ready TLS_DIR.
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

// The directories Issue writes under its root, beside one per host.
const (
	CADir           = "ca"
	OrchestratorDir = "orchestrator"
	caKeyFile       = "ca-key.pem"
)

// Validity is how long a new CA, and any certificate it issues, is valid;
// a certificate never outlives its CA.
const Validity = 10 * 365 * 24 * time.Hour

// Issue writes into root the CA (CADir, made when absent), the
// orchestrator's TLS_DIR (OrchestratorDir, made when absent) and a TLS_DIR
// per named host, and answers the directories it made. Before it writes
// anything it checks every name, refuses a host whose directory exists, and
// refuses an existing OrchestratorDir whose ca.pem is not the CA in use; each
// directory appears whole or not at all, and survives a power loss once
// Issue returns.
func Issue(root string, hosts []string, now time.Time) ([]string, error) {
	if err := checkHosts(root, hosts); err != nil {
		return nil, err
	}
	ca, found, err := loadCA(root, now)
	if err != nil {
		return nil, err
	}
	orch := filepath.Join(root, OrchestratorDir)
	orchExists, err := checkOrchestrator(orch, ca, found)
	if err != nil {
		return nil, err
	}
	if !found {
		if serr := checkStranded(root); serr != nil {
			return nil, serr
		}
	}
	var out []string
	if !found {
		if ca, err = makeCA(filepath.Join(root, CADir), now); err != nil {
			return nil, err
		}
		out = append(out, filepath.Join(root, CADir))
	}
	if !orchExists {
		if err := ca.party(orch, hostapi.OrchestratorFiles, hostapi.OrchestratorName, x509.ExtKeyUsageClientAuth, now); err != nil {
			return out, err
		}
		out = append(out, orch)
	}
	for _, h := range hosts {
		dir := filepath.Join(root, h)
		if err := ca.party(dir, h, h, x509.ExtKeyUsageServerAuth, now); err != nil {
			return out, err
		}
		out = append(out, dir)
	}
	return out, nil
}

// checkOrchestrator answers whether orch exists, refusing one the CA ca did
// not issue or that has no CA beside it (found false): hosts issued now
// would trust a CA the orchestrator's certificate is not from.
func checkOrchestrator(orch string, ca *authority, found bool) (bool, error) {
	_, err := os.Lstat(orch)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pki: %w", err)
	case !found:
		return false, fmt.Errorf("pki: %s exists but its CA %s does not, so new hosts would not trust the orchestrator; restore %s, or remove %s and every host directory to issue the whole pool again", orch, CADir, CADir, orch)
	}
	got, err := os.ReadFile(filepath.Join(orch, hostapi.CAFile))
	if err != nil {
		return false, fmt.Errorf("pki: %w", err)
	}
	if !bytes.Equal(got, ca.pem) {
		return false, fmt.Errorf("pki: %s is not from the CA in %s; restore that CA, or remove %s and every host directory to issue the whole pool again", filepath.Join(orch, hostapi.CAFile), CADir, orch)
	}
	return true, nil
}

// checkStranded refuses a root without a CA that still holds a party's
// directory: a new CA would split the pool, its hosts trusting the old one.
// writeDir's temporaries start with a dot, which no party name does.
func checkStranded(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	var stranded []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, e.Name(), hostapi.CAFile)); err == nil {
			stranded = append(stranded, filepath.Join(root, e.Name()))
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return fmt.Errorf("pki: %w", err)
		}
	}
	if len(stranded) == 0 {
		return nil
	}
	return fmt.Errorf("pki: %s trust a CA %s no longer holds, so a new CA would split the pool; restore %s, or remove %s to issue the whole pool again",
		strings.Join(stranded, ", "), filepath.Join(root, CADir), CADir, strings.Join(stranded, " and "))
}

func checkHosts(root string, hosts []string) error {
	var errs []error
	for i, h := range hosts {
		switch {
		case config.CheckHostName(h) != nil:
			errs = append(errs, fmt.Errorf("host %q: %w", h, config.CheckHostName(h)))
		case slices.Contains(hosts[:i], h):
			errs = append(errs, fmt.Errorf("host %s: named twice", h))
		default:
			if _, err := os.Lstat(filepath.Join(root, h)); err == nil {
				errs = append(errs, fmt.Errorf("host %s: %s exists; remove it to issue the host again", h, filepath.Join(root, h)))
			} else if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("host %s: %w", h, err))
			}
		}
	}
	return errors.Join(errs...)
}

type authority struct {
	key  crypto.Signer
	cert *x509.Certificate
	pem  []byte
}

// loadCA reads root's CA; found is false when CADir is absent.
func loadCA(root string, now time.Time) (ca *authority, found bool, err error) {
	dir := filepath.Join(root, CADir)
	certPEM, err := os.ReadFile(filepath.Join(dir, hostapi.CAFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, serr := os.Lstat(dir); serr == nil {
			return nil, false, fmt.Errorf("pki: %s holds no %s; remove it, or point at the pool's own directory", dir, hostapi.CAFile)
		}
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("pki: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("pki: %s holds the CA without its key %s, so it can issue nothing; issue the whole pool again into a new directory", dir, caKeyFile)
	}
	if err != nil {
		return nil, false, fmt.Errorf("pki: %w", err)
	}
	if ca, err = parseCA(certPEM, keyPEM, now); err != nil {
		return nil, false, fmt.Errorf("pki: %s: %w", dir, err)
	}
	return ca, true, nil
}

func parseCA(certPEM, keyPEM []byte, now time.Time) (*authority, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("the CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("the CA key does not match its certificate")
	}
	if !cert.IsCA || !now.Before(cert.NotAfter) {
		return nil, fmt.Errorf("the CA is not a CA valid now (until %s)", cert.NotAfter.Format(time.DateOnly))
	}
	return &authority{key: key, cert: cert, pem: certPEM}, nil
}

func makeCA(dir string, now time.Time) (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "manade pool CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(Validity),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	ca := &authority{key: key, cert: cert, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	err = writeDir(dir, 0o700, map[string]file{
		hostapi.CAFile: {data: ca.pem, mode: 0o644},
		caKeyFile:      {data: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), mode: 0o600},
	})
	return ca, err
}

// party writes dir: ca.pem and a fresh certificate and key under name,
// for dnsName and usage only.
func (ca *authority) party(dir, name, dnsName string, usage x509.ExtKeyUsage, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	notAfter := now.Add(Validity)
	if ca.cert.NotAfter.Before(notAfter) {
		notAfter = ca.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: now.Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	return writeDir(dir, 0o755, map[string]file{
		hostapi.CAFile:         {data: ca.pem, mode: 0o644},
		hostapi.CertFile(name): {data: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), mode: 0o644},
		hostapi.KeyFile(name):  {data: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), mode: 0o600},
	})
}

type file struct {
	data []byte
	mode os.FileMode
}

// writeDir makes dir with files, through a temporary directory beside it
// renamed into place, so a failure or a power loss leaves no partial dir:
// each file and the temporary directory are synced before the rename, and
// the parent after it.
func writeDir(dir string, mode os.FileMode, files map[string]file) (err error) {
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".tmp-")
	if err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	for name, f := range files {
		if err := writeSynced(filepath.Join(tmp, name), f); err != nil {
			return fmt.Errorf("pki: %w", err)
		}
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	if err := syncDir(tmp); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("pki: %s is written but not durable: %w", dir, err)
	}
	return nil
}

func writeSynced(path string, f file) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.mode)
	if err != nil {
		return err
	}
	_, err = out.Write(f.data)
	if err == nil {
		// The umask narrows OpenFile's mode; the files' modes are exact.
		err = out.Chmod(f.mode)
	}
	if err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// serial is a random 127-bit serial number, as RFC 5280 4.1.2.2 asks: positive
// and at most 20 octets.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand.Read never fails on Linux (Go 1.24 and later)
	}
	return n.Add(n, big.NewInt(1))
}
