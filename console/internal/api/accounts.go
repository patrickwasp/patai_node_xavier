package api

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"patai/console/internal/auth"
	"patai/console/internal/store"
)

const sessionCookie = "__Host-patai-session"
const recoveryCodes = 8

var userName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

func (s *Server) setSession(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode})
}

// claim makes the first owner. The code proves someone can see the unit or holds its card. It's used
// up by the same step that creates the owner: the store's transaction decides who won, and the file
// goes after, so a crash in between leaves a claimed unit with a stale file, which is ignored.
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var body struct{ Code, Name, Password string }
	if !readJSON(w, r, &body) {
		return
	}
	if !s.throttle.Allow("claim") {
		fail(w, http.StatusTooManyRequests, "throttled", "too many attempts; wait and try again")
		return
	}
	want := s.ClaimCode()
	if s.State() != "unclaimed" || want == "" {
		fail(w, http.StatusConflict, "not-claimable", "this unit can't be claimed")
		return
	}
	if !auth.SameCode(want, auth.NormalizeCode(body.Code)) {
		s.throttle.Fail("claim")
		s.Store.AddAudit(s.audit(r, "-", "claim", "", "wrong code"))
		fail(w, http.StatusForbidden, "bad-code", "that code isn't right")
		return
	}
	if !userName.MatchString(body.Name) {
		fail(w, http.StatusBadRequest, "bad-name", "use letters, numbers and . _ @ -")
		return
	}
	if err := auth.CheckStrength(body.Password); err != nil {
		fail(w, http.StatusBadRequest, "weak-password", err.Error())
		return
	}
	plain := make([]string, recoveryCodes)
	hashes := make([]string, recoveryCodes)
	for i := range plain {
		plain[i] = auth.NewCode()
		hashes[i] = auth.HashSecret(plain[i])
	}
	u, err := s.Store.Claim(body.Name, auth.HashPassword(body.Password), hashes,
		s.audit(r, body.Name, "claim", s.Hostname, "ok"))
	if errors.Is(err, store.ErrClaimed) {
		fail(w, http.StatusConflict, "not-claimable", "this unit can't be claimed")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	os.Remove(s.ClaimPath)
	s.throttle.Reset("claim")
	s.setSession(w, s.sessions.Create(u.ID, u.Name, u.Role))
	writeJSON(w, http.StatusCreated, map[string]any{"user": u, "recovery_codes": plain})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Password string }
	if !readJSON(w, r, &body) {
		return
	}
	key := "login:" + source(r) + ":" + body.Name
	if !s.throttle.Allow(key) {
		fail(w, http.StatusTooManyRequests, "throttled", "too many attempts; wait and try again")
		return
	}
	u, err := s.Store.UserByName(body.Name)
	if errors.Is(err, store.ErrNotFound) {
		auth.BurnPasswordCheck(body.Password)
	} else if err != nil {
		s.failStore(w, err)
		return
	}
	if err != nil || !auth.CheckPassword(u.PasswordHash, body.Password) {
		s.throttle.Fail(key)
		s.Store.AddAudit(s.audit(r, body.Name, "login", "", "refused"))
		fail(w, http.StatusUnauthorized, "bad-login", "wrong name or password")
		return
	}
	s.throttle.Reset(key)
	s.setSession(w, s.sessions.Create(u.ID, u.Name, u.Role))
	s.Store.AddAudit(s.audit(r, u.Name, "login", "", "ok"))
	left, _ := s.Store.RecoveryCodesLeft(u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "recovery_codes_left": left})
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request, p *Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"name": p.Name, "role": p.Role, "kind": p.Kind})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, p *Principal) {
	if p.Session != "" {
		s.sessions.Delete(p.Session)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", Secure: true, HttpOnly: true, MaxAge: -1,
		SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// recover uses a recovery code to set a new password. It ends that user's sessions, because whoever
// was logged in may be why they're here.
func (s *Server) recover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string
		Code     string
		Password string
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !s.throttle.Allow("recover") {
		fail(w, http.StatusTooManyRequests, "throttled", "too many attempts; wait and try again")
		return
	}
	if err := auth.CheckStrength(body.Password); err != nil {
		fail(w, http.StatusBadRequest, "weak-password", err.Error())
		return
	}
	hash := auth.HashSecret(auth.NormalizeCode(body.Code))
	u, err := s.Store.Recover(body.Name, hash, auth.HashPassword(body.Password), s.audit(r, body.Name, "recover", body.Name, "ok"))
	if errors.Is(err, store.ErrNotFound) {
		s.throttle.Fail("recover")
		s.Store.AddAudit(s.audit(r, body.Name, "recover", body.Name, "refused"))
		fail(w, http.StatusForbidden, "bad-code", "that name or code isn't right")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	s.sessions.DeleteUser(u.ID, "")
	w.WriteHeader(http.StatusNoContent)
}

// confirmPassword is the fresh password a sensitive action asks for. A token has none, and can't reach
// these: they're all owner actions, and tokens stop at operator.
func (s *Server) confirmPassword(w http.ResponseWriter, p *Principal, password string) bool {
	u, err := s.Store.UserByID(p.UserID)
	if err != nil {
		s.failStore(w, err)
		return false
	}
	key := "confirm:" + p.Name
	if !s.throttle.Allow(key) {
		fail(w, http.StatusTooManyRequests, "throttled", "too many attempts; wait and try again")
		return false
	}
	if !auth.CheckPassword(u.PasswordHash, password) {
		s.throttle.Fail(key)
		fail(w, http.StatusForbidden, "bad-password", "that password isn't right")
		return false
	}
	s.throttle.Reset(key)
	return true
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, p *Principal) {
	if p.Kind != "session" {
		fail(w, http.StatusForbidden, "forbidden", "log in to change a password")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if err := auth.CheckStrength(body.NewPassword); err != nil {
		fail(w, http.StatusBadRequest, "weak-password", err.Error())
		return
	}
	err := s.Store.SetPassword(p.UserID, auth.HashPassword(body.NewPassword), s.audit(r, p.Name, "change-password", p.Name, "ok"))
	if err != nil {
		s.failStore(w, err)
		return
	}
	s.sessions.DeleteUser(p.UserID, p.Session)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, p *Principal) {
	users, err := s.Store.Users()
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func validRole(role string) bool { return role == "owner" || role == "operator" || role == "viewer" }

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Name            string `json:"name"`
		Role            string `json:"role"`
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if !userName.MatchString(body.Name) || !validRole(body.Role) {
		fail(w, http.StatusBadRequest, "bad-name", "use letters, numbers and . _ @ -, and a known role")
		return
	}
	if err := auth.CheckStrength(body.Password); err != nil {
		fail(w, http.StatusBadRequest, "weak-password", err.Error())
		return
	}
	u, err := s.Store.CreateUser(body.Name, body.Role, auth.HashPassword(body.Password),
		s.audit(r, p.Name, "create-user", body.Name+" ("+body.Role+")", "ok"))
	if errors.Is(err, store.ErrExists) {
		fail(w, http.StatusConflict, "exists", "that name is in use")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// userLabel is the name for the audit log, which outlives the user it mentions.
func (s *Server) userLabel(id int64) string {
	if u, err := s.Store.UserByID(id); err == nil {
		return u.Name
	}
	return strconv.FormatInt(id, 10)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	err := s.Store.DeleteUser(id, s.audit(r, p.Name, "delete-user", s.userLabel(id), "ok"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "not-found", "no such user")
	case errors.Is(err, store.ErrLastOwner):
		fail(w, http.StatusConflict, "last-owner", "a unit must keep one owner")
	case err != nil:
		s.failStore(w, err)
	default:
		s.sessions.DeleteUser(id, "")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if err := auth.CheckStrength(body.Password); err != nil {
		fail(w, http.StatusBadRequest, "weak-password", err.Error())
		return
	}
	err := s.Store.SetPassword(id, auth.HashPassword(body.Password),
		s.audit(r, p.Name, "reset-password", s.userLabel(id), "ok"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not-found", "no such user")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	s.sessions.DeleteUser(id, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, p *Principal) {
	tokens, err := s.Store.Tokens()
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// createToken shows the token once; only its hash is kept. It's never an owner: a token can't manage
// the unit, so a leaked one can't lock its owner out.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Name            string `json:"name"`
		Scope           string `json:"scope"`
		ExpiresDays     int    `json:"expires_days"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if !userName.MatchString(body.Name) || (body.Scope != "viewer" && body.Scope != "operator") {
		fail(w, http.StatusBadRequest, "bad-name", "give it a name and a scope of viewer or operator")
		return
	}
	t := store.Token{Name: body.Name, Scope: body.Scope, CreatedBy: p.Name}
	if body.ExpiresDays > 0 {
		at := time.Now().UTC().Add(time.Duration(body.ExpiresDays) * 24 * time.Hour).Format(time.RFC3339)
		t.ExpiresAt = &at
	}
	plain := auth.NewToken()
	created, err := s.Store.CreateToken(t, auth.HashSecret(plain),
		s.audit(r, p.Name, "create-token", body.Name+" ("+body.Scope+")", "ok"))
	if errors.Is(err, store.ErrExists) {
		fail(w, http.StatusConflict, "exists", "that name is in use")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": created, "secret": plain})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.Store.DeleteToken(id, s.audit(r, p.Name, "delete-token", strconv.FormatInt(id, 10), "ok"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not-found", "no such token")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, p *Principal) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	list, err := s.Store.AuditList(200, before)
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
