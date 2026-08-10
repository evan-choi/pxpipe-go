package mitm

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	certificateFilename = "mitm-ca.pem"
	privateKeyFilename  = "mitm-ca-key.pem"
)

// Authority owns a local certificate authority and issues per-host leaf
// certificates. It never installs trust into an operating-system trust store.
type Authority struct {
	certificate     *x509.Certificate
	certificatePath string
	privateKey      crypto.Signer
	leafKey         crypto.Signer

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// LoadOrCreateAuthority loads a valid CA pair from dir or creates a new pair.
func LoadOrCreateAuthority(dir string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create CA directory: %w", err)
	}
	unlock, err := lockAuthorityDirectory(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	certificatePath := filepath.Join(dir, certificateFilename)
	privateKeyPath := filepath.Join(dir, privateKeyFilename)

	_, certificate, privateKey, err := loadAuthority(certificatePath, privateKeyPath)
	if err != nil {
		_, certificate, privateKey, err = createAuthority(certificatePath, privateKeyPath)
		if err != nil {
			return nil, err
		}
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	return &Authority{
		certificate: certificate, certificatePath: certificatePath, privateKey: privateKey,
		leafKey: leafKey, leafs: make(map[string]*tls.Certificate),
	}, nil
}

// CertificatePath is the PEM path callers can expose to a child process.
func (a *Authority) CertificatePath() string {
	return a.certificatePath
}

func loadAuthority(certificatePath, privateKeyPath string) ([]byte, *x509.Certificate, crypto.Signer, error) {
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return nil, nil, nil, err
	}
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return nil, nil, nil, errors.New("invalid CA certificate PEM")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, nil, nil, err
	}
	if !certificate.IsCA || time.Now().Before(certificate.NotBefore) || time.Now().After(certificate.NotAfter) {
		return nil, nil, nil, errors.New("CA certificate is not currently valid")
	}

	privateKeyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	privateKeyBlock, _ := pem.Decode(privateKeyPEM)
	if privateKeyBlock == nil || privateKeyBlock.Type != "PRIVATE KEY" {
		return nil, nil, nil, errors.New("invalid CA private key PEM")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(privateKeyBlock.Bytes)
	if err != nil {
		return nil, nil, nil, err
	}
	privateKey, ok := parsedKey.(crypto.Signer)
	if !ok {
		return nil, nil, nil, errors.New("CA private key cannot sign")
	}
	certificatePublic, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	privatePublic, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil || !bytes.Equal(certificatePublic, privatePublic) {
		return nil, nil, nil, errors.New("CA certificate and private key do not match")
	}
	return certificatePEM, certificate, privateKey, nil
}

func createAuthority(certificatePath, privateKeyPath string) ([]byte, *x509.Certificate, crypto.Signer, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "pxpipe local CA", Organization: []string{"pxpipe"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse generated CA certificate: %w", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal CA private key: %w", err)
	}
	if err := writeFileAtomic(certificatePath, certificatePEM, 0o644); err != nil {
		return nil, nil, nil, fmt.Errorf("write CA certificate: %w", err)
	}
	if err := writeFileAtomic(privateKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}), 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("write CA private key: %w", err)
	}
	if err := os.Chmod(privateKeyPath, 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("secure CA private key: %w", err)
	}
	return certificatePEM, certificate, privateKey, nil
}

// TLSConfig returns a server configuration with a leaf certificate for host.
func (a *Authority) TLSConfig(host string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			} else if !strings.EqualFold(strings.TrimSuffix(name, "."), strings.TrimSuffix(host, ".")) {
				return nil, fmt.Errorf("TLS server name %q does not match CONNECT host %q", name, host)
			}
			return a.certificateFor(name)
		},
	}
}

func (a *Authority) certificateFor(host string) (*tls.Certificate, error) {
	host, _ = splitAuthority(host)
	a.mu.Lock()
	defer a.mu.Unlock()
	if certificate := a.leafs[host]; certificate != nil {
		return certificate, nil
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, a.leafKey.Public(), a.privateKey)
	if err != nil {
		return nil, fmt.Errorf("create certificate for %s: %w", host, err)
	}
	certificate := &tls.Certificate{
		Certificate: [][]byte{der, a.certificate.Raw},
		PrivateKey:  a.leafKey,
	}
	a.leafs[host] = certificate
	return certificate, nil
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return serial
}

func lockAuthorityDirectory(dir string) (func(), error) {
	lockPath := filepath.Join(dir, ".mitm-ca.lock")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := os.Mkdir(lockPath, 0o700); err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock CA directory: %w", err)
		}
		info, err := os.Stat(lockPath)
		if err == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for CA directory lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
