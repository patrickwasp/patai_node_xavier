package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"patai/console/internal/durable"
	"patai/console/internal/rootd"
	"patai/console/internal/tailnet"
)

// Remote access with Tailscale (docs/console.md): an owner's choice, off in every image. Turned on, the unit joins the
// owner's own tailnet, by a login at a URL the owner opens or an auth key from their tailnet, and is reached there by
// its tailnet name, with a certificate from Let's Encrypt through Tailscale once the tailnet has HTTPS on. Wasupu has
// no tailnet and is on none of the owners'. Each change takes a fresh password, as network settings do: a token or the
// cloud never makes one.

// The console's account, which tailscaled lets change its settings as its operator.
const tailscaleOperator = "patai-api"

// How long before the tailnet's certificate ends the console asks for it again; tailscaled renews it when asked within
// its last third.
const tailnetRenew = 30 * 24 * time.Hour

// tailscaleSettings are the owner's, kept in the console's own state: whether the tailnet's certificate is used, and
// whether tailscaled's logs go to Tailscale, which they don't unless the owner says so.
type tailscaleSettings struct {
	Certificate bool `json:"certificate"`
	Logs        bool `json:"logs"`
}

// tailnetWatch is what the console knows between looks at tailscaled.
type tailnetWatch struct {
	mu       sync.Mutex
	syncing  bool      // a login is under way, so its state is synced once it's running
	name     string    // the name the certificate in use is for
	notAfter time.Time // when it ends
	problem  string    // why the certificate isn't in use, in words for the owner
}

func (s *Server) tailscaleClient() *tailnet.Client { return &tailnet.Client{Socket: s.TailscaleSocket} }

func (s *Server) tailscaleSettings() tailscaleSettings {
	var t tailscaleSettings
	if data, err := os.ReadFile(s.TailscalePath); err == nil {
		json.Unmarshal(data, &t)
	}
	return t
}

func (s *Server) saveTailscaleSettings(t tailscaleSettings) error {
	data, _ := json.Marshal(t)
	return durable.Write(s.TailscalePath, data, 0o600)
}

// tailscaleOn is whether the owner has turned it on, which patai-rootd marks.
func (s *Server) tailscaleOn() bool {
	_, err := os.Stat(s.TailscaleOnPath)
	return err == nil
}

func (s *Server) tailscaleAvailable() bool {
	_, err := os.Stat(s.TailscaleBinary)
	return err == nil && s.TailscalePath != ""
}

type tailscaleJSON struct {
	Available   bool       `json:"available"`
	On          bool       `json:"on"`
	State       string     `json:"state"` // tailscaled's, or "starting" while it doesn't answer
	LoginURL    string     `json:"login_url,omitempty"`
	Tailnet     string     `json:"tailnet,omitempty"`
	Name        string     `json:"name,omitempty"`
	Addresses   []string   `json:"addresses"`
	KeyExpiry   *time.Time `json:"key_expiry,omitempty"`
	Expired     bool       `json:"expired,omitempty"`
	Health      []string   `json:"health"`
	HTTPS       bool       `json:"https"` // the tailnet gives this unit's name a certificate
	Certificate struct {
		On       bool       `json:"on"`
		InUse    bool       `json:"in_use"`
		NotAfter *time.Time `json:"not_after,omitempty"`
		Problem  string     `json:"problem,omitempty"`
	} `json:"certificate"`
	Logs    bool   `json:"logs"`
	Version string `json:"version,omitempty"`
}

