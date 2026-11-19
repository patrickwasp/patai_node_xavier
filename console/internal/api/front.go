package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"

	"patai/console/internal/durable"
	"patai/console/internal/workloads"
)

// AdminPort is where the console always is, whatever the unit's address opens: https://<name>.local:9443.
const AdminPort = 9443

// The front page is what the unit's address opens, https://<name>.local on 443, and what the page on 80 points to.
// It's the console's until an owner gives it to an app with pages of its own, such as Chat, which everyone in the house
// uses every day; the console is at AdminPort then, as it always is too (docs/console.md, The front page). Whatever is
// in front, the AI API for keys, /v1/, and /healthz stay at the address, and an app's pages stay on their own port. An
// app in front that isn't on leaves the console there, so the owner can turn it on again.
type front struct {
	mu   sync.Mutex
	read bool
	app  string
}

// frontApp is the app the owner put in front, or "" for the console.
func (s *Server) frontApp() string {
	s.front.mu.Lock()
	defer s.front.mu.Unlock()
	if !s.front.read && s.FrontPath != "" {
		data, _ := os.ReadFile(s.FrontPath)
		s.front.app = strings.TrimSpace(string(data))
	}
	s.front.read = true
	return s.front.app
}

func (s *Server) setFrontApp(app string) error {
	if err := durable.Write(s.FrontPath, []byte(app+"\n"), 0o600); err != nil {
		return err
	}
	s.front.mu.Lock()
	s.front.app, s.front.read = app, true
	s.front.mu.Unlock()
	return nil
}

// frontPage is the app in front, while it's on.
func (s *Server) frontPage(ctx context.Context) (string, bool) {
	name := s.frontApp()
	if name == "" {
		return "", false
	}
	app, err := workloads.ReadApp(s.AppsPath, name)
	if err != nil || app.UI == nil || app.UI.Port == 0 {
		return "", false
	}
	v, err := s.view(ctx)
	if err != nil || !v.ui[app.UI.Port] {
		return "", false
	}
	return name, true
}

// Front is the listener on 443: the console, or the app in front.
func (s *Server) Front() http.Handler {
	console := s.Handler()
	pages := map[string]http.Handler{}
	for _, app := range workloads.ReadApps(s.AppsPath) {
		if app.UI != nil && app.UI.Port > 0 {
			pages[app.Name] = s.AppUI(app)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/v1/") {
			if name, ok := s.frontPage(r.Context()); ok && pages[name] != nil {
				pages[name].ServeHTTP(w, r)
				return
			}
		}
		console.ServeHTTP(w, r)
	})
}

type frontJSON struct {
	App       string `json:"app"`
	AdminPort int    `json:"admin_port"`
}

func (s *Server) getFront(w http.ResponseWriter, r *http.Request, p *Principal) {
	writeJSON(w, http.StatusOK, frontJSON{App: s.frontApp(), AdminPort: AdminPort})
}

// putFront is the owner choosing what the unit's address opens: an app with pages of its own, or "" for the console.
func (s *Server) putFront(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		App string `json:"app"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	target := "the console"
	if body.App != "" {
		app, err := workloads.ReadApp(s.AppsPath, body.App)
		if err != nil || app.UI == nil || app.UI.Port == 0 {
			fail(w, http.StatusBadRequest, "no-pages", "that app has no pages of its own")
			return
		}
		target = app.Title
	}
	if s.FrontPath == "" {
		fail(w, http.StatusConflict, "recovery", "the unit can't keep this change in recovery mode")
		return
	}
	if err := s.setFrontApp(body.App); err != nil {
		s.failStore(w, err)
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "front-page", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, frontJSON{App: body.App, AdminPort: AdminPort})
}
