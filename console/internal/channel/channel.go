// Package channel signs and checks the latest.json a channel offers (docs/updates.md, Channels). RAUC checks a
// bundle's signature, but not which release it is, so without this a file server could offer any older signed
// release. The fleet key that signs bundles signs this too, and a unit refuses a file older than one it has seen.
package channel

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"patai/console/internal/updates"
)

// Format names the envelope, and starts what's signed, so a signature made for anything else can't pass as one.
const Format = "patai-latest/1"

// MaxSize is what patai-update reads from a channel.
const MaxSize = 64 * 1024

// The most a sequence can be and still be the same number in Python, JSON and Go.
const maxSequence = 1<<53 - 1

// The fleet key is RSA-4096 (docs/updates.md, Keys).
const minBits = 4096

// MaxBundle is the most a bundle can be. patai-update downloads it whole before installing it.
const MaxBundle = 4 << 30

// Payload is what a channel offers. It's signed as these exact bytes, never as reserialised JSON, and a field
// added later is ignored by a unit that doesn't know it, because it's signed too.
type Payload struct {
	Compatible   string `json:"compatible"`    // RAUC's compatible, so another product's file can't pass
	Channel      string `json:"channel"`       // the unit's update-channel-name has to match it
	Sequence     int64  `json:"sequence"`      // Unix seconds when it was signed; a unit never goes below one it saw
	Version      string `json:"version"`       // the release
	Bundle       string `json:"bundle"`        // relative to the channel, or a full http(s) address
	ManifestHash string `json:"manifest_hash"` // RAUC installs only the bundle with this manifest
	// Size is the bundle's, so a unit knows it has room for the download and stops a server that sends more.
	Size int64 `json:"size"`
}

// Offer is a payload that passed, for patai-update.
type Offer struct {
	Payload
	Digest string `json:"payload_sha256"` // tells two different files with the same sequence apart
	Signer string `json:"signer"`         // SHA-256 of the signing certificate
}

type envelope struct {
	Format      string `json:"format"`
	Payload     []byte `json:"payload"`
	Certificate []byte `json:"certificate"`
	Signature   []byte `json:"signature"`
}

var (
	word     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	relative = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+~-]{0,255}$`)
	hash     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validBundle(s string) bool {
	if !strings.Contains(s, "://") {
		for _, part := range strings.Split(s, "/") {
			if part == "" || part == "." || part == ".." {
				return false
			}
		}
		return relative.MatchString(s)
	}
	if len(s) > 1024 || strings.ContainsFunc(s, func(c rune) bool { return c <= ' ' || c > '~' }) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil &&
		u.RawQuery == "" && u.Fragment == ""
}

func (p Payload) check() error {
	switch {
	case !word.MatchString(p.Compatible):
		return errors.New("its compatible isn't one")
	case !updates.ValidChannelName(p.Channel):
		return errors.New("its channel isn't a name")
	case p.Sequence < 1 || p.Sequence > maxSequence:
		return errors.New("its sequence is out of range")
	case !updates.ValidVersion(p.Version):
		return errors.New("its version isn't one")
	case !validBundle(p.Bundle):
		return errors.New("its bundle isn't a plain path or http(s) address")
	case !hash.MatchString(p.ManifestHash):
		return errors.New("its manifest hash isn't one")
	case p.Size < 1 || p.Size > MaxBundle:
		return errors.New("its size is out of range")
	}
	return nil
}

func signed(payload []byte) []byte {
	return append([]byte(Format+"\n"), payload...)
}

// Sign makes a latest.json. The certificate has to be the key's.
func Sign(p Payload, key crypto.Signer, certDER []byte) ([]byte, error) {
	if err := p.check(); err != nil {
		return nil, fmt.Errorf("not signing it: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}
	if public, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !public.Equal(cert.PublicKey) {
		return nil, errors.New("the certificate isn't this key's")
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(signed(payload))
	signature, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(envelope{Format, payload, certDER, signature}, "", "  ")
	return append(out, '\n'), err
}

// Roots reads a keyring: the fleet CA's certificate, or more than one while it's being replaced.
func Roots(keyring []byte) ([]*x509.Certificate, error) {
	var roots []*x509.Certificate
	for {
		var block *pem.Block
		block, keyring = pem.Decode(keyring)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		roots = append(roots, cert)
	}
	if len(roots) == 0 {
		return nil, errors.New("the keyring has no certificate")
	}
	return roots, nil
}

// signer checks the signing certificate the way RAUC's check-purpose=codesign does, for the one arrangement the
// fleet has: a code-signing certificate issued by a CA in the keyring. Its dates aren't checked: a unit's clock
// can be years out, and RAUC still checks the bundle's.
func signer(certDER []byte, roots []*x509.Certificate) (*rsa.PublicKey, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, errors.New("its certificate can't be read")
	}
	issued := false
	for _, root := range roots {
		if bytes.Equal(root.RawSubject, cert.RawIssuer) && cert.CheckSignatureFrom(root) == nil {
			issued = true
			break
		}
	}
	codeSigning := false
	for _, use := range cert.ExtKeyUsage {
		codeSigning = codeSigning || use == x509.ExtKeyUsageCodeSigning
	}
	public, rsaKey := cert.PublicKey.(*rsa.PublicKey)
	switch {
	case !issued:
		return nil, errors.New("its certificate wasn't issued by the fleet CA")
	case cert.IsCA || cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 || !codeSigning:
		return nil, errors.New("its certificate isn't for code signing")
	case len(cert.UnhandledCriticalExtensions) > 0:
		return nil, errors.New("its certificate has a critical extension this unit doesn't know")
	case !rsaKey || public.N.BitLen() < minBits:
		return nil, errors.New("its certificate's key is too weak")
	}
	return public, nil
}

// Verify says what a latest.json offers, or why it isn't the fleet's. Whether it's this unit's channel, and newer
// than what the unit has seen, is patai-update's to decide.
func Verify(data []byte, roots []*x509.Certificate) (Offer, error) {
	var e envelope
	if len(data) > MaxSize || json.Unmarshal(data, &e) != nil {
		return Offer{}, errors.New("it isn't a signed latest.json")
	}
	if e.Format != Format {
		return Offer{}, fmt.Errorf("its format is %q, not %q", e.Format, Format)
	}
	public, err := signer(e.Certificate, roots)
	if err != nil {
		return Offer{}, err
	}
	digest := sha256.Sum256(signed(e.Payload))
	if rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], e.Signature) != nil {
		return Offer{}, errors.New("its signature doesn't match")
	}
	var p Payload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Offer{}, errors.New("what it signs can't be read")
	}
	if err := p.check(); err != nil {
		return Offer{}, err
	}
	payloadSum, certSum := sha256.Sum256(e.Payload), sha256.Sum256(e.Certificate)
	return Offer{p, hex.EncodeToString(payloadSum[:]), hex.EncodeToString(certSum[:])}, nil
}
