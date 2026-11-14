package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"patai/console/internal/rootd"
)

const rootdWait = 30 * time.Second

// ask is a short call to the helper. When it says no, the reason is for the person who asked; when it
// doesn't answer, that isn't.
func (s *Server) ask(w http.ResponseWriter, r *http.Request, req rootd.Request) (string, bool) {
	return s.askWithin(w, r, req, rootdWait)
}

// askWithin is ask for one that can take longer: one that waits for the apps to stop, say.
func (s *Server) askWithin(w http.ResponseWriter, r *http.Request, req rootd.Request, wait time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	resp, err := s.Rootd.Call(ctx, req)
	if err == nil {
		return resp.Output, true
	}
	if resp.Error != "" {
		fail(w, http.StatusBadRequest, "refused", resp.Error)
	} else {
		fail(w, http.StatusBadGateway, "rootd", "the unit's root helper isn't answering")
	}
	return "", false
}

type diskStatus struct {
	State string `json:"state"`
}

func (s *Server) diskState(w http.ResponseWriter, r *http.Request) (json.RawMessage, string, bool) {
	output, ok := s.ask(w, r, rootd.Request{Verb: "disk-status"})
	if !ok {
		return nil, "", false
	}
	var status diskStatus
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't say how its disk is")
		return nil, "", false
	}
	return json.RawMessage(output), status.State, true
}

func (s *Server) disk(w http.ResponseWriter, r *http.Request, p *Principal) {
	if raw, _, ok := s.diskState(w, r); ok {
		writeJSON(w, http.StatusOK, raw)
	}
}

var diskProblems = map[string]string{
	"undecided": "the disk has data from before: choose to keep or erase it first",
	"foreign":   "the data disk isn't set up yet",
	"absent":    "this unit has no data disk",
	"unmounted": "the data disk didn't mount",
}

// diskReady stops a workload start that couldn't work, and says why. patai-workload checks for itself.
func (s *Server) diskReady(w http.ResponseWriter, r *http.Request) bool {
	_, state, ok := s.diskState(w, r)
	if !ok {
		return false
	}
	if problem, bad := diskProblems[state]; bad {
		fail(w, http.StatusConflict, "disk-not-ready", problem)
		return false
	}
	return true
}

// diskAnswer is the owner's keep or erase. It's a job because taking 28 GB of old images off a disk
// takes a while, and it's asked of patai-data-disk, which checks the state again.
func (s *Server) diskAnswer(kind string, from ...string) authed {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) {
		var body struct {
			CurrentPassword string `json:"current_password"`
		}
		if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
			return
		}
		_, state, ok := s.diskState(w, r)
		if !ok {
			return
		}
		if !slices.Contains(from, state) {
			fail(w, http.StatusConflict, "disk-not-waiting", "the disk isn't waiting for an answer")
			return
		}
		s.startJob(w, r, p, kind, "nvme")
	}
}

// factoryReset can't be undone, so it wants the unit's name as well as a fresh password, and only a
// person gets this far: tokens stop at operator. The claim code is for whoever has the unit next. It
// goes back only to the person who asked, never into a job or the audit log.
func (s *Server) factoryReset(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Name            string `json:"name"`
		KeepData        bool   `json:"keep_data"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if body.Name != s.Hostname {
		fail(w, http.StatusBadRequest, "bad-name", "that isn't this unit's name")
		return
	}
	target := "erase the disk"
	if body.KeepData {
		target = "keep the disk"
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "factory-reset", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	code, ok := s.ask(w, r, rootd.Request{Verb: "factory-reset", KeepData: body.KeepData})
	if !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"claim_code": code})
}

func (s *Server) listSSHKeys(w http.ResponseWriter, r *http.Request, p *Principal) {
	if output, ok := s.ask(w, r, rootd.Request{Verb: "ssh-keys-list"}); ok {
		writeJSON(w, http.StatusOK, json.RawMessage(output))
	}
}

// A key is root on this unit, so adding or removing one takes the same fresh password as a person.
func (s *Server) addSSHKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Key             string `json:"key"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	fingerprint, ok := s.ask(w, r, rootd.Request{Verb: "ssh-key-add", Key: body.Key})
	if !ok {
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "add-ssh-key", fingerprint, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeSSHKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.ask(w, r, rootd.Request{Verb: "ssh-key-remove", Key: id}); !ok {
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "remove-ssh-key", id, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
