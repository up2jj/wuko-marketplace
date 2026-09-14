// Package localtls creates short-lived certificate authorities for loopback services.
package localtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Authority owns one in-memory CA and its cached server certificates.
type Authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
	sha256      string
	mu          sync.Mutex
	leaves      map[string]*tls.Certificate
}

// NewAuthority creates a CA valid for one day around now.
func NewAuthority(now time.Time, commonName string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating CA certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parsing generated CA certificate: %w", err)
	}
	digest := sha256.Sum256(der)
	return &Authority{certificate: certificate, key: key,
		pem:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		sha256: hex.EncodeToString(digest[:]), leaves: make(map[string]*tls.Certificate)}, nil
}

func (authority *Authority) PEM() []byte { return append([]byte(nil), authority.pem...) }

func (authority *Authority) SHA256() string { return authority.sha256 }

// CertificateFor returns a cached server certificate for host.
func (authority *Authority) CertificateFor(host string) (*tls.Certificate, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, fmt.Errorf("TLS server name is empty")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if certificate := authority.leaves[host]; certificate != nil {
		return certificate, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating certificate key for %s: %w", host, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: authority.certificate.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, &key.PublicKey, authority.key)
	if err != nil {
		return nil, fmt.Errorf("creating certificate for %s: %w", host, err)
	}
	certificate := &tls.Certificate{Certificate: [][]byte{der, authority.certificate.Raw}, PrivateKey: key}
	authority.leaves[host] = certificate
	return certificate, nil
}

// WriteCertificate writes PEM to a private temporary file under runDir.
func WriteCertificate(runDir, pattern string, certificate []byte) (string, error) {
	if runDir == "" {
		runDir = os.TempDir()
	}
	file, err := os.CreateTemp(runDir, pattern)
	if err != nil {
		return "", fmt.Errorf("creating CA certificate file: %w", err)
	}
	path := filepath.Clean(file.Name())
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("setting CA certificate permissions: %w", err)
	}
	if _, err := file.Write(certificate); err != nil {
		return "", fmt.Errorf("writing CA certificate: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing CA certificate: %w", err)
	}
	failed = false
	return path, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating certificate serial: %w", err)
	}
	return serial, nil
}
