package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"

	"patai/console/internal/durable"
)

// The node key (docs/console.md, Remote access, The node key): a second key, which the unit makes on itself the first
// time a way of reaching it needs one, for every certificate it gets by ACME. The unit's own key is made on the PC that
// flashes it, so its fingerprint can go on the card; this one nobody else ever had. It's kept on /persist, goes where
// the unit's own certificate goes in a restore, and a factory reset removes it.

// NodeKey is the unit's node key, made the first time it's asked for and kept at Path.
type NodeKey struct {
	Path string

	mu  sync.Mutex
	key *ecdsa.PrivateKey
}

// Get is the key, read from Path, or made and kept there if there's none yet.
func (n *NodeKey) Get() (*ecdsa.PrivateKey, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.key != nil {
		return n.key, nil
	}
	if key, err := readNodeKey(n.Path); err == nil {
		n.key = key
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// Written durably: a key lost in a power cut after a certificate was had for it would leave a name the unit can't
	// answer for.
	if err := durable.Write(n.Path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	n.key = key
	return key, nil
}

// Existing is the key if the unit has made one, and nil otherwise; it never makes one.
func (n *NodeKey) Existing() *ecdsa.PrivateKey {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.key == nil {
		if key, err := readNodeKey(n.Path); err == nil {
			n.key = key
		}
	}
	return n.key
}

func readNodeKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s holds no key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%s isn't a P-256 key", path)
	}
	return key, nil
}

// Pin is what a native app pins a key by: the SHA-256 of its SubjectPublicKeyInfo, as 64 lowercase hex digits. A
// renewed certificate keeps its key, so the pin holds through every renewal (patai_chat docs/clients.md).
func Pin(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// CertPin is the pin of a certificate's key, for the unit's own.
func CertPin(cert tls.Certificate) (string, error) {
	leaf := cert.Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return "", err
		}
	}
	return Pin(leaf.PublicKey)
}
