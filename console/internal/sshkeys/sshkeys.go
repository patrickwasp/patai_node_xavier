// Package sshkeys manages the owner's SSH keys for the `admin` account, in the one file sshd reads
// (overlay/etc/ssh/sshd_config.d). The file is the truth, so a rollback sees the same keys.
package sshkeys

import (
	"bufio"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const File = "/persist/provision/authorized_keys"

const maxComment = 100

var (
	ErrBad      = errors.New("that isn't an SSH public key this unit accepts")
	ErrNotFound = errors.New("no such key")
)

// Key is one line of the file, for showing to a person. ID is safe in a URL, which a fingerprint isn't.
type Key struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment"`
}

func describe(pub ssh.PublicKey, comment string) Key {
	sum := sha256.Sum256(pub.Marshal())
	return Key{ID: hex.EncodeToString(sum[:]), Type: pub.Type(), Fingerprint: ssh.FingerprintSHA256(pub), Comment: comment}
}

// Parse checks a key someone gave and returns it with the line to store. Options are refused, because
// one like command= would have sshd do something the owner can't see here.
func Parse(line string) (Key, string, error) {
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil || len(options) > 0 || len(rest) > 0 {
		return Key{}, "", ErrBad
	}
	if !allowed(pub) {
		return Key{}, "", ErrBad
	}
	if len(comment) > maxComment {
		comment = comment[:maxComment]
	}
	comment = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, comment)
	stored := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		stored += " " + comment
	}
	return describe(pub, comment), stored, nil
}

func allowed(pub ssh.PublicKey) bool {
	switch pub.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
		return true
	case ssh.KeyAlgoRSA:
		crypto, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return false
		}
		key, ok := crypto.CryptoPublicKey().(*rsa.PublicKey)
		return ok && key.N.BitLen() >= 2048
	}
	return false
}

// lines reads the file as it stands. One missing is a unit with no keys.
func lines(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}
	return all, scanner.Err()
}

// List shows every key in the file, including provisioned ones with options.
func List(path string) ([]Key, error) {
	all, err := lines(path)
	if err != nil {
		return nil, err
	}
	keys := []Key{}
	for _, line := range all {
		if pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
			keys = append(keys, describe(pub, comment))
		}
	}
	return keys, nil
}

// Add appends a key. Adding one that's already there changes nothing.
func Add(path, line string) (Key, error) {
	key, stored, err := Parse(line)
	if err != nil {
		return Key{}, err
	}
	all, err := lines(path)
	if err != nil {
		return Key{}, err
	}
	for _, existing := range all {
		if pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(existing)); err == nil && describe(pub, "").ID == key.ID {
			return key, nil
		}
	}
	return key, write(path, append(all, stored))
}

// Remove takes out one key and leaves every other line as it was.
func Remove(path, id string) error {
	all, err := lines(path)
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(all))
	for _, line := range all {
		if pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil && describe(pub, "").ID == id {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == len(all) {
		return ErrNotFound
	}
	return write(path, kept)
}

func write(path string, all []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	content := ""
	if len(all) > 0 {
		content = strings.Join(all, "\n") + "\n"
	}
	if _, err = f.WriteString(content); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
