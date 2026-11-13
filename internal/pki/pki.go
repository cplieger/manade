// Package pki issues a pool's TLS files: one CA, a server-only certificate
// per host carrying its config name, and one client-only certificate for the
// orchestrator carrying hostapi.OrchestratorName. Each party's files land in
// a directory of their own that is a ready TLS_DIR.
package pki

import (
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
// per named host, and answers the directories it made. It checks every name
// and refuses a host whose directory exists before it writes anything, and
// each directory appears whole or not at all.
func Issue(root string, hosts []string, now time.Time) ([]string, error) {
	if err := checkHosts(root, hosts); err != nil {
		return nil, err
	}
	ca, made, err := loadOrMakeCA(root, now)
	if err != nil {
		return nil, err
	}
	var out []string
	if made {
		out = append(out, filepath.Join(root, CADir))
	}
	orch := filepath.Join(root, OrchestratorDir)
	switch _, serr := os.Lstat(orch); {
	case errors.Is(serr, os.ErrNotExist):
		if err := ca.party(orch, hostapi.OrchestratorFiles, hostapi.OrchestratorName, x509.ExtKeyUsageClientAuth, now); err != nil {
			return out, err
		}
		out = append(out, orch)
	case serr != nil:
		return out, fmt.Errorf("pki: %w", serr)
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

func checkHosts(root string, hosts []string) error {
	var errs []error
	for i, h := range hosts {
		switch {
		case config.CheckHostName(h) != nil:
			errs = append(errs, fmt.Errorf("host %q: %w", h, config.CheckHostName(h)))
		case h == CADir || h == OrchestratorDir:
			errs = append(errs, fmt.Errorf("host %s: the name of a directory pki keeps for itself; rename the host", h))
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

// loadOrMakeCA reads root's CA, or makes one when CADir is absent.
func loadOrMakeCA(root string, now time.Time) (ca *authority, made bool, err error) {
	dir := filepath.Join(root, CADir)
	certPEM, err := os.ReadFile(filepath.Join(dir, hostapi.CAFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, serr := os.Lstat(dir); serr == nil {
			return nil, false, fmt.Errorf("pki: %s holds no %s; remove it, or point at the pool's own directory", dir, hostapi.CAFile)
		}
		ca, err = makeCA(dir, now)
		return ca, err == nil, err
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
	ca, err = parseCA(certPEM, keyPEM, now)
	if err != nil {
		return nil, false, fmt.Errorf("pki: %s: %w", dir, err)
	}
	return ca, false, nil
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
// renamed into place, so a failure leaves no partial dir.
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
		if err := os.WriteFile(filepath.Join(tmp, name), f.data, f.mode); err != nil {
			return fmt.Errorf("pki: %w", err)
		}
		if err := os.Chmod(filepath.Join(tmp, name), f.mode); err != nil {
			return fmt.Errorf("pki: %w", err)
		}
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	return nil
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
