// Package certs makes the unit's own self-signed certificate (docs/console.md, HTTPS on a LAN), and serves an
// owner's beside it. The unit's is kept for the life of the unit, so the fingerprint on the screen and the card
// never changes.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"time"

	"patai/console/internal/durable"
)

// Fixed, so making it doesn't depend on the unit's clock.
var (
	notBefore = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter  = time.Date(2055, 1, 1, 0, 0, 0, 0, time.UTC)
)

// Load reads the unit's certificate, or makes one if there's none yet. With an empty certFile nothing
// is kept: recovery mode has nowhere to keep it.
func Load(certFile, keyFile, hostname string) (tls.Certificate, error) {
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err == nil {
			return cert, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return tls.Certificate{}, err
		}
	}
	certPEM, keyPEM, err := Make(hostname)
	if err != nil {
		return tls.Certificate{}, err
	}
	if certFile != "" {
		// An empty one after a power cut would never be replaced, and the fingerprint would change at
		// each boot. A reset leaves this to the first boot after it.
		if err := durable.Write(keyFile, keyPEM, 0o600); err != nil {
			return tls.Certificate{}, err
		}
		if err := durable.Write(certFile, certPEM, 0o644); err != nil {
			return tls.Certificate{}, err
		}
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func Make(hostname string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hostname},
		DNSNames:              []string{hostname, hostname + ".local"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// Fingerprint is the SHA-256 of the certificate, written the way browsers show it.
func Fingerprint(cert tls.Certificate) string { return fingerprint(cert.Certificate[0]) }
