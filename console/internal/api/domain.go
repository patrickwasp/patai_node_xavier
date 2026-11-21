package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"patai/console/internal/acme"
	"patai/console/internal/durable"
	"patai/console/internal/rootd"
)

// Remote access, Your own domain (docs/console.md; patai_docs notes/remote-access.md, agreed 2026-11-21): the owner
// gives one name they own, their DNS provider and its credentials, and the unit gets a certificate for that name from
// Let's Encrypt by DNS-01, with its node key, and renews it by itself. The credentials are the owner's, set on the
// unit and kept there (ops decisions/0009); they're never shown again, logged or audited. Each change takes a fresh
// password, as network settings do: a token or the cloud never makes one.

// domainSettings are the owner's, kept in the console's own state.
type domainSettings struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// domainWatch is what the console knows between looks.
type domainWatch struct {
	mu           sync.Mutex
	name         string    // the name the certificate in use is for
	notAfter     time.Time // when it ends
	renewAt      time.Time // when to get the next, by ARI or at two thirds of its life
	problem      string    // why the last try failed, in words for the owner
	failingSince time.Time // when tries began to fail, while they still do
	nextTry      time.Time // no try before this, after a failure
	backoff      time.Duration
	waiting      string // what a try waits for, such as a clock it can trust
	kick         chan struct{}
}

// How long the first retry waits after a failure, doubling to the most. Let's Encrypt allows 5 failed authorizations
// for a name an hour.
const (
	domainRetry    = 5 * time.Minute
	domainMaxRetry = 6 * time.Hour
	domainLook     = 6 * time.Hour // how often the watch looks when nothing is due
	domainWarn     = 7 * 24 * time.Hour
)

func (s *Server) domainAvailable() bool {
	return s.DomainPath != "" && s.Issuer != nil && s.NodeKey != nil
}

func (s *Server) domainSettings() (domainSettings, bool) {
	var d domainSettings
	data, err := os.ReadFile(s.DomainPath)
	if err != nil || json.Unmarshal(data, &d) != nil || d.Name == "" {
		return domainSettings{}, false
	}
	return d, true
}

func (s *Server) domainFile(name string) string {
	return filepath.Join(filepath.Dir(s.DomainPath), name)
}

func (s *Server) domainCredentials() map[string]string {
	values := map[string]string{}
	if data, err := os.ReadFile(s.domainFile("domain-credentials.json")); err == nil {
		json.Unmarshal(data, &values)
	}
	return values
}

func (s *Server) domainKick() chan struct{} {
	s.domain.mu.Lock()
	defer s.domain.mu.Unlock()
	if s.domain.kick == nil {
		s.domain.kick = make(chan struct{}, 1)
	}
	return s.domain.kick
}

func (s *Server) wakeDomain() {
	select {
	case s.domainKick() <- struct{}{}:
	default:
	}
}

// A name a public CA can certify, and that's the owner's to give: no address, wildcard, single label, or name that's
// already the unit's or another way's.
var domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func checkDomainName(raw, hostname string) (string, error) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	bad := errors.New("that isn't a name a certificate can be had for, such as patai.example.com")
	if name == "" || len(name) > 253 || net.ParseIP(name) != nil {
		return "", bad
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", bad
	}
	for _, label := range labels {
		if !domainLabel.MatchString(label) {
			return "", bad
		}
	}
	last := labels[len(labels)-1]
	for _, suffix := range []string{"local", "localhost", "test", "example", "invalid", "internal", "lan", "home", "arpa"} {
		if last == suffix {
			return "", errors.New("names under ." + suffix + " can't have a public certificate")
		}
	}
	switch {
	case strings.HasSuffix(name, ".ts.net"):
		return "", errors.New("the tailnet's name is Tailscale's: use Remote access with Tailscale")
	case name == "node.patai.ca" || strings.HasSuffix(name, ".node.patai.ca"):
		return "", errors.New("patai.ca's names are its own")
	case name == strings.ToLower(hostname):
		return "", bad
	}
	return name, nil
}

type domainJSON struct {
	Available    bool             `json:"available"`
	Providers    []acme.Provider  `json:"providers"`
	Name         string           `json:"name,omitempty"`
	Provider     string           `json:"provider,omitempty"`
	State        string           `json:"state"` // off, getting, in_use
	NotAfter     *time.Time       `json:"not_after,omitempty"`
	RenewAt      *time.Time       `json:"renew_at,omitempty"`
	Problem      string           `json:"problem,omitempty"`
	FailingSince *time.Time       `json:"failing_since,omitempty"`
	Waiting      string           `json:"waiting,omitempty"`
	Warn         bool             `json:"warn"` // failing for a week, or a week left: the Status page says so
	Addresses    []string         `json:"addresses"`
	Resolves     []string         `json:"resolves"`
	PointsHere   bool             `json:"points_here"`
	Credentials  []domainCredsSet `json:"credentials"` // which fields are set, never their values
}

