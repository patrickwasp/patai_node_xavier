package certs

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"patai/console/internal/durable"
)

// An owner's certificate is used only at its own names, and an address or <name>.local always gets the unit's
// own one, whose fingerprint is on the screen and the card, so that it's never the only way in (docs/console.md,
// HTTPS on a LAN).

const maxChain = 8

// Owner is what the console shows about an owner's certificate. The key never leaves the unit.
type Owner struct {
	Names       []string  `json:"names"`  // where it's served
	Unused      []string  `json:"unused"` // in the certificate, but the unit's own certificate answers there
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Fingerprint string    `json:"fingerprint"`  // of the leaf, the way browsers show it
	Chain       int       `json:"chain"`        // certificates served, the leaf included
	DatesUnsure bool      `json:"dates_unsure"` // the unit's clock may be wrong, so its dates weren't checked
}

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ownName is a name this certificate can have to itself: a dotted DNS name that isn't the unit's own or under .local,
// which the unit's certificate keeps.
func ownName(name, hostname string) bool {
	if len(name) > 253 || !strings.Contains(name, ".") || strings.HasSuffix(name, ".local") || name == strings.ToLower(hostname) {
		return false
	}
	if net.ParseIP(name) != nil {
		return false
	}
	for _, part := range strings.Split(name, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}

// Check reads a certificate chain and its key as an owner gives them, and says what's wrong in words for them.
// clockTrusted is false when the unit's clock may be wrong: then the dates are shown, not checked.
func Check(chainPEM, keyPEM []byte, hostname string, now time.Time, clockTrusted bool) (*Owner, tls.Certificate, error) {
	none := tls.Certificate{}
	chain, err := readChain(chainPEM)
	if err != nil {
		return nil, none, err
	}
	key, err := readKey(keyPEM)
	if err != nil {
		return nil, none, err
	}
	leaf := chain[0]
	if leaf.IsCA {
		return nil, none, errors.New("the first certificate is a CA's: give this unit's own certificate first, then the ones that issued it")
	}
	if !publicMatches(leaf, key) {
		return nil, none, errors.New("that key isn't the certificate's")
	}
	if err := strongEnough(leaf); err != nil {
		return nil, none, err
	}
	if leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, none, errors.New("the certificate isn't allowed to sign, which a TLS server needs")
	}
	if !hasServerAuth(leaf) {
		return nil, none, errors.New("the certificate isn't for TLS servers: its extended key usage leaves out server authentication")
	}
	// A browser has to refuse what it doesn't understand there, and a chain of one isn't verified below.
	for _, c := range chain {
		if len(c.UnhandledCriticalExtensions) > 0 {
			return nil, none, fmt.Errorf("a certificate has a critical extension browsers won't know, %v", c.UnhandledCriticalExtensions[0])
		}
	}

	owner := &Owner{Names: []string{}, Unused: []string{}, Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(),
		NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter, Fingerprint: fingerprint(leaf.Raw)}
	for _, name := range leaf.DNSNames {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		switch {
		case strings.Contains(name, "*"):
			return nil, none, errors.New("wildcard certificates can't be used: this unit's storage isn't encrypted, so a stolen unit would give up a key for every name under it")
		case ownName(name, hostname):
			owner.Names = append(owner.Names, name)
		default:
			owner.Unused = append(owner.Unused, name)
		}
	}
	for _, ip := range leaf.IPAddresses {
		owner.Unused = append(owner.Unused, ip.String())
	}
	if len(owner.Names) == 0 {
		return nil, none, errors.New("the certificate has no DNS name the unit can use it for. Addresses and .local names always use the unit's own certificate")
	}

	// Dates are checked by the unit's clock only when that's known to be right. Otherwise anything inside every
	// certificate's dates will do, so that the rest is still checked.
	at := now
	if !clockTrusted {
		owner.DatesUnsure = true
		from, until := leaf.NotBefore, leaf.NotAfter
		for _, c := range chain {
			if c.NotBefore.After(from) {
				from = c.NotBefore
			}
			if c.NotAfter.Before(until) {
				until = c.NotAfter
			}
		}
		if !from.Before(until) {
			return nil, none, errors.New("the certificates' dates never overlap, so the chain is never valid")
		}
		at = from.Add(until.Sub(from) / 2)
	} else if now.Before(leaf.NotBefore) {
		return nil, none, fmt.Errorf("the certificate isn't valid until %s", leaf.NotBefore.UTC().Format(time.RFC3339))
	} else if !now.Before(leaf.NotAfter) {
		return nil, none, fmt.Errorf("the certificate ran out on %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	}

	// What's served: the leaf and what issued it, without a root at the end, which a browser has or doesn't.
	served := chain
	if len(chain) > 1 && selfSigned(chain[len(chain)-1]) {
		served = chain[:len(chain)-1]
	}
	if len(chain) > 1 {
		roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
		roots.AddCert(chain[len(chain)-1])
		for _, c := range chain[1 : len(chain)-1] {
			intermediates.AddCert(c)
		}
		_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: at,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		if err != nil {
			return nil, none, fmt.Errorf("the chain doesn't hold together: %v", err)
		}
	}
	owner.Chain = len(served)

	cert := tls.Certificate{PrivateKey: key, Leaf: leaf}
	for _, c := range served {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	return owner, cert, nil
}

func readChain(data []byte) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	rest := bytes.TrimSpace(data)
	for len(rest) > 0 {
		block, more := pem.Decode(rest)
		if block == nil {
			return nil, errors.New("the certificate file isn't PEM: it should start with -----BEGIN CERTIFICATE-----")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the certificate file has a %s in it: give only certificates there", strings.ToLower(block.Type))
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a certificate couldn't be read: %v", err)
		}
		chain = append(chain, c)
		if len(chain) > maxChain {
			return nil, fmt.Errorf("that's more than %d certificates", maxChain)
		}
		rest = bytes.TrimSpace(more)
	}
	if len(chain) == 0 {
		return nil, errors.New("there's no certificate in that file")
	}
	return chain, nil
}

func readKey(data []byte) (any, error) {
	block, rest := pem.Decode(bytes.TrimSpace(data))
	if block == nil {
		return nil, errors.New("the key file isn't PEM: it should start with -----BEGIN PRIVATE KEY-----")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("the key file has more than one thing in it: give only the key")
	}
	if strings.Contains(block.Type, "ENCRYPTED") || block.Headers["Proc-Type"] != "" {
		return nil, errors.New("the key has a passphrase. Give it without one, for example with: openssl pkey -in key.pem -out plain.pem")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("the key file has a %s, not a private key", strings.ToLower(block.Type))
	}
	if err != nil {
		return nil, fmt.Errorf("the key couldn't be read: %v", err)
	}
	return key, nil
}

func publicMatches(leaf *x509.Certificate, key any) bool {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return false
	}
	public, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	return ok && public.Equal(signer.Public())
}

// strongEnough is what every browser takes: RSA of 2048 to 4096 bits, or ECDSA on P-256 or P-384.
func strongEnough(leaf *x509.Certificate) error {
	switch public := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		if bits := public.N.BitLen(); bits < 2048 || bits > 4096 {
			return fmt.Errorf("an RSA key has to be 2048 to 4096 bits, and this one is %d", bits)
		}
	case *ecdsa.PublicKey:
		if public.Curve != elliptic.P256() && public.Curve != elliptic.P384() {
			return errors.New("an ECDSA key has to be on P-256 or P-384")
		}
	default:
		return errors.New("the key has to be RSA or ECDSA: browsers don't take others for TLS")
	}
	return nil
}

