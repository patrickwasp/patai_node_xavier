package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"patai/console/internal/rootd"
	"patai/console/internal/system"
)

func (s *Server) getClock(w http.ResponseWriter, r *http.Request, p *Principal) {
	if output, ok := s.ask(w, r, rootd.Request{Verb: "clock-status"}); ok {
		writeJSON(w, http.StatusOK, json.RawMessage(output))
	}
}

// setClock is for a unit that no time server reaches, and the time is the owner's browser's as it sent it.
// The clock decides when tokens run out and what the audit log says, so this takes the owner and a fresh
// password, and a token that had run out goes before the clock is set back, so that it stays out. One at a
// time: a setting forward between another's look at the clock and its setting back would bring tokens back.
func (s *Server) setClock(w http.ResponseWriter, r *http.Request, p *Principal) {
	// The password check alone takes most of a second, which is time the browser's clock has moved on.
	arrived := time.Now()
	var body struct {
		TimeMS          int64  `json:"time_ms"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if !system.ValidTime(body.TimeMS) {
		fail(w, http.StatusBadRequest, "bad-time", "that isn't a time")
		return
	}
	output, ok := s.ask(w, r, rootd.Request{Verb: "clock-status"})
	if !ok {
		return
	}
	var clock struct {
		Synced bool `json:"synced"`
	}
	if json.Unmarshal([]byte(output), &clock) != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't say how its clock is")
		return
	}
	if clock.Synced {
		fail(w, http.StatusConflict, "ntp", "NTP keeps this unit's clock, so it can't be set by hand")
		return
	}
	s.clockLock.Lock()
	defer s.clockLock.Unlock()
	before, to := time.Now(), time.UnixMilli(body.TimeMS).Add(time.Since(arrived))
	var removed int64
	if to.Before(before) {
		var err error
		if removed, err = s.Store.DeleteExpiredTokens(before); err != nil {
			s.failStore(w, err)
			return
		}
	}
	// The helper is asked at once, so what's left between here and the setting is its own start.
	to = to.Add(time.Since(before))
	output, ok = s.ask(w, r, rootd.Request{Verb: "clock-set", Target: system.FormatTime(to.UnixMilli())})
	if !ok {
		return
	}
	// And again by the time the helper set it from: a token may have run out since the first look.
	var done struct {
		Set *struct {
			FromMS int64 `json:"from_ms"`
		} `json:"set"`
	}
	if json.Unmarshal([]byte(output), &done) == nil && done.Set != nil && to.UnixMilli() < done.Set.FromMS {
		more, err := s.Store.DeleteExpiredTokens(time.UnixMilli(done.Set.FromMS))
		if err != nil {
			s.failStore(w, err)
			return
		}
		removed += more
	}
	target := fmt.Sprintf("from %s to %s", before.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if removed > 0 {
		target += fmt.Sprintf(", removing %d token(s) that had run out", removed)
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "clock-set", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(output))
}

type powerMode struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type powerStatus struct {
	Modes   []powerMode `json:"modes"`
	Current *int        `json:"current"`
}

func (s *Server) getPowerMode(w http.ResponseWriter, r *http.Request, p *Principal) {
	if output, ok := s.ask(w, r, rootd.Request{Verb: "power-status"}); ok {
		writeJSON(w, http.StatusOK, json.RawMessage(output))
	}
}

// setPowerMode is the owner's: it's kept, and the wrong one for the unit's supply resets it under load.
// The unit can't measure its supply, so MAXN asks the owner to say it's 15 V or more (docs/hardware.md).
func (s *Server) setPowerMode(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Mode            int    `json:"mode"`
		SupplyChecked   bool   `json:"supply_checked"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	output, ok := s.ask(w, r, rootd.Request{Verb: "power-status"})
	if !ok {
		return
	}
	var status powerStatus
	if json.Unmarshal([]byte(output), &status) != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't say what its power modes are")
		return
	}
	at := slices.IndexFunc(status.Modes, func(m powerMode) bool { return m.ID == body.Mode })
	if at < 0 {
		fail(w, http.StatusBadRequest, "no-mode", "this unit has no such power mode")
		return
	}
	if status.Modes[at].Name == "MAXN" && !body.SupplyChecked {
		fail(w, http.StatusBadRequest, "supply", "MAXN needs a supply of 15 V or more, so say this unit has one")
		return
	}
	from := "unknown"
	if status.Current != nil {
		if was := slices.IndexFunc(status.Modes, func(m powerMode) bool { return m.ID == *status.Current }); was >= 0 {
			from = status.Modes[was].Name
		}
	}
	output, ok = s.ask(w, r, rootd.Request{Verb: "power-mode", Target: strconv.Itoa(body.Mode)})
	if !ok {
		return
	}
	target := fmt.Sprintf("from %s to %s", from, status.Modes[at].Name)
	if err := s.Store.AddAudit(s.audit(r, p.Name, "power-mode", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(output))
}

// rename keeps the new name and restarts the unit, which is when everything that uses the name takes it
// up. A name is how people find the unit, so it takes the owner and a fresh password.
func (s *Server) rename(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Name            string `json:"name"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if err := system.CheckName(body.Name); err != nil {
		fail(w, http.StatusBadRequest, "bad-name", err.Error())
		return
	}
	if body.Name == s.Hostname {
		fail(w, http.StatusBadRequest, "same-name", "that's this unit's name already")
		return
	}
	if status, ok := s.readUpdates(w, r); !ok || !s.free(w, status) {
		return
	}
	s.startJob(w, r, p, "rename", body.Name)
}

// shutdown is for unplugging the unit, which takes someone beside it, so it's for a person and not a token.
// The watchdog starts a stopped unit again after about 2 minutes, so a mistake costs a restart.
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request, p *Principal) {
	if p.Kind != "session" {
		fail(w, http.StatusForbidden, "forbidden", "log in to shut the unit down")
		return
	}
	if status, ok := s.readUpdates(w, r); !ok || !s.free(w, status) {
		return
	}
	s.startJob(w, r, p, "shutdown", s.Hostname)
}