type domainCredsSet struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
}

func (s *Server) getDomain(w http.ResponseWriter, r *http.Request, p *Principal) {
	writeJSON(w, http.StatusOK, s.domainStatus(r.Context()))
}

func (s *Server) domainStatus(ctx context.Context) domainJSON {
	out := domainJSON{Available: s.domainAvailable(), Providers: acme.Providers, State: "off", Addresses: []string{},
		Resolves: []string{}, Credentials: []domainCredsSet{}}
	settings, ok := s.domainSettings()
	if !out.Available || !ok {
		return out
	}
	out.Name, out.Provider, out.State = settings.Name, settings.Provider, "getting"
	if provider := acme.Find(settings.Provider); provider != nil {
		values := s.domainCredentials()
		for _, f := range provider.Fields {
			out.Credentials = append(out.Credentials, domainCredsSet{Name: f.Name, Set: values[f.Name] != ""})
		}
	}
	s.domain.mu.Lock()
	if s.domain.name == settings.Name {
		out.State = "in_use"
		notAfter, renewAt := s.domain.notAfter, s.domain.renewAt
		out.NotAfter = &notAfter
		if !renewAt.IsZero() {
			out.RenewAt = &renewAt
		}
	}
	out.Problem, out.Waiting = s.domain.problem, s.domain.waiting
	if !s.domain.failingSince.IsZero() {
		since := s.domain.failingSince
		out.FailingSince = &since
		out.Warn = time.Since(since) >= domainWarn
	}
	if out.NotAfter != nil && time.Until(*out.NotAfter) < domainWarn {
		out.Warn = true
	}
	s.domain.mu.Unlock()

	// Where the owner should point the name, and where it points now, as this unit's resolver sees it.
	look, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out.Addresses = append(out.Addresses, s.lanAddresses(look)...)
	out.Addresses = append(out.Addresses, s.tailnetAddresses(look)...)
	if found, err := net.DefaultResolver.LookupHost(look, settings.Name); err == nil {
		out.Resolves = found
		for _, a := range found {
			for _, mine := range out.Addresses {
				if a == mine {
					out.PointsHere = true
				}
			}
		}
	}
	return out
}

// lanAddresses are the unit's addresses on its ports, as patai-rootd says.
func (s *Server) lanAddresses(ctx context.Context) []string {
	var out []string
	resp, err := s.Rootd.Call(ctx, rootd.Request{Verb: "network-status"})
	if err != nil {
		return out
	}
	var status struct {
		Ports []struct {
			Addresses []string `json:"addresses"`
		} `json:"ports"`
	}
	if json.Unmarshal([]byte(resp.Output), &status) == nil {
		for _, port := range status.Ports {
			for _, address := range port.Addresses {
				if ip, _, err := net.ParseCIDR(address); err == nil {
					out = append(out, ip.String())
				}
			}
		}
	}
	return out
}

// tailnetAddresses are the unit's on its tailnet, while Tailscale is on.
func (s *Server) tailnetAddresses(ctx context.Context) []string {
	if !s.tailscaleAvailable() || !s.tailscaleOn() {
		return nil
	}
	st, err := s.tailscaleClient().Status(ctx)
	if err != nil {
		return nil
	}
	return st.TailscaleIPs
}

