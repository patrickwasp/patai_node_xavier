package api

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"patai/console/internal/certs"
	"patai/console/internal/rootd"
)

type certificateBody struct {
	Chain           string `json:"chain"`
	Key             string `json:"key"`
	CurrentPassword string `json:"current_password"`
}

func (s *Server) getCertificate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var owner *certs.Owner
	problem := ""
	if s.TLS != nil {
		owner, problem = s.TLS.Owner()
	}
	writeJSON(w, http.StatusOK, map[string]any{"unit_fingerprint": s.Fingerprint, "owner": owner, "problem": problem,
		"available": s.OwnerCertPath != ""})
}

// clockTrusted is whether a certificate's dates can be checked by the unit's clock: a time server keeps it, or an
// owner set it this boot. Otherwise a unit back from a cut with no battery could refuse a good certificate.
func (s *Server) clockTrusted(r *http.Request) bool {
	resp, err := s.Rootd.Call(r.Context(), rootd.Request{Verb: "clock-status"})
	if err != nil {
		return false
	}
	var clock struct {
		Synced bool            `json:"synced"`
		Set    json.RawMessage `json:"set"`
	}
	return json.Unmarshal([]byte(resp.Output), &clock) == nil && (clock.Synced || len(clock.Set) > 0 && string(clock.Set) != "null")
}

func (s *Server) readCertificate(w http.ResponseWriter, r *http.Request, body certificateBody) (*certs.Owner, tls.Certificate, bool) {
	about, cert, err := certs.Check([]byte(body.Chain), []byte(body.Key), s.Hostname, time.Now(), s.clockTrusted(r))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-certificate", err.Error())
		return nil, cert, false
	}
	return about, cert, true
}

// checkCertificate says what a certificate would do here, and changes nothing.
func (s *Server) checkCertificate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body certificateBody
	if !s.certificateUsable(w) || !readJSON(w, r, &body) {
		return
	}
	if about, _, ok := s.readCertificate(w, r, body); ok {
		writeJSON(w, http.StatusOK, about)
	}
}

func (s *Server) certificateUsable(w http.ResponseWriter) bool {
	if s.OwnerCertPath == "" || s.TLS == nil {
		fail(w, http.StatusServiceUnavailable, "no-certificate", "this unit can't keep a certificate now")
		return false
	}
	return true
}

// setCertificate puts the owner's certificate in use at its names, with its key kept on the unit. It decides
// what a browser trusts as this unit, so it's the owner's, with a fresh password.
func (s *Server) setCertificate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body certificateBody
	if !s.certificateUsable(w) || !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	about, cert, ok := s.readCertificate(w, r, body)
	if !ok {
		return
	}
	s.certLock.Lock()
	defer s.certLock.Unlock()
	if err := certs.SaveOwner(s.OwnerCertPath, []byte(body.Chain), []byte(body.Key)); err != nil {
		s.failStore(w, err)
		return
	}
	s.TLS.SetOwner(about, &cert)
	target := fmt.Sprintf("%s, %s", strings.Join(about.Names, " "), about.Fingerprint)
	if err := s.Store.AddAudit(s.audit(r, p.Name, "certificate-set", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, about)
}

func (s *Server) removeCertificate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body certificateBody
	if !s.certificateUsable(w) || !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	s.certLock.Lock()
	defer s.certLock.Unlock()
	if err := certs.RemoveOwner(s.OwnerCertPath); err != nil {
		s.failStore(w, err)
		return
	}
	target := "none in use"
	if about, _ := s.TLS.Owner(); about != nil {
		target = fmt.Sprintf("%s, %s", strings.Join(about.Names, " "), about.Fingerprint)
	}
	s.TLS.SetOwner(nil, nil)
	if err := s.Store.AddAudit(s.audit(r, p.Name, "certificate-remove", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
