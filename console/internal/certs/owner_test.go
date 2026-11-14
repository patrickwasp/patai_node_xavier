package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

type issued struct {
	cert *x509.Certificate
	key  any
	pem  []byte
}

func keyPEM(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// issue makes a certificate from template, signed by parent, or by itself with none.
func issue(t *testing.T, template *x509.Certificate, parent *issued, key any) issued {
	t.Helper()
	if key == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key = k
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template.SerialNumber = serial
	if template.NotBefore.IsZero() {
		template.NotBefore, template.NotAfter = now.Add(-24*time.Hour), now.Add(90*24*time.Hour)
	}
	signer, signerCert := key, template
	if parent != nil {
		signer, signerCert = parent.key, parent.cert
	}
	public := key.(crypto.Signer).Public()
	der, err := x509.CreateCertificate(rand.Reader, template, signerCert, public, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return issued{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func ca(t *testing.T, name string, parent *issued) issued {
	return issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: now.AddDate(-5, 0, 0), NotAfter: now.AddDate(5, 0, 0)}, parent, nil)
}

func server(names []string, ips ...net.IP) *x509.Certificate {
	return &x509.Certificate{Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, IPAddresses: ips,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
}

func chain(parts ...issued) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p.pem...)
	}
	return out
}

func TestAGoodChain(t *testing.T) {
	root := ca(t, "Lab root", nil)
	inter := ca(t, "Lab issuing", &root)
	leaf := issue(t, server([]string{"gpu1.lab.example", "patai.local", "patai"}, net.ParseIP("192.168.4.26")), &inter, nil)
	owner, cert, err := Check(chain(leaf, inter, root), keyPEM(t, leaf.key), "patai", now, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(owner.Names, " ") != "gpu1.lab.example" || strings.Join(owner.Unused, " ") != "patai.local patai 192.168.4.26" {
		t.Fatalf("%+v", owner)
	}
	// The root is the browser's to have.
	if owner.Chain != 2 || len(cert.Certificate) != 2 || owner.DatesUnsure || owner.Issuer != "CN=Lab issuing" {
		t.Fatalf("%+v", owner)
	}
	if owner.Fingerprint != fingerprint(leaf.cert.Raw) {
		t.Fatal("fingerprint")
	}
}

func TestWhatsRefused(t *testing.T) {
	root := ca(t, "Lab root", nil)
	other := ca(t, "Someone else", nil)
	leaf := issue(t, server([]string{"gpu1.lab.example"}), &root, nil)
	good := keyPEM(t, leaf.key)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	weak := issue(t, server([]string{"gpu1.lab.example"}), &root, rsaKey)
	noServerAuth := server([]string{"gpu1.lab.example"})
	noServerAuth.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	client := issue(t, noServerAuth, &root, nil)
	wild := issue(t, server([]string{"gpu1.lab.example", "*.lab.example"}), &root, nil)
	local := issue(t, server([]string{"patai.local"}, net.ParseIP("192.168.4.26")), &root, nil)
	old := server([]string{"gpu1.lab.example"})
	old.NotBefore, old.NotAfter = now.Add(-400*24*time.Hour), now.Add(-24*time.Hour)
	expired := issue(t, old, &root, nil)
	encrypted := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("x")})
	legacy := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: []byte("x")})

	cases := []struct {
		name       string
		chain, key []byte
		want       string
	}{
		{"another key", chain(leaf, root), keyPEM(t, client.key), "isn't the certificate's"},
		{"a CA first", chain(root), keyPEM(t, root.key), "CA's"},
		{"a weak key", chain(weak, root), keyPEM(t, rsaKey), "2048 to 4096"},
		{"not for servers", chain(client, root), keyPEM(t, client.key), "server authentication"},
		{"a wildcard", chain(wild, root), keyPEM(t, wild.key), "wildcard"},
		{"only the unit's own names", chain(local, root), keyPEM(t, local.key), "no DNS name"},
		{"out of date", chain(expired, root), keyPEM(t, expired.key), "ran out"},
		{"the wrong issuer", chain(leaf, other), good, "doesn't hold together"},
		{"a passphrase", chain(leaf, root), encrypted, "passphrase"},
		{"an old-style passphrase", chain(leaf, root), legacy, "passphrase"},
		{"not PEM", []byte("hello"), good, "isn't PEM"},
		{"a key among the certificates", append(chain(leaf, root), good...), good, "private key in it"},
		{"two keys", chain(leaf, root), append(good, good...), "more than one"},
		{"nothing", nil, good, "no certificate"},
	}
	for _, c := range cases {
		if _, _, err := Check(c.chain, c.key, "patai", now, true); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	var long []byte
	for range maxChain + 1 {
		long = append(long, root.pem...)
	}
	if _, _, err := Check(append(leaf.pem, long...), good, "patai", now, true); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("long chain: %v", err)
	}
}