// hasServerAuth is what x509.Verify takes for a server: no extended key usage at all, server authentication, or any.
func hasServerAuth(leaf *x509.Certificate) bool {
	if len(leaf.ExtKeyUsage) == 0 && len(leaf.UnknownExtKeyUsage) == 0 {
		return true
	}
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func selfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignatureFrom(c) == nil
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	encoded := strings.ToUpper(hex.EncodeToString(sum[:]))
	groups := make([]string, 0, len(sum))
	for i := 0; i < len(encoded); i += 2 {
		groups = append(groups, encoded[i:i+2])
	}
	return strings.Join(groups, ":")
}

// SaveOwner keeps the chain and key in one file, so that a power cut leaves the old pair or the new one. Only
// patai-api can read it.
func SaveOwner(path string, chainPEM, keyPEM []byte) error {
	data := append(append(bytes.TrimSpace(chainPEM), '\n'), append(bytes.TrimSpace(keyPEM), '\n')...)
	return durable.Write(path, data, 0o600)
}

// RemoveOwner forgets it for good: a removal that came back after a power cut would put the certificate back.
func RemoveOwner(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return durable.SyncDir(filepath.Dir(path))
}

// LoadOwner reads what SaveOwner kept, and checks it again, with the dates only shown: the clock at boot may be
// anything. No file is no certificate, and not an error.
func LoadOwner(path, hostname string) (*Owner, tls.Certificate, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, tls.Certificate{}, nil
	}
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	var chain, key []byte
	rest := data
	for {
		block, more := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			chain = append(chain, pem.EncodeToMemory(block)...)
		} else {
			key = append(key, pem.EncodeToMemory(block)...)
		}
		rest = more
	}
	return Check(chain, key, hostname, time.Time{}, false)
}

