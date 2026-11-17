package egress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// CADir is the egress CA's directory in glovd's data directory, which only
// SYSTEM and administrators can open: ca.pem, which isolated VMs trust, and
// ca.key, which never leaves the host.
const CADir = "egress-ca"

// CACommonName and LeafLifetime are macOS's (Sources/GlovDaemonAPI/EgressCA.swift).
const (
	CACommonName = "glov egress CA"
	// LeafLifetime is how long a host certificate is valid; the proxy makes
	// a new one once it is a day old.
	LeafLifetime = 7 * 24 * time.Hour
	leafRenewal  = 24 * time.Hour
)

// CA is the CA the egress proxy signs host certificates with, for the
// hosts a granted secret is bound to (docs/networking.md "Secrets").
type CA struct {
	// PEM is the certificate, as isolated VMs get it.
	PEM  []byte
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// LoadOrCreateCA is the CA in dir, made on first use. Two processes making
// it at once both end up with the one that won the rename.
func LoadOrCreateCA(dir string) (*CA, error) {
	if ca, err := loadCA(dir); ca != nil || err != nil {
		return ca, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	ski := sha1.Sum(pub)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: CACommonName},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          ski[:],
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp(parent, ".egress-ca-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	if err := os.WriteFile(filepath.Join(temp, "ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(temp, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(temp, dir); err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, "ca.pem")); statErr != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	ca, err := loadCA(dir)
	if err == nil && ca == nil {
		err = fmt.Errorf("%s has no CA", dir)
	}
	return ca, err
}

func loadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	// Never the parse error's text: it could quote the key file.
	broken := fmt.Errorf("cannot read the egress CA in %s; remove the directory to make a new one, then recreate isolated VMs", dir)
	if err != nil {
		return nil, broken
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, broken
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, broken
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, broken
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, broken
	}
	return &CA{PEM: certPEM, cert: cert, key: key}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// Leaf is a certificate for the DNS name host signed by the CA, with its key.
func (ca *CA) Leaf(host string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	cn := host
	if len(cn) > 64 {
		cn = cn[:64]
	}
	tmpl := &x509.Certificate{
		SerialNumber:   serial(),
		Subject:        pkix.Name{CommonName: cn},
		NotBefore:      now.Add(-time.Hour),
		NotAfter:       now.Add(LeafLifetime),
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:       []string{host},
		AuthorityKeyId: ca.cert.SubjectKeyId,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
