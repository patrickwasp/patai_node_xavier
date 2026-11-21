package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/challenge"

	"patai/console/internal/certs"
)

// A stand-in CA: it signs whatever name it's asked for with the node key, as Let's Encrypt would once DNS-01 passed.
type fakeIssuer struct {
	mu       sync.Mutex
	nodeKey  *certs.KeyFile
	caKey    *ecdsa.PrivateKey
	ca       *x509.Certificate
	checks   []string
	obtained []string
	previous [][]byte
	revoked  int
	checkErr error
	fail     error
	renewAt  time.Time // zero: two thirds of the way through
}

func newFakeIssuer(t *testing.T, nodeKey *certs.KeyFile) *fakeIssuer {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &fakeIssuer{nodeKey: nodeKey, caKey: key, ca: ca}
}

func (f *fakeIssuer) Check(_ context.Context, name string, dns challenge.Provider) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, name)
	return f.checkErr
}

func (f *fakeIssuer) Obtain(_ context.Context, name string, _ challenge.Provider, previous []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.obtained = append(f.obtained, name)
	f.previous = append(f.previous, previous)
	if f.fail != nil {
		return nil, f.fail
	}
	key, err := f.nodeKey.Get()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(len(f.obtained) + 1)), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.ca, &key.PublicKey, f.caKey)
	if err != nil {
		return nil, err
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw})...), nil
}

func (f *fakeIssuer) RenewAt(_ context.Context, chain []byte) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.renewAt.IsZero() {
		return f.renewAt
	}
	leaf, _ := x509.ParseCertificate(mustBlock(chain))
	return leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
}

func (f *fakeIssuer) Revoke(context.Context, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked++
	return nil
}

func mustBlock(chain []byte) []byte {
	block, _ := pem.Decode(chain)
	return block.Bytes
}

func (r *rig) withDomain(t *testing.T) *fakeIssuer {
	t.Helper()
	r.s.DomainPath = filepath.Join(r.dir, "domain.json")
	f := newFakeIssuer(t, r.s.NodeKey)
	r.s.Issuer = f
	if r.rootd.outputs == nil {
		r.rootd.outputs = map[string]string{}
	}
	r.rootd.outputs["clock-status"] = `{"synced":true}`
	r.rootd.outputs["network-status"] = `{"ports":[{"id":"gbe1","addresses":["10.4.4.50/24"]}]}`
	return f
}

