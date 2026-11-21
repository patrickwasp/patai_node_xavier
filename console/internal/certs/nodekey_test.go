package certs

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// The node key is made once, on the unit, kept at 0600, and read back as the same key; its pin is the SHA-256 of its
// SubjectPublicKeyInfo, which the unit's own certificate's pin is made the same way as.
func TestNodeKeyIsMadeOnceKeptAndPinnedByItsPublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-key.pem")
	n := &KeyFile{Path: path}
	if n.Existing() != nil {
		t.Fatal("a key before one was made")
	}
	key, err := n.Get()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	again := &KeyFile{Path: path}
	if k := again.Existing(); k == nil || !k.Equal(key) {
		t.Fatal("not the same key when read back")
	}
	if k, _ := (&KeyFile{Path: filepath.Join(t.TempDir(), "node-key.pem")}).Get(); k.Equal(key) {
		t.Fatal("two units, one key")
	}

	pin, err := Pin(key.Public())
	der, _ := x509.MarshalPKIXPublicKey(key.Public())
	sum := sha256.Sum256(der)
	if err != nil || pin != hex.EncodeToString(sum[:]) || len(pin) != 64 {
		t.Fatalf("%q %v", pin, err)
	}

	unit, _ := Load("", "", "patai-0001")
	unitPin, err := CertPin(unit)
	leaf, _ := x509.ParseCertificate(unit.Certificate[0])
	want, _ := Pin(leaf.PublicKey)
	if err != nil || unitPin != want || unitPin == pin {
		t.Fatalf("%q %q %v", unitPin, want, err)
	}

	// A file that isn't a P-256 key is a fault, not a reason to make another and lose the names the old one had.
	os.WriteFile(path, []byte("not a key"), 0o600)
	if _, err := (&KeyFile{Path: path}).Get(); err == nil {
		t.Fatal("replaced a damaged key")
	}
}