// setDomain checks the owner's credentials can write the name's challenge record, keeps them and the name, and has
// the watch get the certificate.
func (s *Server) setDomain(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !s.domainAvailable() {
		fail(w, http.StatusNotFound, "no-domain", "this release can't get a certificate for a domain")
		return
	}
	var body struct {
		CurrentPassword string            `json:"current_password"`
		Name            string            `json:"name"`
		Provider        string            `json:"provider"`
		Credentials     map[string]string `json:"credentials"`
		Agree           bool              `json:"agree"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	name, err := checkDomainName(body.Name, s.Hostname)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-name", err.Error())
		return
	}
	if !body.Agree {
		fail(w, http.StatusBadRequest, "not-agreed", "accept Let's Encrypt's Subscriber Agreement first")
		return
	}
	// Fields left empty keep what's kept for the same provider, so the owner can change the name without the token.
	settings, had := s.domainSettings()
	values := map[string]string{}
	if had && settings.Provider == body.Provider {
		values = s.domainCredentials()
	}
	for k, v := range body.Credentials {
		if strings.TrimSpace(v) != "" {
			values[k] = v
		}
	}
	dns, err := acme.Build(body.Provider, values, filepath.Dir(s.DomainPath))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-credentials", err.Error())
		return
	}
	check, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.Issuer.Check(check, name, dns); err != nil {
		fail(w, http.StatusBadRequest, "dns-check", "Couldn't write a TXT record at _acme-challenge."+name+": "+shortProblem(err))
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "domain-set", name+" at "+body.Provider, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	creds, _ := json.Marshal(acme.Keep(body.Provider, values))
	if err := durable.Write(s.domainFile("domain-credentials.json"), creds, 0o600); err != nil {
		s.failStore(w, err)
		return
	}
	data, _ := json.Marshal(domainSettings{Name: name, Provider: body.Provider})
	if err := durable.Write(s.DomainPath, data, 0o600); err != nil {
		s.failStore(w, err)
		return
	}
	if had && settings.Name != name {
		s.dropDomainCertificate()
		os.Remove(s.domainFile("domain-cert.pem"))
	}
	s.domain.mu.Lock()
	s.domain.problem, s.domain.failingSince, s.domain.nextTry, s.domain.backoff = "", time.Time{}, time.Time{}, 0
	s.domain.mu.Unlock()
	s.wakeDomain()
	writeJSON(w, http.StatusOK, s.domainStatus(r.Context()))
}

// removeDomain stops using the name, forgets the credentials, and revokes the certificate the unit had for it.
func (s *Server) removeDomain(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	settings, ok := s.domainSettings()
	if !ok {
		writeJSON(w, http.StatusOK, s.domainStatus(r.Context()))
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "domain-remove", settings.Name, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	s.dropDomainCertificate()
	chain, _ := os.ReadFile(s.domainFile("domain-cert.pem"))
	for _, name := range []string{"domain-cert.pem", "domain-credentials.json", "domain-acmedns.json"} {
		os.Remove(s.domainFile(name))
	}
	if err := os.Remove(s.DomainPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.failStore(w, err)
		return
	}
	durable.SyncDir(filepath.Dir(s.DomainPath))
	s.domain.mu.Lock()
	s.domain.problem, s.domain.failingSince, s.domain.waiting = "", time.Time{}, ""
	s.domain.mu.Unlock()
	if len(chain) > 0 {
		// Best effort, and not waited for: a certificate nobody serves is only an entry in the logs.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := s.Issuer.Revoke(ctx, chain); err != nil {
				log.Printf("domain: revoking %s's certificate: %v", settings.Name, err)
			}
		}()
	}
	writeJSON(w, http.StatusOK, s.domainStatus(r.Context()))
}

// WatchDomain keeps the certificate for the owner's domain: it puts the kept one in use at start, gets one when
// there's none, and renews it when it's due.
func (s *Server) WatchDomain(ctx context.Context) {
	kick := s.domainKick()
	for {
		wait := s.lookAtDomain(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-kick:
			timer.Stop()
		}
	}
}

// lookAtDomain is one look, and says how long until the next.
func (s *Server) lookAtDomain(ctx context.Context) time.Duration {
	settings, ok := s.domainSettings()
	if !s.domainAvailable() || !ok {
		s.dropDomainCertificate()
		return domainLook
	}
	key, err := s.NodeKey.Get()
	if err != nil {
		s.domainFailed(settings.Name, "The node key couldn't be read: "+err.Error())
		return domainRetry
	}
	chain, _ := os.ReadFile(s.domainFile("domain-cert.pem"))
	cert, leaf, usable := domainCertificate(chain, settings.Name, key)
	if usable {
		s.useDomainCertificate(settings.Name, cert, leaf.NotAfter)
	} else {
		chain = nil
		s.dropDomainCertificate()
	}
	s.domain.mu.Lock()
	renewAt, nextTry := s.domain.renewAt, s.domain.nextTry
	s.domain.mu.Unlock()
	now := time.Now()
	if usable && renewAt.IsZero() {
		renewAt = s.Issuer.RenewAt(ctx, chain)
		s.domain.mu.Lock()
		s.domain.renewAt = renewAt
		s.domain.mu.Unlock()
	}
	if usable && now.Before(renewAt) {
		return min(time.Until(renewAt), domainLook)
	}
	if now.Before(nextTry) {
		return time.Until(nextTry)
	}
	// A unit back from a power cut with no battery could ask for a certificate with its clock years out.
	if !s.clockTrustedCtx(ctx) {
		s.domain.mu.Lock()
		s.domain.waiting = "Waiting for the clock to be set."
		s.domain.mu.Unlock()
		return time.Minute
	}
	dns, err := acme.Build(settings.Provider, s.domainCredentials(), filepath.Dir(s.DomainPath))
	if err != nil {
		s.domainFailed(settings.Name, err.Error())
		return s.domainBackoff()
	}
	s.domain.mu.Lock()
	s.domain.waiting = ""
	s.domain.mu.Unlock()
	get, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	fresh, err := s.Issuer.Obtain(get, settings.Name, dns, chain)
	if err != nil {
		log.Printf("domain: certificate for %s: %v", settings.Name, err)
		s.domainFailed(settings.Name, "The certificate couldn't be had: "+shortProblem(err))
		return s.domainBackoff()
	}
	cert, leaf, usable = domainCertificate(fresh, settings.Name, key)
	if !usable {
		s.domainFailed(settings.Name, "The CA's certificate isn't for this name and key.")
		return s.domainBackoff()
	}
	if err := durable.Write(s.domainFile("domain-cert.pem"), fresh, 0o600); err != nil {
		s.domainFailed(settings.Name, "The certificate couldn't be kept: "+err.Error())
		return s.domainBackoff()
	}
	s.useDomainCertificate(settings.Name, cert, leaf.NotAfter)
	renewAt = s.Issuer.RenewAt(ctx, fresh)
	s.domain.mu.Lock()
	s.domain.renewAt, s.domain.problem, s.domain.failingSince, s.domain.backoff, s.domain.nextTry =
		renewAt, "", time.Time{}, 0, time.Time{}
	s.domain.mu.Unlock()
	return min(time.Until(renewAt), domainLook)
}

// domainCertificate is the kept chain as a certificate, if it's for name, has the node key and hasn't run out.
func domainCertificate(chain []byte, name string, key *ecdsa.PrivateKey) (*tls.Certificate, *x509.Certificate, bool) {
	if len(chain) == 0 {
		return nil, nil, false
	}
	leaf, err := acme.Leaf(chain)
	if err != nil || leaf.VerifyHostname(name) != nil || time.Now().After(leaf.NotAfter) {
		return nil, nil, false
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return nil, nil, false
	}
	var cert tls.Certificate
	for rest := chain; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
	}
	cert.PrivateKey, cert.Leaf = key, leaf
	return &cert, leaf, len(cert.Certificate) > 0
}

func (s *Server) useDomainCertificate(name string, cert *tls.Certificate, notAfter time.Time) {
	s.domain.mu.Lock()
	changed := s.domain.name != name || !s.domain.notAfter.Equal(notAfter)
	if changed {
		s.domain.renewAt = time.Time{}
	}
	s.domain.name, s.domain.notAfter = name, notAfter
	s.domain.mu.Unlock()
	if changed && s.TLS != nil {
		s.TLS.SetDomain(name, cert)
	}
}

func (s *Server) dropDomainCertificate() {
	s.domain.mu.Lock()
	had := s.domain.name != ""
	s.domain.name, s.domain.notAfter, s.domain.renewAt = "", time.Time{}, time.Time{}
	s.domain.mu.Unlock()
	if had && s.TLS != nil {
		s.TLS.SetDomain("", nil)
	}
}

func (s *Server) domainFailed(name, problem string) {
	s.domain.mu.Lock()
	defer s.domain.mu.Unlock()
	s.domain.problem = problem
	if s.domain.failingSince.IsZero() {
		s.domain.failingSince = time.Now()
	}
}

func (s *Server) domainBackoff() time.Duration {
	s.domain.mu.Lock()
	defer s.domain.mu.Unlock()
	if s.domain.backoff == 0 {
		s.domain.backoff = domainRetry
	} else {
		s.domain.backoff = min(2*s.domain.backoff, domainMaxRetry)
	}
	s.domain.nextTry = time.Now().Add(s.domain.backoff)
	return s.domain.backoff
}

// shortProblem is an error in a line for the page, without anything long a provider might have sent back.
func shortProblem(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

// domainName is the owner's domain while the unit has a certificate in use there, or "".
func (s *Server) domainName() string {
	s.domain.mu.Lock()
	defer s.domain.mu.Unlock()
	return s.domain.name
}
