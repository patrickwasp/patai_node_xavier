package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"patai/console/internal/network"
	"patai/console/internal/rootd"
)

// getNetwork adds the address this request came in on, so the page can say whether the owner is keeping a
// change through the port that changed or through another one.
func (s *Server) getNetwork(w http.ResponseWriter, r *http.Request, p *Principal) {
	output, ok := s.ask(w, r, rootd.Request{Verb: "network-status"})
	if !ok {
		return
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't say how its network is")
		return
	}
	via := ""
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if host, _, err := net.SplitHostPort(addr.String()); err == nil {
			via = host
		}
	}
	status["via"], _ = json.Marshal(via)
	writeJSON(w, http.StatusOK, status)
}

// tryNetwork changes one port for a while: unless the owner keeps it, the unit goes back by itself. A port
// is how everyone reaches the unit, so it takes the owner and a fresh password, and never a token.
func (s *Server) tryNetwork(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		network.Setting
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	port := r.PathValue("port")
	if !network.ValidPort(port) {
		fail(w, http.StatusNotFound, "no-port", "there's no such port")
		return
	}
	if err := body.Setting.Check(); err != nil {
		fail(w, http.StatusBadRequest, "bad-setting", err.Error())
		return
	}
	output, ok := s.ask(w, r, rootd.Request{Verb: "network-try", Target: port, Network: &body.Setting})
	if !ok {
		return
	}
	var trial struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(output), &trial)
	target := fmt.Sprintf("%s: %s (change %s)", network.Label(port), body.Setting.Summary(), trial.ID)
	if err := s.Store.AddAudit(s.audit(r, p.Name, "network-try", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, json.RawMessage(output))
}

// networkDecision keeps a change or undoes it. The password went with the change, minutes ago at most.
func (s *Server) networkDecision(verb string) authed {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) {
		var body struct {
			ID string `json:"id"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if !network.ValidID(body.ID) {
			fail(w, http.StatusBadRequest, "bad-id", "that isn't a change this unit made")
			return
		}
		if _, ok := s.ask(w, r, rootd.Request{Verb: verb, Target: body.ID}); !ok {
			return
		}
		if err := s.Store.AddAudit(s.audit(r, p.Name, verb, "change "+body.ID, "ok")); err != nil {
			s.failStore(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
