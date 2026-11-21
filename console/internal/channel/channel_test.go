package channel

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"patai/console/internal/formats"
)

// Keys are slow to make, so each test shares them: one RSA-4096 for every signing certificate that's meant to pass
// the size check, and 2048 for the rest.
var (
	once                    sync.Once
	strong, weak, caKey     *rsa.PrivateKey
	otherKey, intermediates *rsa.PrivateKey
)

func keys(t *testing.T) {
	t.Helper()
	once.Do(func() {
		var err error
		for _, k := range []struct {
			key  **rsa.PrivateKey
			bits int
		}{{&strong, 4096}, {&weak, 2048}, {&caKey, 2048}, {&otherKey, 2048}, {&intermediates, 2048}} {
			if *k.key, err = rsa.GenerateKey(rand.Reader, k.bits); err != nil {
				panic(err)
			}
		}
	})
}

var serial int64

func issue(t *testing.T, tmpl *x509.Certificate, parent *x509.Certificate, public crypto.PublicKey, signer crypto.Signer) *x509.Certificate {
	t.Helper()
	serial++
	tmpl.SerialNumber = big.NewInt(serial)
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	}
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, public, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func ca(t *testing.T, cn string, key *rsa.PrivateKey) *x509.Certificate {
	return issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: cn}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}, nil, &key.PublicKey, key)
}