// Selector picks the certificate for each connection, by the name the client asked for. It's shared by every
// listener, and a change is seen by the next handshake.
type Selector struct {
	mu      sync.RWMutex
	unit    tls.Certificate
	owner   *tls.Certificate
	about   *Owner
	fault   string
	names   map[string]bool
	tailnet *tls.Certificate // the tailnet's, at its one name, while Tailscale is on and the owner uses it
	tsName  string
	domain  *tls.Certificate // the unit's own, by ACME, at the owner's own domain (docs/console.md, Remote access)
	dName   string
	config  *tls.Config
	current *tls.Config // this generation's, with its own session ticket keys
}

func NewSelector(unit tls.Certificate) *Selector {
	s := &Selector{unit: unit, names: map[string]bool{}}
	s.config = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: s.forClient}
	s.renew()
	return s
}

// Config is the TLS configuration for every listener. A listener copies it, so each handshake asks for the
// current generation's instead: a session made before a change can't be resumed after it.
func (s *Selector) Config() *tls.Config { return s.config }

func (s *Selector) forClient(*tls.ClientHelloInfo) (*tls.Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current, nil
}

// renew starts a generation. It's called with the lock held, or before anyone has the selector.
func (s *Selector) renew() {
	c := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.get, NextProtos: []string{"h2", "http/1.1"}}
	var key [32]byte
	if _, err := rand.Read(key[:]); err == nil {
		c.SetSessionTicketKeys([][32]byte{key})
	}
	s.current = c
}

func (s *Selector) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if s.owner != nil && s.names[name] {
		return s.owner, nil
	}
	if s.tailnet != nil && name == s.tsName {
		return s.tailnet, nil
	}
	if s.domain != nil && name == s.dName {
		return s.domain, nil
	}
	return &s.unit, nil
}

// SetOwner puts a certificate in use, or takes it out of use with nil. Sessions from before can't be resumed:
// they were made with what's being replaced.
func (s *Selector) SetOwner(about *Owner, cert *tls.Certificate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner, s.about, s.fault = cert, about, ""
	s.names = map[string]bool{}
	if about != nil {
		for _, name := range about.Names {
			s.names[name] = true
		}
	}
	s.renew()
}

// Fault says why a kept certificate isn't in use, until one is put in use or removed.
func (s *Selector) Fault(why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = why
}

// Owner is the certificate in use at the owner's names, if there is one, and why a kept one isn't.
func (s *Selector) Owner() (*Owner, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.about, s.fault
}

// Serves says whether name is one of the owner's, the unit's tailnet name or its own domain while their certificate is in use, which the
// console then answers to.
func (s *Selector) Serves(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names[name] || s.tailnet != nil && name == s.tsName || s.domain != nil && name == s.dName
}

// SetDomain puts the certificate the unit got for itself by ACME in use at the owner's domain, or takes it out of use
// with nil. Only the domain's owner can point that name at the unit and let it get a certificate there. A renewal at
// the same name keeps sessions; a new name or none starts afresh.
func (s *Selector) SetDomain(name string, cert *tls.Certificate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if cert == nil {
		name = ""
	}
	same := name == s.dName && (cert == nil) == (s.domain == nil)
	s.domain, s.dName = cert, name
	if !same {
		s.renew()
	}
}

// SetTailnet puts the tailnet's certificate in use at the unit's tailnet name (docs/console.md, Remote access with
// Tailscale), or takes it out of use with nil. Only Tailscale's control server can point that name at the unit, as
// only an owner's CA can name theirs. A renewal at the same name keeps sessions; a new name or none starts afresh.
func (s *Selector) SetTailnet(name string, cert *tls.Certificate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if cert == nil {
		name = ""
	}
	same := name == s.tsName && (cert == nil) == (s.tailnet == nil)
	s.tailnet, s.tsName = cert, name
	if !same {
		s.renew()
	}
}
