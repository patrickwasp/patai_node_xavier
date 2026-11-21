// Package sealing keeps what the console holds to sign in somewhere outside, such as where backups go, sealed with a
// key made on the unit that never leaves it: it isn't in a backup, a restore keeps the unit's own, and a factory reset
// takes it away with what it sealed (docs/console.md, Backups). A copy of the file a secret is in, without the key,
// holds nothing of it.
package sealing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"patai/console/internal/durable"
)

// Name is the key's file, beside what it seals.
const Name = "sealing-key"

const prefix = "sealed1."

// Key seals and opens secrets: AES-256-GCM, each bound to what it's for, so one can't be moved into another's place.
type Key struct{ aead cipher.AEAD }

// Open reads the key at path, or makes it there, durably, readable by this account alone, if there's none yet.
func Open(path string) (*Key, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		raw = make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		if err := durable.Write(path, raw, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%s isn't a sealing key", path)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Key{aead: aead}, nil
}

// Seal is a secret for purpose, as text to keep. Nothing sealed is nothing.
func (k *Key) Seal(purpose, secret string) string {
	if secret == "" {
		return ""
	}
	nonce := make([]byte, k.aead.NonceSize())
	rand.Read(nonce)
	sealed := k.aead.Seal(nonce, nonce, []byte(secret), []byte(purpose))
	return prefix + base64.RawURLEncoding.EncodeToString(sealed)
}

// ErrOpen is a secret this key didn't seal for that purpose: another unit's, or changed.
var ErrOpen = errors.New("that secret wasn't sealed with this unit's key")

// Unseal is what Seal sealed for purpose.
func (k *Key) Unseal(purpose, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil || !strings.HasPrefix(sealed, prefix) || len(data) < k.aead.NonceSize() {
		return "", ErrOpen
	}
	nonce, body := data[:k.aead.NonceSize()], data[k.aead.NonceSize():]
	secret, err := k.aead.Open(nil, nonce, body, []byte(purpose))
	if err != nil {
		return "", ErrOpen
	}
	return string(secret), nil
}