func leaf(t *testing.T, issuer *x509.Certificate, issuerKey *rsa.PrivateKey, key *rsa.PrivateKey, change func(*x509.Certificate)) *x509.Certificate {
	tmpl := &x509.Certificate{Subject: pkix.Name{CommonName: "signing"}, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	if change != nil {
		change(tmpl)
	}
	return issue(t, tmpl, issuer, &key.PublicKey, issuerKey)
}

func payload() Payload {
	return Payload{Compatible: "patai-node-xavier", Channel: "stable", Sequence: 1762200000, Version: "2026.11.03-1",
		Bundle: "2026.11.03-1/patai-2026.11.03-1.raucb", ManifestHash: strings.Repeat("ab", 32), Size: 908794456,
		Data: formats.Release{"chat-server": {Title: "Chat server", Format: &formats.Format{Version: 2, Floor: 1}},
			"chat-postgres": {Cluster: "18"}}}
}

// raw signs bytes as they are, so a test can sign what Sign refuses to.
func raw(t *testing.T, payload []byte, key *rsa.PrivateKey, cert *x509.Certificate) []byte {
	t.Helper()
	digest := sha256.Sum256(signed(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(envelope{Format, payload, cert.Raw, signature})
	return out
}

func TestSignAndVerify(t *testing.T) {
	keys(t)
	root := ca(t, "fleet", caKey)
	cert := leaf(t, root, caKey, strong, nil)
	data, err := Sign(payload(), strong, cert.Raw)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := Verify(data, []*x509.Certificate{root})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(offer.Payload, payload()) || len(offer.Digest) != 64 || len(offer.Signer) != 64 {
		t.Fatalf("got %+v", offer)
	}
	// What's signed is what's in the file, byte for byte.
	var e envelope
	json.Unmarshal(data, &e)
	sum := sha256.Sum256(e.Payload)
	if offer.Digest != hex.EncodeToString(sum[:]) {
		t.Fatal("the digest isn't of the payload's bytes")
	}
}

func TestTampering(t *testing.T) {
	keys(t)
	root := ca(t, "fleet", caKey)
	cert := leaf(t, root, caKey, strong, nil)
	other := leaf(t, root, caKey, strong, func(c *x509.Certificate) { c.Subject.CommonName = "another" })
	data, _ := Sign(payload(), strong, cert.Raw)
	var good envelope
	json.Unmarshal(data, &good)

	changes := map[string]func(*envelope){
		"payload": func(e *envelope) {
			e.Payload = []byte(strings.Replace(string(e.Payload), "2026.11.03-1", "2026.10.31-1", 1))
		},
		"signature":       func(e *envelope) { e.Signature[10] ^= 1 },
		"format":          func(e *envelope) { e.Format = "patai-latest/2" },
		"no format":       func(e *envelope) { e.Format = "" },
		"short signature": func(e *envelope) { e.Signature = e.Signature[:100] },
	}
	for what, change := range changes {
		e := good
		e.Payload = append([]byte(nil), good.Payload...)
		e.Signature = append([]byte(nil), good.Signature...)
		change(&e)
		out, _ := json.Marshal(e)
		if _, err := Verify(out, []*x509.Certificate{root}); err == nil {
			t.Errorf("%s: a changed file passed", what)
		}
	}

	// The certificate isn't signed, so another fleet certificate for the same key passes: the signature is what counts.
	e := good
	e.Certificate = other.Raw
	out, _ := json.Marshal(e)
	if _, err := Verify(out, []*x509.Certificate{root}); err != nil {
		t.Errorf("the same key in another fleet certificate: %v", err)
	}

	for _, body := range []string{"", "not json", "[]", "null", `{"format": "patai-latest/1"}`,
		`{"format": "patai-latest/1", "payload": "%%%", "certificate": "", "signature": ""}`,
		`{"version": "2026.11.03-1", "bundle": "x.raucb"}`, strings.Repeat(" ", MaxSize) + string(data)} {
		if _, err := Verify([]byte(body), []*x509.Certificate{root}); err == nil {
			t.Errorf("%.60q passed", body)
		}
	}
}

func TestCertificates(t *testing.T) {
	keys(t)
	root := ca(t, "fleet", caKey)
	otherCA := ca(t, "someone else", otherKey)
	middle := issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: "intermediate"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}, root, &intermediates.PublicKey, caKey)
	unknown := pkix.Extension{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1}, Critical: true, Value: []byte{5, 0}}

	cases := []struct {
		what string
		key  *rsa.PrivateKey
		cert *x509.Certificate
		ok   bool
	}{
		{"the fleet's", strong, leaf(t, root, caKey, strong, nil), true},
		{"expired, since dates aren't checked", strong, leaf(t, root, caKey, strong, func(c *x509.Certificate) {
			c.NotBefore, c.NotAfter = time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)
		}), true},
		{"another CA's code signing", strong, leaf(t, otherCA, otherKey, strong, nil), false},
		{"the CA itself", caKey, root, false},
		{"through an intermediate", strong, leaf(t, middle, intermediates, strong, nil), false},
		{"no extended key usage", strong, leaf(t, root, caKey, strong, func(c *x509.Certificate) { c.ExtKeyUsage = nil }), false},
		{"S/MIME only", strong, leaf(t, root, caKey, strong, func(c *x509.Certificate) {
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection}
		}), false},
		{"any purpose", strong, leaf(t, root, caKey, strong, func(c *x509.Certificate) {
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
		}), false},
		{"no digital signature", strong, leaf(t, root, caKey, strong, func(c *x509.Certificate) {
			c.KeyUsage = x509.KeyUsageKeyEncipherment
		}), false},
		{"a CA", strong, leaf(t, root, caKey, strong, func(c *x509.Certificate) {
			c.IsCA, c.KeyUsage = true, c.KeyUsage|x509.KeyUsageCertSign
		}), false},
		{"a 2048-bit key", weak, leaf(t, root, caKey, weak, nil), false},
		{"an unknown critical extension", strong, leaf(t, root, caKey, strong, func(c *x509.Certificate) {
			c.ExtraExtensions = []pkix.Extension{unknown}
		}), false},
	}
	for _, c := range cases {
		data, err := Sign(payload(), c.key, c.cert.Raw)
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		_, err = Verify(data, []*x509.Certificate{root})
		if (err == nil) != c.ok {
			t.Errorf("%s: got %v", c.what, err)
		}
	}
}

