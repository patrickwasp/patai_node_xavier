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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"patai/console/internal/rootd"
)

// A stand-in tailscaled on a unix socket, answering its local API as far as the console uses it, and refusing what
// tailscaled refuses: another Host, an Origin or a Referer.
type fakeTailscaled struct {
	mu      sync.Mutex
	state   string
	authURL string
	https   bool
	name    string
	started []map[string]any
	logins  int
	certs   int
	asked   int    // certificate requests, failed ones included
	fail    bool   // the next certificate requests fail, as Let's Encrypt can
	cert    []byte // the key and chain it gives, in one PEM
	leaf    []byte
	refused []string
}

func startTailscaled(t *testing.T) (*fakeTailscaled, string) {
	t.Helper()
	// A socket's path has to be short, which a test's own directory may not be.
	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTailscaled{state: "NeedsLogin", name: "patai-0001.tail1234.ts.net"}
	f.cert, f.leaf = selfSigned(t, f.name)
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f, sock
}

func selfSigned(t *testing.T, name string) (pair, leaf []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	pair = append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	return pair, der
}

func (f *fakeTailscaled) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Host != "local-tailscaled.sock" || r.Header.Get("Origin") != "" || r.Referer() != "" {
		f.refused = append(f.refused, r.URL.Path)
		http.Error(w, "invalid localapi request", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/localapi/v0/status":
		st := map[string]any{"Version": "1.104.7-patai", "BackendState": f.state, "AuthURL": f.authURL, "TUN": true,
			"TailscaleIPs": []string{}, "Health": []string{}}
		if f.state == "Running" {
			st["TailscaleIPs"] = []string{"100.101.102.103", "fd7a:115c:a1e0::1"}
			st["Self"] = map[string]any{"HostName": "patai-0001", "DNSName": f.name + ".", "Online": true}
			st["CurrentTailnet"] = map[string]any{"Name": "ada@example.com", "MagicDNSSuffix": "tail1234.ts.net", "MagicDNSEnabled": true}
			if f.https {
				st["CertDomains"] = []string{f.name}
			}
		}
		json.NewEncoder(w).Encode(st)
	case r.Method == "POST" && r.URL.Path == "/localapi/v0/start":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.started = append(f.started, body)
		if key, _ := body["AuthKey"].(string); key != "" {
			f.state = "Running"
		}
	case r.Method == "POST" && r.URL.Path == "/localapi/v0/login-interactive":
		f.logins++
		f.authURL = "https://login.tailscale.com/a/abc123"
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "POST" && r.URL.Path == "/localapi/v0/logout":
		f.state, f.authURL = "NeedsLogin", ""
	case r.Method == "GET" && r.URL.Path == "/localapi/v0/cert/"+f.name && r.URL.Query().Get("type") == "pair":
		f.asked++
		if !f.https || f.fail {
			http.Error(w, "your Tailscale account does not support getting TLS certs", http.StatusInternalServerError)
			return
		}
		f.certs++
		w.Write(f.cert)
	default:
		http.NotFound(w, r)
	}
}

func (r *rig) withTailscale(t *testing.T) (*fakeTailscaled, string) {
	t.Helper()
	f, sock := startTailscaled(t)
	binary := filepath.Join(r.dir, "tailscaled")
	os.WriteFile(binary, nil, 0o755)
	r.s.TailscaleSocket, r.s.TailscaleBinary = sock, binary
	r.s.TailscaleOnPath = filepath.Join(r.dir, "tailscale-on")
	r.s.TailscalePath = filepath.Join(r.dir, "tailscale.json")
	return f, r.s.TailscaleOnPath
}

