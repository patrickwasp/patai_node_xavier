package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"patai/console/internal/auth"
	"patai/console/internal/store"
	"patai/console/internal/workloads"
)

// The AI API's keys are tokens with the key scope, each with limits (docs/apps.md, Keys). A key is shown once,
// and only its hash is kept.

// How far a limit can go.
const (
	maxKeyConcurrent = 64
	maxKeyBody       = 64 << 20
	maxKeyDaily      = 1 << 40
)

type keyBody struct {
	Name            string       `json:"name"`
	ExpiresDays     int          `json:"expires_days"`
	Limits          store.Limits `json:"limits"`
	CurrentPassword string       `json:"current_password"`
}

// checkLimits says what's wrong with a key's limits, if anything: only APIs some app has, and nothing past what the
// unit could do.
func (s *Server) checkLimits(l *store.Limits) string {
	known := []string{}
	for _, app := range workloads.ReadApps(s.AppsPath) {
		for _, api := range app.APIs {
			known = append(known, api.Name)
		}
	}
	if l.APIs == nil {
		l.APIs = []string{}
	}
	for _, api := range l.APIs {
		if !slices.Contains(known, api) {
			return "no API here is called " + api
		}
	}
	slices.Sort(l.APIs)
	l.APIs = slices.Compact(l.APIs)
	switch {
	case l.MaxConcurrent < 0 || l.MaxConcurrent > maxKeyConcurrent:
		return "requests at once can be 0, for no limit, to " + strconv.Itoa(maxKeyConcurrent)
	case l.MaxBody < 0 || l.MaxBody > maxKeyBody:
		return "a request's size can be 0, for no limit, to 64 MiB"
	case l.TokensPerDay < 0 || l.TokensPerDay > maxKeyDaily || l.ImagesPerDay < 0 || l.ImagesPerDay > maxKeyDaily:
		return "a day's limit can't be below 0"
	}
	return ""
}

// keyView is a key, with what it has used today.
type keyView struct {
	store.Key
	Today *store.Usage `json:"today"`
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, p *Principal) {
	keys, err := s.Store.Keys()
	if err != nil {
		s.failStore(w, err)
		return
	}
	usage, err := s.Store.UsageSince(store.Day(time.Now()))
	if err != nil {
		s.failStore(w, err)
		return
	}
	list := []keyView{}
	for _, k := range keys {
		view := keyView{Key: k}
		for i := range usage {
			if usage[i].KeyID == k.ID {
				view.Today = &usage[i]
			}
		}
		list = append(list, view)
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body keyBody
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if !userName.MatchString(body.Name) {
		fail(w, http.StatusBadRequest, "bad-name", "give it a name of letters, digits, dots, dashes and underscores")
		return
	}
	if problem := s.checkLimits(&body.Limits); problem != "" {
		fail(w, http.StatusBadRequest, "bad-limits", problem)
		return
	}
	t := store.Token{Name: body.Name, CreatedBy: p.Name}
	if body.ExpiresDays < 0 || body.ExpiresDays > 3650 {
		fail(w, http.StatusBadRequest, "bad-expiry", "it can run out in 1 to 3650 days, or never")
		return
	}
	if body.ExpiresDays > 0 {
		at := time.Now().UTC().Add(time.Duration(body.ExpiresDays) * 24 * time.Hour).Format(time.RFC3339)
		t.ExpiresAt = &at
	}
	plain := auth.NewToken()
	created, err := s.Store.CreateKey(t, auth.HashSecret(plain), body.Limits,
		s.audit(r, p.Name, "create-key", body.Name+" ("+limitWords(body.Limits)+")", "ok"))
	if errors.Is(err, store.ErrExists) {
		fail(w, http.StatusConflict, "exists", "a key or token has that name")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": created, "secret": plain})
}

func (s *Server) setKeyLimits(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var l store.Limits
	if !readJSON(w, r, &l) {
		return
	}
	if problem := s.checkLimits(&l); problem != "" {
		fail(w, http.StatusBadRequest, "bad-limits", problem)
		return
	}
	err := s.Store.SetLimits(id, l, s.audit(r, p.Name, "key-limits", strconv.FormatInt(id, 10)+" ("+limitWords(l)+")", "ok"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not-found", "no such key")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// limitWords is a key's limits for the audit log.
func limitWords(l store.Limits) string {
	words := []string{}
	if len(l.APIs) > 0 {
		words = append(words, strings.Join(l.APIs, "+"))
	}
	for _, limit := range []struct {
		value int64
		unit  string
	}{{l.MaxConcurrent, "at once"}, {l.MaxBody, "bytes"}, {l.TokensPerDay, "tokens a day"}, {l.ImagesPerDay, "images a day"}} {
		if limit.value > 0 {
			words = append(words, strconv.FormatInt(limit.value, 10)+" "+limit.unit)
		}
	}
	if len(words) == 0 {
		return "no limits"
	}
	return strings.Join(words, ", ")
}

// apiUsage is each key's use by day, for up to 30 days.
func (s *Server) apiUsage(w http.ResponseWriter, r *http.Request, p *Principal) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > store.RequestDays {
		days = 7
	}
	usage, err := s.Store.UsageSince(store.Day(time.Now().AddDate(0, 0, 1-days)))
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// apiRequests is the request log, newest first, a page at a time.
func (s *Server) apiRequests(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	list, err := s.Store.APIRequests(limit, before)
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