func TestPayloads(t *testing.T) {
	keys(t)
	root := ca(t, "fleet", caKey)
	cert := leaf(t, root, caKey, strong, nil)
	base, _ := json.Marshal(payload())
	edit := func(field, value string) []byte {
		var m map[string]json.RawMessage
		json.Unmarshal(base, &m)
		m[field] = json.RawMessage(value)
		out, _ := json.Marshal(m)
		return out
	}
	bad := map[string][]byte{
		"sequence 0":        edit("sequence", "0"),
		"negative":          edit("sequence", "-1"),
		"past 2^53":         edit("sequence", "9007199254740992"),
		"a fraction":        edit("sequence", "1762200000.5"),
		"a string sequence": edit("sequence", `"1762200000"`),
		"a boolean":         edit("sequence", "true"),
		"no version":        edit("version", `""`),
		"an option":         edit("version", `"-rf"`),
		"a space":           edit("version", `"a b"`),
		"a parent":          edit("bundle", `"../x.raucb"`),
		"absolute":          edit("bundle", `"/x.raucb"`),
		"empty segment":     edit("bundle", `"a//x.raucb"`),
		"a file address":    edit("bundle", `"file:///etc/shadow"`),
		"a password":        edit("bundle", `"https://user:pw@example.com/x.raucb"`),
		"a space in it":     edit("bundle", `"https://example.com/a b.raucb"`),
		"a query":           edit("bundle", `"https://example.com/x.raucb?a=b"`),
		"upper-case hash":   edit("manifest_hash", `"`+strings.Repeat("AB", 32)+`"`),
		"short hash":        edit("manifest_hash", `"abcd"`),
		"a capital channel": edit("channel", `"Stable"`),
		"no compatible":     edit("compatible", `""`),
		"not an object":     []byte(`["2026.11.03-1"]`),
		"not json":          []byte(`version=2026.11.03-1`),
		"no manifest hash":  []byte(`{"compatible":"patai-node-xavier","channel":"stable","sequence":1,"version":"v","bundle":"x"}`),
		"a negative size":   edit("size", "-1"),
		"too big a bundle":  edit("size", "4294967297"),
		"a string size":     edit("size", `"908794456"`),
		"a fractional size": edit("size", "1.5"),
	}
	for what, p := range bad {
		if _, err := Verify(raw(t, p, strong, cert), []*x509.Certificate{root}); err == nil {
			t.Errorf("%s: passed", what)
		}
	}
	// A field a later release adds is signed too, so a unit that doesn't know it carries on.
	if _, err := Verify(raw(t, edit("workloads", `"https://example.com/workloads/"`), strong, cert), []*x509.Certificate{root}); err != nil {
		t.Errorf("an added field: %v", err)
	}
	// A unit has to know it has room for the bundle.
	if _, err := Verify(raw(t, edit("size", "0"), strong, cert), []*x509.Certificate{root}); err == nil {
		t.Error("no size: passed")
	}
	sized := payload()
	sized.Size = 908794456
	data, err := Sign(sized, strong, cert.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if offer, err := Verify(data, []*x509.Certificate{root}); err != nil || offer.Size != 908794456 {
		t.Errorf("a size: %v, %d", err, offer.Size)
	}
	full := payload()
	full.Bundle = "https://files.example.com/2026.11.03-1/x.raucb"
	if _, err := Verify(raw(t, mustJSON(full), strong, cert), []*x509.Certificate{root}); err != nil {
		t.Errorf("a full address: %v", err)
	}
	invalid := payload()
	invalid.Sequence = 0
	if _, err := Sign(invalid, strong, cert.Raw); err == nil {
		t.Error("Sign signed a bad payload")
	}
}

func mustJSON(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}

func TestRootsAndKeys(t *testing.T) {
	keys(t)
	root, next := ca(t, "fleet", caKey), ca(t, "next fleet", otherKey)
	keyring := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: next.Raw})...)
	roots, err := Roots(keyring)
	if err != nil || len(roots) != 2 {
		t.Fatalf("got %d roots, %v", len(roots), err)
	}
	// While the CA is being replaced, either one's signer passes.
	data, _ := Sign(payload(), strong, leaf(t, next, otherKey, strong, nil).Raw)
	if _, err := Verify(data, roots); err != nil {
		t.Errorf("the second root: %v", err)
	}
	if _, err := Roots([]byte("no certificates here")); err == nil {
		t.Error("an empty keyring")
	}
	if _, err := Sign(payload(), weak, leaf(t, root, caKey, strong, nil).Raw); err == nil {
		t.Error("Sign used a certificate that isn't the key's")
	}
}

func TestDataIsChecked(t *testing.T) {
	keys(t)
	root := ca(t, "fleet", caKey)
	cert := leaf(t, root, caKey, strong, nil)
	bad := payload()
	bad.Data = formats.Release{"chat-server": {Format: &formats.Format{Version: 1, Floor: 2}}}
	if _, err := Sign(bad, strong, cert.Raw); err == nil {
		t.Fatal("signed a floor above its version")
	}
	// A release that declares nothing of its data signs none.
	old := payload()
	old.Data = nil
	data, err := Sign(old, strong, cert.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if offer, err := Verify(data, []*x509.Certificate{root}); err != nil || offer.Data != nil {
		t.Fatalf("%+v %v", offer, err)
	}
}