func (s *Server) getTailscale(w http.ResponseWriter, r *http.Request, p *Principal) {
	out := tailscaleJSON{Available: s.tailscaleAvailable(), Addresses: []string{}, Health: []string{}}
	settings := s.tailscaleSettings()
	out.Logs, out.Certificate.On = settings.Logs, settings.Certificate
	out.On = out.Available && s.tailscaleOn()
	if !out.On {
		out.State = "off"
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := s.tailscaleClient().Status(ctx)
	if err != nil {
		out.State = "starting"
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.State, out.LoginURL, out.Version = st.BackendState, st.AuthURL, st.Version
	out.Addresses = append(out.Addresses, st.TailscaleIPs...)
	out.Health = append(out.Health, st.Health...)
	if st.CurrentTailnet != nil {
		out.Tailnet = st.CurrentTailnet.Name
	}
	out.Name = st.Name()
	if st.Self != nil {
		out.KeyExpiry, out.Expired = st.Self.KeyExpiry, st.Self.Expired
	}
	out.HTTPS = out.Name != "" && slices.Contains(st.CertDomains, out.Name)
	s.tailnet.mu.Lock()
	if s.tailnet.name != "" && s.tailnet.name == out.Name {
		out.Certificate.InUse = true
		at := s.tailnet.notAfter
		out.Certificate.NotAfter = &at
	}
	out.Certificate.Problem = s.tailnet.problem
	s.tailnet.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// tailscaleChange reads a change's body, with its fresh password, and refuses one this release can't make.
func (s *Server) tailscaleChange(w http.ResponseWriter, r *http.Request, p *Principal, body any, password func() string) bool {
	if !s.tailscaleAvailable() {
		fail(w, http.StatusNotFound, "no-tailscale", "this release has no Tailscale")
		return false
	}
	return readJSON(w, r, body) && s.confirmPassword(w, p, password())
}

func (s *Server) tailscaleAudit(w http.ResponseWriter, r *http.Request, p *Principal, action, target string) bool {
	if err := s.Store.AddAudit(s.audit(r, p.Name, action, target, "ok")); err != nil {
		s.failStore(w, err)
		return false
	}
	return true
}

func (s *Server) tailscaleTurnOn(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !s.tailscaleChange(w, r, p, &body, func() string { return body.CurrentPassword }) {
		return
	}
	if !s.tailscaleAudit(w, r, p, "tailscale-on", "") {
		return
	}
	if _, ok := s.ask(w, r, rootd.Request{Verb: "tailscale-on", Target: logsTarget(s.tailscaleSettings().Logs)}); !ok {
		return
	}
	s.getTailscale(w, r, p)
}

func logsTarget(logs bool) string {
	if logs {
		return "logs"
	}
	return ""
}

func (s *Server) tailscaleTurnOff(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !s.tailscaleChange(w, r, p, &body, func() string { return body.CurrentPassword }) {
		return
	}
	if !s.tailscaleAudit(w, r, p, "tailscale-off", "") {
		return
	}
	if _, ok := s.ask(w, r, rootd.Request{Verb: "tailscale-off"}); !ok {
		return
	}
	s.dropTailnetCertificate("")
	s.getTailscale(w, r, p)
}

// controlURL is a control server an owner names, such as their own Headscale's: https, with nothing after the host but
// a path. Empty is Tailscale's.
func controlURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("a control server is an https address, such as https://headscale.example.com")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func (s *Server) tailscaleLogin(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		AuthKey         string `json:"auth_key"`
		ControlURL      string `json:"control_url"`
	}
	if !s.tailscaleChange(w, r, p, &body, func() string { return body.CurrentPassword }) {
		return
	}
	if !s.tailscaleOn() {
		fail(w, http.StatusConflict, "tailscale-off", "turn Tailscale on first")
		return
	}
	control, err := controlURL(body.ControlURL)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-control-url", err.Error())
		return
	}
	key := strings.TrimSpace(body.AuthKey)
	if len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
		fail(w, http.StatusBadRequest, "bad-auth-key", "that isn't an auth key")
		return
	}
	// What's logged is how, never the key.
	how := "with a login"
	if key != "" {
		how = "with an auth key"
	}
	if control != "" {
		how += " at " + control
	}
	if !s.tailscaleAudit(w, r, p, "tailscale-login", how) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client := s.tailscaleClient()
	s.tailnet.mu.Lock()
	s.tailnet.syncing = true
	s.tailnet.mu.Unlock()
	err = client.Start(ctx, tailnet.Login{ControlURL: control, AuthKey: key}, tailscaleOperator)
	if err == nil && key == "" {
		err = client.LoginInteractive(ctx)
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "tailscale", tailscaleProblem(err))
		return
	}
	// The login URL comes a moment later: the page asks again until it's there.
	s.getTailscale(w, r, p)
}

func (s *Server) tailscaleLogout(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !s.tailscaleChange(w, r, p, &body, func() string { return body.CurrentPassword }) {
		return
	}
	if !s.tailscaleAudit(w, r, p, "tailscale-logout", "") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.tailscaleClient().Logout(ctx); err != nil {
		fail(w, http.StatusBadGateway, "tailscale", tailscaleProblem(err))
		return
	}
	s.dropTailnetCertificate("")
	s.getTailscale(w, r, p)
}

