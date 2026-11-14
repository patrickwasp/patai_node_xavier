// Package auth holds the console's password, token and session handling.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// RFC 9106's second recommendation, with fewer threads for a unit that may be busy.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonLen     = 32
	saltLen      = 16
)

const MinPassword = 10
const MaxPassword = 256

var ErrWeakPassword = fmt.Errorf("a password needs at least %d characters", MinPassword)

func CheckStrength(password string) error {
	if len(password) < MinPassword || len(password) > MaxPassword {
		return ErrWeakPassword
	}
	return nil
}

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func HashPassword(password string) string {
	salt := random(saltLen)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key))
}

// CheckPassword reads its parameters from the hash, so raising them later doesn't lock anyone out.
func CheckPassword(encoded, password string) bool {
	var memory, time uint32
	var threads uint8
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash makes a login for a name that doesn't exist take as long as one that does.
var dummyHash = HashPassword("patai-no-such-user")

func BurnPasswordCheck(password string) { CheckPassword(dummyHash, password) }

// HashSecret is for what's already random: tokens and codes, where a slow hash adds nothing.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func NewToken() string { return "patai_" + base64.RawURLEncoding.EncodeToString(random(32)) }

// NewCode makes a code to read off a screen or a card: 100 bits, in groups of five, with no
// characters that look alike in base32.
func NewCode() string {
	s := base32.StdEncoding.EncodeToString(random(13))[:20]
	return s[:5] + "-" + s[5:10] + "-" + s[10:15] + "-" + s[15:]
}

// NormalizeCode forgives how a person types one.
func NormalizeCode(code string) string {
	code = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
	if len(code) != 20 {
		return code
	}
	return code[:5] + "-" + code[5:10] + "-" + code[10:15] + "-" + code[15:]
}

func SameCode(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var ErrThrottled = errors.New("too many attempts")
