package hostapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeBackend is a Backend whose answers each test sets.
type fakeBackend struct {
	err      error
	changes  chan Runner
	probe    func(ctx context.Context) (ProbeReport, error)
	created  []RunnerRequest
	deleted  []string
	runners  []Runner
	logs     []byte
	diag     []byte
	status   Status
	deadline time.Duration
	mu       sync.Mutex
	diagAsk  bool
}

var _ Backend = (*fakeBackend)(nil)

func (f *fakeBackend) Status(context.Context) Status { return f.status }

func (f *fakeBackend) CreateRunner(_ context.Context, req *RunnerRequest) (Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Runner{}, f.err
	}
	for i := range f.created {
		if f.created[i].RunnerName == req.RunnerName {
			return Runner{Name: req.RunnerName, Profile: req.Profile, Scope: req.Scope, RunnerID: req.RunnerID, State: StateStarting}, nil
		}
	}
	f.created = append(f.created, *req)
	return Runner{Name: req.RunnerName, Profile: req.Profile, Scope: req.Scope, RunnerID: req.RunnerID, State: StateCreating}, nil
}

func (f *fakeBackend) Runners(context.Context) ([]Runner, error) { return f.runners, f.err }

func (f *fakeBackend) DeleteRunner(_ context.Context, name string, diag bool) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted, f.diagAsk = append(f.deleted, name), diag
	if f.err != nil {
		return nil, f.err
	}
	if diag {
		return f.diag, nil
	}
	return nil, nil
}

func (f *fakeBackend) Logs(context.Context, string, int) ([]byte, error) { return f.logs, f.err }

func (f *fakeBackend) Diag(context.Context, string) ([]byte, error) { return f.diag, f.err }

func (f *fakeBackend) Subscribe(ctx context.Context) <-chan Runner {
	out := make(chan Runner)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case r, ok := <-f.changes:
				if !ok {
					return
				}
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (f *fakeBackend) Probe(ctx context.Context) (ProbeReport, error) {
	if f.probe != nil {
		return f.probe(ctx)
	}
	return ProbeReport{Proven: true}, nil
}

func (f *fakeBackend) ProbeDeadline() time.Duration {
	if f.deadline == 0 {
		return time.Minute
	}
	return f.deadline
}

func validRequest() RunnerRequest {
	return RunnerRequest{Profile: "small", RunnerName: "manade-small-0123456789ab", Scope: "acme/app", RunnerID: 42, JITConfig: "eyJqaXQiOiJ0ZXN0In0="}
}

// testCA is a certificate authority for one test.
type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{key: key, cert: cert, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue writes dir/<file>.pem and dir/<file>-key.pem, a leaf for dnsName
// with usages, and dir/ca.pem.
func (ca *testCA) issue(t *testing.T, dir, file, dnsName string, usages ...x509.ExtKeyUsage) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(CAFile, ca.pem)
	write(CertFile(file), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(KeyFile(file), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}