// putTailscaleSettings turns the tailnet's certificate on or off, and sends tailscaled's logs to Tailscale or not,
// which starts it again.
func (s *Server) putTailscaleSettings(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		Certificate     *bool  `json:"certificate"`
		Logs            *bool  `json:"logs"`
	}
	if !s.tailscaleChange(w, r, p, &body, func() string { return body.CurrentPassword }) {
		return
	}
	settings := s.tailscaleSettings()
	var changed []string
	restart := false
	if body.Certificate != nil && *body.Certificate != settings.Certificate {
		settings.Certificate = *body.Certificate
		changed = append(changed, "certificate "+onOff(settings.Certificate))
	}
	if body.Logs != nil && *body.Logs != settings.Logs {
		settings.Logs = *body.Logs
		changed = append(changed, "logs to Tailscale "+onOff(settings.Logs))
		restart = s.tailscaleOn()
	}
	if len(changed) == 0 {
		s.getTailscale(w, r, p)
		return
	}
	if !s.tailscaleAudit(w, r, p, "tailscale-settings", strings.Join(changed, ", ")) {
		return
	}
	if err := s.saveTailscaleSettings(settings); err != nil {
		s.failStore(w, err)
		return
	}
	if restart {
		if _, ok := s.ask(w, r, rootd.Request{Verb: "tailscale-on", Target: logsTarget(settings.Logs)}); !ok {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	s.lookAtTailnet(ctx, true)
	s.getTailscale(w, r, p)
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// tailscaleProblem is tailscaled's complaint, in a sentence for the page.
func tailscaleProblem(err error) string {
	if errors.Is(err, tailnet.ErrNotRunning) {
		return "Tailscale is still starting. Try again in a moment."
	}
	return strings.TrimPrefix(err.Error(), "tailscaled: ")
}

// WatchTailnet looks at tailscaled each half a minute while it's on: once a login has finished, it has root flush
// what tailscaled wrote, and while the owner wants the tailnet's certificate, it keeps the console using it.
func (s *Server) WatchTailnet(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		look, cancel := context.WithTimeout(ctx, 2*time.Minute)
		s.lookAtTailnet(look, false)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// lookAtTailnet is one look. now asks for the certificate whether or not it's due, after the owner changed something.
func (s *Server) lookAtTailnet(ctx context.Context, now bool) {
	if !s.tailscaleAvailable() || !s.tailscaleOn() {
		s.dropTailnetCertificate("")
		return
	}
	st, err := s.tailscaleClient().Status(ctx)
	if err != nil {
		return
	}
	running := st.BackendState == "Running"
	s.tailnet.mu.Lock()
	flush := running && s.tailnet.syncing
	if flush {
		s.tailnet.syncing = false
	}
	s.tailnet.mu.Unlock()
	if flush {
		if _, err := s.Rootd.Call(ctx, rootd.Request{Verb: "tailscale-sync"}); err != nil {
			log.Printf("tailscale: syncing its state: %v", err)
		}
	}
	name := st.Name()
	switch {
	case !s.tailscaleSettings().Certificate:
		s.dropTailnetCertificate("")
		return
	case !running:
		// Out of the tailnet, as when its key ran out, the name isn't the unit's to answer to.
		s.dropTailnetCertificate("")
		return
	case name == "" || !slices.Contains(st.CertDomains, name):
		s.dropTailnetCertificate("Turn on MagicDNS and HTTPS Certificates for the tailnet in Tailscale's admin console, on its DNS page.")
		return
	}
	s.tailnet.mu.Lock()
	due := now || s.tailnet.name != name || time.Until(s.tailnet.notAfter) < tailnetRenew
	s.tailnet.mu.Unlock()
	if !due {
		return
	}
	cert, err := s.tailscaleClient().Certificate(ctx, name)
	if err != nil {
		log.Printf("tailscale: certificate for %s: %v", name, err)
		s.tailnet.mu.Lock()
		s.tailnet.problem = "The certificate couldn't be had: " + tailscaleProblem(err)
		s.tailnet.mu.Unlock()
		return
	}
	notAfter := time.Time{}
	if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
		notAfter = leaf.NotAfter
	}
	s.useTailnetCertificate(name, &cert, notAfter)
}

func (s *Server) useTailnetCertificate(name string, cert *tls.Certificate, notAfter time.Time) {
	s.tailnet.mu.Lock()
	s.tailnet.name, s.tailnet.notAfter, s.tailnet.problem = name, notAfter, ""
	s.tailnet.mu.Unlock()
	s.TLS.SetTailnet(name, cert)
}

// dropTailnetCertificate stops answering to the tailnet's name, saying why if there's a reason the owner can act on.
func (s *Server) dropTailnetCertificate(problem string) {
	s.tailnet.mu.Lock()
	had := s.tailnet.name != ""
	s.tailnet.name, s.tailnet.notAfter, s.tailnet.problem = "", time.Time{}, problem
	s.tailnet.mu.Unlock()
	if had && s.TLS != nil {
		s.TLS.SetTailnet("", nil)
	}
}