func TestAClockThatMayBeWrongOnlyShowsTheDates(t *testing.T) {
	root := ca(t, "Lab root", nil)
	old := server([]string{"gpu1.lab.example"})
	old.NotBefore, old.NotAfter = now.Add(-400*24*time.Hour), now.Add(-24*time.Hour)
	expired := issue(t, old, &root, nil)
	owner, _, err := Check(chain(expired, root), keyPEM(t, expired.key), "patai", now, false)
	if err != nil || !owner.DatesUnsure {
		t.Fatalf("%+v %v", owner, err)
	}
	// The rest is still checked.
	short := issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: "Short root"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: now, NotAfter: now.AddDate(1, 0, 0)}, nil, nil)
	dated := issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: "gpu1.lab.example"}, DNSNames: []string{"gpu1.lab.example"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore: now.AddDate(-2, 0, 0), NotAfter: now.AddDate(-1, 0, 0)}, &short, nil)
	if _, _, err := Check(chain(dated, short), keyPEM(t, dated.key), "patai", now, false); err == nil ||
		!strings.Contains(err.Error(), "never overlap") {
		t.Fatalf("dates that never overlap: %v", err)
	}
	other := ca(t, "Someone else", nil)
	if _, _, err := Check(chain(expired, other), keyPEM(t, expired.key), "patai", now, false); err == nil {
		t.Fatal("a broken chain passed with the dates unchecked")
	}
}

func TestExtendedKeyUsage(t *testing.T) {
	root := ca(t, "Lab root", nil)
	bare := server([]string{"gpu1.lab.example"})
	bare.ExtKeyUsage = nil
	none := issue(t, bare, &root, nil)
	owner, _, err := Check(chain(none, root), keyPEM(t, none.key), "patai", now, true)
	if err != nil || len(owner.Unused) != 0 || owner.Unused == nil {
		t.Fatalf("none at all: %+v %v", owner, err)
	}
	anyUse := server([]string{"gpu1.lab.example"})
	anyUse.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	anyLeaf := issue(t, anyUse, &root, nil)
	if _, _, err := Check(chain(anyLeaf, root), keyPEM(t, anyLeaf.key), "patai", now, true); err != nil {
		t.Fatalf("any: %v", err)
	}
}

func TestAnUnknownCriticalExtensionAlone(t *testing.T) {
	root := ca(t, "Lab root", nil)
	odd := server([]string{"gpu1.lab.example"})
	odd.ExtraExtensions = []pkix.Extension{{Id: []int{1, 3, 6, 1, 4, 1, 99999, 1}, Critical: true, Value: []byte{5, 0}}}
	leaf := issue(t, odd, &root, nil)
	if _, _, err := Check(leaf.pem, keyPEM(t, leaf.key), "patai", now, true); err == nil ||
		!strings.Contains(err.Error(), "critical extension") {
		t.Fatalf("%v", err)
	}
}

func TestALeafOnItsOwn(t *testing.T) {
	root := ca(t, "Lab root", nil)
	leaf := issue(t, server([]string{"gpu1.lab.example"}), &root, nil)
	owner, _, err := Check(leaf.pem, keyPEM(t, leaf.key), "patai", now, true)
	if err != nil || owner.Chain != 1 {
		t.Fatalf("%+v %v", owner, err)
	}
	for _, key := range []any{leaf.key} {
		ec := key.(*ecdsa.PrivateKey)
		der, _ := x509.MarshalECPrivateKey(ec)
		sec1 := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		if _, _, err := Check(leaf.pem, sec1, "patai", now, true); err != nil {
			t.Fatalf("SEC1 key: %v", err)
		}
	}
}