func domainOf(t *testing.T, r *rig, cookie string) domainJSON {
	t.Helper()
	w := r.do("GET", "/api/v1/domain", nil, opts{cookie: cookie})
	want(t, w, 200)
	if strings.Contains(w.Body.String(), "cf-secret-token") {
		t.Fatal("the token came back")
	}
	var out domainJSON
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// An owner gives a name, a DNS provider and its token: the token is checked by writing a record, kept on the unit and
// never shown, logged or audited; the unit gets a certificate for the name with its node key once its clock can be
// trusted, answers to the name with it, renews it under ARI when it's due, backs off when the CA says no, and stops,
// forgets and revokes when the owner removes it. Each change takes a fresh password.
func TestOwnDomainGetsAndRenewsItsCertificate(t *testing.T) {
	r := newRig(t)
	ca := r.withDomain(t)
	owner, _ := r.claim()
	viewer := r.user(owner, "bo", "viewer")
	ctx := context.Background()

	got := domainOf(t, r, owner)
	if !got.Available || got.State != "off" || len(got.Providers) != 12 {
		t.Fatalf("%+v", got)
	}
	want(t, r.do("GET", "/api/v1/domain", nil, opts{cookie: viewer}), 403)

	set := func(body map[string]any) int {
		if _, ok := body["current_password"]; !ok {
			body["current_password"] = password
		}
		return r.do("PUT", "/api/v1/domain", body, opts{cookie: owner}).Code
	}
	good := func() map[string]any {
		return map[string]any{"name": "Patai.Example.com.", "provider": "cloudflare", "agree": true,
			"credentials": map[string]string{"token": "cf-secret-token"}}
	}
	for name, body := range map[string]map[string]any{
		"wrong password": {"current_password": "wrong"},
		"an address":     {"name": "10.4.4.50"},
		"a .local name":  {"name": "patai.local"},
		"a wildcard":     {"name": "*.example.com"},
		"one label":      {"name": "patai"},
		"a tailnet name": {"name": "patai.tail1234.ts.net"},
		"patai.ca's":     {"name": "abc.node.patai.ca"},
		"not agreed":     {"agree": false},
		"no token":       {"credentials": map[string]string{}},
		"no provider":    {"provider": "nowhere"},
	} {
		b := good()
		for k, v := range body {
			b[k] = v
		}
		if code := set(b); code != 400 && code != 403 {
			t.Fatalf("%s: %d", name, code)
		}
	}
	ca.checkErr = errors.New("cloudflare: 403 Forbidden")
	if code := set(good()); code != 400 {
		t.Fatal(code)
	}
	ca.checkErr = nil
	if _, err := os.Stat(r.s.DomainPath); err == nil {
		t.Fatal("kept a name whose check failed")
	}
	if len(ca.obtained) != 0 {
		t.Fatal("asked for a certificate before any check passed")
	}

	if code := set(good()); code != 200 {
		t.Fatal(code)
	}
	if got := domainOf(t, r, owner); got.Name != "patai.example.com" || got.Provider != "cloudflare" || got.State != "getting" ||
		len(got.Credentials) != 1 || !got.Credentials[0].Set || len(got.Addresses) != 1 {
		t.Fatalf("%+v", got)
	}
	creds := filepath.Join(r.dir, "domain-credentials.json")
	if info, err := os.Stat(creds); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	if data, _ := os.ReadFile(creds); !strings.Contains(string(data), "cf-secret-token") {
		t.Fatal("the token wasn't kept")
	}
	audits, _ := r.s.Store.AuditList(50, 0)
	logged := ""
	for _, a := range audits {
		logged += a.Action + " " + a.Target + "\n"
	}
	if strings.Contains(logged, "cf-secret-token") || !strings.Contains(logged, "domain-set patai.example.com at cloudflare") {
		t.Fatal(logged)
	}

	// Not with a clock it can't trust, then with one.
	r.rootd.outputs["clock-status"] = `{"synced":false,"set":null}`
	r.s.lookAtDomain(ctx)
	if got := domainOf(t, r, owner); got.State != "getting" || got.Waiting == "" || len(ca.obtained) != 0 {
		t.Fatalf("%+v", got)
	}
	r.rootd.outputs["clock-status"] = `{"synced":true}`
	r.s.lookAtDomain(ctx)
	got = domainOf(t, r, owner)
	if got.State != "in_use" || got.NotAfter == nil || got.RenewAt == nil || got.Problem != "" || got.Waiting != "" ||
		len(ca.obtained) != 1 || ca.previous[0] != nil {
		t.Fatalf("%+v %d", got, len(ca.obtained))
	}
	// The name, with the node key's certificate, and the console answers to it; other names don't change.
	nodeKey, _ := r.s.NodeKey.Get()
	conf, _ := r.s.TLS.Config().GetConfigForClient(&tls.ClientHelloInfo{})
	cert, _ := conf.GetCertificate(&tls.ClientHelloInfo{ServerName: "patai.example.com"})
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if !leaf.PublicKey.(*ecdsa.PublicKey).Equal(nodeKey.Public()) || len(cert.Certificate) != 2 {
		t.Fatal("not the node key's certificate")
	}
	if other, _ := conf.GetCertificate(&tls.ClientHelloInfo{ServerName: "patai-0001.local"}); string(other.Certificate[0]) == string(cert.Certificate[0]) {
		t.Fatal("the unit's own name got the domain's certificate")
	}
	if !r.s.hostAllowed("patai.example.com:9443") || r.s.hostAllowed("evil.example.com") {
		t.Fatal("the console's names")
	}

	// Not again until it's due; then a renewal that names the one it replaces.
	r.s.lookAtDomain(ctx)
	if len(ca.obtained) != 1 {
		t.Fatal(len(ca.obtained))
	}
	r.s.domain.mu.Lock()
	r.s.domain.renewAt = time.Now().Add(-time.Minute)
	r.s.domain.mu.Unlock()
	r.s.lookAtDomain(ctx)
	if len(ca.obtained) != 2 || ca.previous[1] == nil {
		t.Fatalf("%d %v", len(ca.obtained), ca.previous)
	}

	// A refusal keeps the certificate in use, says why, and isn't tried again at once.
	ca.fail = errors.New("acme: error: 429 :: too many certificates")
	r.s.domain.mu.Lock()
	r.s.domain.renewAt = time.Now().Add(-time.Minute)
	r.s.domain.mu.Unlock()
	if wait := r.s.lookAtDomain(ctx); wait < domainRetry-time.Second {
		t.Fatal(wait)
	}
	r.s.lookAtDomain(ctx)
	got = domainOf(t, r, owner)
	if len(ca.obtained) != 3 || got.State != "in_use" || !strings.Contains(got.Problem, "too many") || got.FailingSince == nil || got.Warn {
		t.Fatalf("%d %+v", len(ca.obtained), got)
	}
	// A week of it, and the Status page says so.
	r.s.domain.mu.Lock()
	r.s.domain.failingSince = time.Now().Add(-domainWarn)
	r.s.domain.mu.Unlock()
	if got := domainOf(t, r, owner); !got.Warn {
		t.Fatalf("%+v", got)
	}
	ca.fail = nil
	r.s.domain.mu.Lock()
	r.s.domain.nextTry = time.Time{}
	r.s.domain.mu.Unlock()
	r.s.lookAtDomain(ctx)
	if got := domainOf(t, r, owner); got.Problem != "" || got.FailingSince != nil || got.Warn {
		t.Fatalf("%+v", got)
	}

	// After a restart, the kept certificate is put in use without asking the CA.
	again := newRig(t)
	again.dir, again.s.DomainPath = r.dir, r.s.DomainPath
	again.s.NodeKey = &certs.KeyFile{Path: r.s.NodeKey.Path}
	again.s.Issuer = ca
	again.rootd.outputs = map[string]string{"clock-status": `{"synced":true}`}
	asked := len(ca.obtained)
	again.s.lookAtDomain(ctx)
	if !again.s.TLS.Serves("patai.example.com") || len(ca.obtained) != asked {
		t.Fatal("the kept certificate wasn't used")
	}

	// Another name, the same provider: the token is kept, and the old name stops being answered.
	if code := set(map[string]any{"name": "chat.example.com", "provider": "cloudflare", "agree": true}); code != 200 {
		t.Fatal(code)
	}
	if r.s.TLS.Serves("patai.example.com") {
		t.Fatal("still answering at the old name")
	}
	r.s.lookAtDomain(ctx)
	if !r.s.TLS.Serves("chat.example.com") || ca.obtained[len(ca.obtained)-1] != "chat.example.com" {
		t.Fatal("the new name")
	}

	// Removing it: a fresh password, then nothing kept, nothing answered, and the certificate revoked.
	want(t, r.do("DELETE", "/api/v1/domain", map[string]string{"current_password": "wrong"}, opts{cookie: owner}), 403)
	want(t, r.do("DELETE", "/api/v1/domain", map[string]string{"current_password": password}, opts{cookie: owner}), 200)
	for _, name := range []string{"domain.json", "domain-credentials.json", "domain-cert.pem"} {
		if _, err := os.Stat(filepath.Join(r.dir, name)); err == nil {
			t.Fatalf("%s is still there", name)
		}
	}
	if r.s.TLS.Serves("chat.example.com") || domainOf(t, r, owner).State != "off" {
		t.Fatal("still in use")
	}
	for i := 0; i < 100; i++ {
		ca.mu.Lock()
		revoked := ca.revoked
		ca.mu.Unlock()
		if revoked == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ca.revoked != 1 {
		t.Fatal("not revoked")
	}
}

// A kept certificate is used only if it's for the name, has the node key and hasn't run out.
func TestOwnDomainCertificateMustBeTheNodeKeys(t *testing.T) {
	dir := t.TempDir()
	nodeKey := &certs.KeyFile{Path: filepath.Join(dir, "node-key.pem")}
	key, _ := nodeKey.Get()
	ca := newFakeIssuer(t, nodeKey)
	chain, _ := ca.Obtain(context.Background(), "patai.example.com", nil, nil)
	if _, _, ok := domainCertificate(chain, "patai.example.com", key); !ok {
		t.Fatal("refused its own")
	}
	if _, _, ok := domainCertificate(chain, "other.example.com", key); ok {
		t.Fatal("another name")
	}
	other, _ := (&certs.KeyFile{Path: filepath.Join(dir, "other.pem")}).Get()
	if _, _, ok := domainCertificate(chain, "patai.example.com", other); ok {
		t.Fatal("another key")
	}
	if _, _, ok := domainCertificate(nil, "patai.example.com", key); ok {
		t.Fatal("nothing")
	}
}
