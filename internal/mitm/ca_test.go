package mitm

import (
	"bytes"
	"crypto/x509"
	"os"
	"sync"
	"testing"
)

func TestAuthorityPersistsAndIssuesCertificates(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateAuthority(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateAuthority(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.certificate.Equal(second.certificate) {
		t.Fatal("authority was not reused")
	}

	info, err := os.Stat(dir + "/" + privateKeyFilename)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("private key mode = %o", got)
	}

	leaf, err := first.certificateFor("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(first.certificate)
	if _, err := certificate.Verify(x509.VerifyOptions{DNSName: "127.0.0.1", Roots: roots}); err != nil {
		t.Fatalf("verify leaf: %v", err)
	}
}

func TestAuthorityConcurrentCreationUsesOnePair(t *testing.T) {
	dir := t.TempDir()
	const count = 12
	certificates := make(chan []byte, count)
	errors := make(chan error, count)
	var wait sync.WaitGroup
	for range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			authority, err := LoadOrCreateAuthority(dir)
			if err != nil {
				errors <- err
				return
			}
			certificates <- authority.certificate.Raw
		}()
	}
	wait.Wait()
	close(errors)
	close(certificates)
	for err := range errors {
		t.Error(err)
	}
	var first []byte
	for certificate := range certificates {
		if first == nil {
			first = certificate
			continue
		}
		if !bytes.Equal(first, certificate) {
			t.Fatal("concurrent callers loaded different CA certificates")
		}
	}
}