func TestKeptThroughARestartAndRemoved(t *testing.T) {
	root := ca(t, "Lab root", nil)
	leaf := issue(t, server([]string{"gpu1.lab.example"}), &root, nil)
	path := filepath.Join(t.TempDir(), "owner-tls.pem")
	if owner, _, err := LoadOwner(path, "patai"); owner != nil || err != nil {
		t.Fatal("nothing kept should be nothing")
	}
	if err := SaveOwner(path, chain(leaf, root), keyPEM(t, leaf.key)); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	owner, cert, err := LoadOwner(path, "patai")
	if err != nil || owner.Names[0] != "gpu1.lab.example" || len(cert.Certificate) != 1 || !owner.DatesUnsure {
		t.Fatalf("%+v %v", owner, err)
	}
	if err := RemoveOwner(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwner(path); err != nil {
		t.Fatal("removing nothing")
	}
	os.WriteFile(path, []byte("garbage"), 0o600)
	if _, _, err := LoadOwner(path, "patai"); err == nil {
		t.Fatal("garbage loaded")
	}
}

// served is the leaf a client gets when it asks for name, or for nothing with "".
func served(t *testing.T, config *tls.Config, name string) *x509.Certificate {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{ServerName: name, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0]
}

// A session from before a change mustn't be resumed after it, through a listener that copied the configuration as
// http.Server does.
func TestNoSessionOutlivesAChange(t *testing.T) {
	unit, err := Load("", "", "patai")
	if err != nil {
		t.Fatal(err)
	}
	root := ca(t, "Lab root", nil)
	first := issue(t, server([]string{"gpu1.lab.example"}), &root, nil)
	second := issue(t, server([]string{"gpu1.lab.example"}), &root, nil)
	selector := NewSelector(unit)
	put := func(leaf issued) {
		owner, cert, err := Check(chain(leaf, root), keyPEM(t, leaf.key), "patai", now, true)
		if err != nil {
			t.Fatal(err)
		}
		selector.SetOwner(owner, &cert)
	}
	put(first)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", selector.Config().Clone())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := conn.(*tls.Conn)
				tc.Handshake()
				tc.Write([]byte("x"))
				conn.Close()
			}()
		}
	}()
	cache := tls.NewLRUClientSessionCache(4)
	dial := func() tls.ConnectionState {
		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{ServerName: "gpu1.lab.example",
			InsecureSkipVerify: true, ClientSessionCache: cache})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Read(make([]byte, 1)) // a TLS 1.3 ticket comes after the handshake
		return conn.ConnectionState()
	}
	dial()
	if !dial().DidResume {
		t.Fatal("the test can't tell: an unchanged session didn't resume")
	}
	put(second)
	state := dial()
	if state.DidResume || fingerprint(state.PeerCertificates[0].Raw) != fingerprint(second.cert.Raw) {
		t.Fatal("a session from before the replacement was resumed")
	}
	selector.SetOwner(nil, nil)
	state = dial()
	if state.DidResume || state.PeerCertificates[0].Subject.CommonName != "patai" {
		t.Fatal("a session from before the removal was resumed")
	}
}

func TestEachNameGetsItsCertificate(t *testing.T) {
	unit, err := Load("", "", "patai")
	if err != nil {
		t.Fatal(err)
	}
	root := ca(t, "Lab root", nil)
	leaf := issue(t, server([]string{"gpu1.lab.example", "gpu.lab.example"}), &root, nil)
	selector := NewSelector(unit)
	config := selector.Config()
	if got := served(t, config, "gpu1.lab.example"); got.Subject.CommonName != "patai" {
		t.Fatal("an owner's name before there's a certificate for it")
	}
	owner, cert, err := Check(chain(leaf, root), keyPEM(t, leaf.key), "patai", now, true)
	if err != nil {
		t.Fatal(err)
	}
	selector.SetOwner(owner, &cert)
	for name, want := range map[string]string{"gpu1.lab.example": "gpu1.lab.example", "GPU.lab.example.": "gpu1.lab.example",
		"patai.local": "patai", "": "patai", "attacker.example": "patai"} {
		if got := served(t, config, name); got.Subject.CommonName != want {
			t.Errorf("%q got %s", name, got.Subject.CommonName)
		}
	}
	if !selector.Serves("gpu.lab.example") || selector.Serves("patai.local") {
		t.Fatal("serves")
	}
	selector.SetOwner(nil, nil)
	if got := served(t, config, "gpu1.lab.example"); got.Subject.CommonName != "patai" || selector.Serves("gpu1.lab.example") {
		t.Fatal("still in use after removal")
	}
}