func tailscaleOf(t *testing.T, r *rig, cookie string) tailscaleJSON {
	t.Helper()
	w := r.do("GET", "/api/v1/tailscale", nil, opts{cookie: cookie})
	want(t, w, 200)
	var out tailscaleJSON
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// An owner turns Tailscale on, joins their tailnet at a login URL or with an auth key, gets the tailnet's certificate by
// itself once the tailnet has HTTPS on, can leave it unused, sends the logs or not, logs out and turns it off: each with
// a fresh password, each audited without the key, and every call to tailscaled as tailscaled takes it.
func TestTailscaleIsTheOwnersToTurnOnAndJoin(t *testing.T) {
	r := newRig(t)
	ts, on := r.withTailscale(t)
	owner, _ := r.claim()
	viewer := r.user(owner, "bo", "viewer")

	if got := tailscaleOf(t, r, owner); !got.Available || got.On || got.State != "off" {
		t.Fatalf("%+v", got)
	}
	want(t, r.do("GET", "/api/v1/tailscale", nil, opts{cookie: viewer}), 403)
	want(t, r.do("POST", "/api/v1/tailscale/on", map[string]string{"current_password": "wrong"}, opts{cookie: owner}), 403)
	if len(r.rootd.called("tailscale-on")) != 0 {
		t.Fatal("turned on without the password")
	}
	r.rootd.respond = func(req rootd.Request) (string, bool) {
		if req.Verb == "tailscale-on" {
			os.WriteFile(on, nil, 0o644)
		}
		return "", false
	}
	want(t, r.do("POST", "/api/v1/tailscale/on", map[string]string{"current_password": password}, opts{cookie: owner}), 200)
	if calls := r.rootd.called("tailscale-on"); len(calls) != 1 || calls[0].Target != "" {
		t.Fatalf("%+v", calls)
	}
	if got := tailscaleOf(t, r, owner); !got.On || got.State != "NeedsLogin" {
		t.Fatalf("%+v", got)
	}

	// A login at a URL: the unit's own DNS and routes, the console as operator, nothing updating itself.
	login := func(body map[string]string) int {
		body["current_password"] = password
		return r.do("POST", "/api/v1/tailscale/login", body, opts{cookie: owner}).Code
	}
	if code := login(map[string]string{"control_url": "http://headscale.example.com"}); code != 400 {
		t.Fatal(code)
	}
	if code := login(map[string]string{"auth_key": "tskey auth"}); code != 400 {
		t.Fatal(code)
	}
	if code := login(map[string]string{}); code != 200 {
		t.Fatal(code)
	}
	ts.mu.Lock()
	started := ts.started[len(ts.started)-1]
	ts.mu.Unlock()
	prefs, _ := json.Marshal(started["UpdatePrefs"])
	for _, s := range []string{`"CorpDNS":false`, `"RouteAll":false`, `"WantRunning":true`, `"NetfilterMode":1`,
		`"OperatorUser":"patai-api"`, `"ControlURL":""`, `"AutoUpdate":{"Apply":false,"Check":false}`} {
		if !strings.Contains(string(prefs), s) {
			t.Fatalf("%s lacks %s", prefs, s)
		}
	}
	if got := tailscaleOf(t, r, owner); got.LoginURL != "https://login.tailscale.com/a/abc123" || ts.logins != 1 {
		t.Fatalf("%+v", got)
	}

	// An auth key joins at once, and isn't kept or logged.
	if code := login(map[string]string{"auth_key": "tskey-auth-secret123", "control_url": "https://headscale.example.com/"}); code != 200 {
		t.Fatal(code)
	}
	ts.mu.Lock()
	started = ts.started[len(ts.started)-1]
	ts.mu.Unlock()
	if started["AuthKey"] != "tskey-auth-secret123" || !strings.Contains(string(must(json.Marshal(started["UpdatePrefs"]))), `"ControlURL":"https://headscale.example.com"`) {
		t.Fatalf("%v", started)
	}
	audits, _ := r.s.Store.AuditList(50, 0)
	logged := ""
	for _, a := range audits {
		logged += a.Action + " " + a.Target + "\n"
	}
	if strings.Contains(logged, "secret123") || !strings.Contains(logged, "tailscale-login with an auth key at https://headscale.example.com") {
		t.Fatal(logged)
	}
	data, _ := os.ReadFile(r.s.TailscalePath)
	if strings.Contains(string(data), "secret123") {
		t.Fatal("the key was kept")
	}

	// Running: the state is flushed once, and the unit shows as the tailnet knows it.
	ctx := context.Background()
	r.s.lookAtTailnet(ctx, false)
	r.s.lookAtTailnet(ctx, false)
	if calls := r.rootd.called("tailscale-sync"); len(calls) != 1 {
		t.Fatalf("%+v", calls)
	}
	got := tailscaleOf(t, r, owner)
	if got.State != "Running" || got.Name != "patai-0001.tail1234.ts.net" || got.Tailnet != "ada@example.com" ||
		len(got.Addresses) != 2 || got.HTTPS || !got.MagicDNS {
		t.Fatalf("%+v", got)
	}

	// The certificate is wanted with nothing turned on, but not had until the tailnet has HTTPS on, and then only at
	// the tailnet name.
	settings := func(body map[string]any) int {
		body["current_password"] = password
		return r.do("PUT", "/api/v1/tailscale/settings", body, opts{cookie: owner}).Code
	}
	if got := tailscaleOf(t, r, owner); !got.Certificate.On || got.Certificate.InUse ||
		!strings.Contains(got.Certificate.Problem, "HTTPS") || r.s.TLS.Serves(ts.name) || r.s.hostAllowed(ts.name) {
		t.Fatalf("%+v", got)
	}
	if ts.asked != 0 {
		t.Fatal("asked for a certificate the tailnet can't give")
	}
	kicked := func() bool {
		select {
		case <-r.s.tailnetKick():
			return true
		default:
			return false
		}
	}
	if kicked() {
		t.Fatal("woke the watch with nothing to do")
	}
	// HTTPS turned on in the tailnet's admin console: the page's next look wakes the watch, and the watch gets it.
	ts.mu.Lock()
	ts.https, ts.fail = true, true
	ts.mu.Unlock()
	tailscaleOf(t, r, owner)
	if !kicked() {
		t.Fatal("the page didn't wake the watch")
	}
	// A failure isn't asked again at once, however often the page looks.
	r.s.lookAtTailnet(ctx, false)
	r.s.lookAtTailnet(ctx, false)
	if got := tailscaleOf(t, r, owner); ts.asked != 1 || got.Certificate.InUse || !strings.Contains(got.Certificate.Problem, "couldn't be had") {
		t.Fatalf("%d %+v", ts.asked, got)
	}
	ts.mu.Lock()
	ts.fail = false
	ts.mu.Unlock()
	r.s.tailnet.mu.Lock()
	r.s.tailnet.failedAt = time.Now().Add(-tailnetRetry)
	r.s.tailnet.mu.Unlock()
	r.s.lookAtTailnet(ctx, false)
	kicked() // the looks while it was failing
	if got := tailscaleOf(t, r, owner); !got.HTTPS || !got.Certificate.InUse || got.Certificate.Problem != "" {
		t.Fatalf("%+v", got)
	}
	if kicked() {
		t.Fatal("woke the watch with the certificate in use")
	}
	if !r.s.hostAllowed(ts.name+":9443") || r.s.hostAllowed("evil.tail1234.ts.net") {
		t.Fatal("the console's names")
	}
	conf, _ := r.s.TLS.Config().GetConfigForClient(&tls.ClientHelloInfo{})
	for name, tailnet := range map[string]bool{ts.name: true, "patai-0001.local": false} {
		cert, _ := conf.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
		if (string(cert.Certificate[0]) == string(ts.leaf)) != tailnet {
			t.Fatalf("%s got the wrong certificate", name)
		}
	}
	// Asked again only when it's due.
	r.s.lookAtTailnet(ctx, false)
	if ts.certs != 1 {
		t.Fatal(ts.certs)
	}

	// Don't use a certificate: the name goes, and comes back when it's wanted again.
	if code := settings(map[string]any{"certificate": false}); code != 200 {
		t.Fatal(code)
	}
	if got := tailscaleOf(t, r, owner); got.Certificate.On || got.Certificate.InUse || r.s.TLS.Serves(ts.name) {
		t.Fatalf("%+v", got)
	}
	if data, _ := os.ReadFile(r.s.TailscalePath); !strings.Contains(string(data), `"no_certificate":true`) {
		t.Fatalf("%s", data)
	}
	if code := settings(map[string]any{"certificate": true}); code != 200 {
		t.Fatal(code)
	}
	if got := tailscaleOf(t, r, owner); !got.Certificate.InUse || !r.s.TLS.Serves(ts.name) {
		t.Fatalf("%+v", got)
	}

	// Logs to Tailscale start it again with them.
	if code := settings(map[string]any{"logs": true}); code != 200 {
		t.Fatal(code)
	}
	if calls := r.rootd.called("tailscale-on"); len(calls) != 2 || calls[1].Target != "logs" {
		t.Fatalf("%+v", calls)
	}

	// Logging out ends the name; turning it off asks root.
	want(t, r.do("POST", "/api/v1/tailscale/logout", map[string]string{"current_password": password}, opts{cookie: owner}), 200)
	if r.s.TLS.Serves(ts.name) {
		t.Fatal("still answering at the tailnet name")
	}
	want(t, r.do("POST", "/api/v1/tailscale/off", map[string]string{"current_password": password}, opts{cookie: owner}), 200)
	if len(r.rootd.called("tailscale-off")) != 1 {
		t.Fatal("not turned off")
	}
	if len(ts.refused) != 0 {
		t.Fatal(ts.refused)
	}
}

func must(data []byte, _ error) []byte { return data }

// A release without Tailscale says so, and turns nothing on.
func TestTailscaleWithoutTheBinary(t *testing.T) {
	r := newRig(t)
	r.withTailscale(t)
	r.s.TailscaleBinary = filepath.Join(r.dir, "none")
	owner, _ := r.claim()
	if got := tailscaleOf(t, r, owner); got.Available || got.On {
		t.Fatalf("%+v", got)
	}
	want(t, r.do("POST", "/api/v1/tailscale/on", map[string]string{"current_password": password}, opts{cookie: owner}), 404)
}
