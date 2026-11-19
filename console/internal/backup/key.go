// Package backup is a disaster-recovery backup of a unit (docs/console.md, Backups): what makes it what it is, from
// /persist and the apps' folders on /data, as one file encrypted to a key only its owner keeps, and the bucket it goes
// to. patai-rootd gathers and writes it, and puts a restore in place; patai-api sends it and fetches it.
package backup

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"filippo.io/age"
)

// Key is what the unit keeps of its owner's backup key: the public half, which backups are encrypted to, and the key
// that seals them, so that nobody without either can make one a restore would take. Neither can read a backup.
type Key struct {
	Recipient string `json:"recipient"`
	ID        string `json:"id"`
	Seal      string `json:"seal"` // hex
	Made      string `json:"made,omitempty"`
}

// NewKey makes an owner's key: the secret, shown to them once and never kept, and what the unit keeps of it.
func NewKey() (string, Key, error) {
	identity, err := age.GenerateHybridIdentity()
	if err != nil {
		return "", Key{}, err
	}
	secret := identity.String()
	key, err := keyOf(identity, secret)
	return secret, key, err
}

// Parse reads an owner's key as they kept it, the line that starts AGE-SECRET-KEY-PQ-1, from text that may hold more,
// such as the file the console gave them.
func Parse(text string) (*age.HybridIdentity, Key, error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "AGE-SECRET-KEY-PQ-1") {
			continue
		}
		identity, err := age.ParseHybridIdentity(strings.ToUpper(line))
		if err != nil {
			return nil, Key{}, errors.New("that key is damaged: check it was copied whole")
		}
		key, err := keyOf(identity, identity.String())
		return identity, key, err
	}
	return nil, Key{}, errors.New("that isn't a backup key")
}

func keyOf(identity *age.HybridIdentity, secret string) (Key, error) {
	recipient := identity.Recipient().String()
	seal, err := hkdf.Key(sha256.New, []byte(secret), nil, "patai backup seal 1", 32)
	if err != nil {
		return Key{}, err
	}
	return Key{Recipient: recipient, ID: KeyID(recipient), Seal: hex.EncodeToString(seal)}, nil
}

// KeyID is a short name for a key, to tell one from another: the start of its public half's hash.
func KeyID(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	h := hex.EncodeToString(sum[:4])
	return h[:4] + "-" + h[4:]
}

// SealKey is the key that seals backups, from its hex.
func (k Key) SealKey() ([]byte, error) {
	seal, err := hex.DecodeString(k.Seal)
	if err != nil || len(seal) != 32 {
		return nil, errors.New("the backup key's seal is damaged")
	}
	return seal, nil
}

// Recipients is what a backup is encrypted to.
func (k Key) Recipients() (age.Recipient, error) {
	return age.ParseHybridRecipient(k.Recipient)
}

func mac(seal, data []byte) []byte {
	h := hmac.New(sha256.New, seal)
	h.Write(data)
	return h.Sum(nil)
}
