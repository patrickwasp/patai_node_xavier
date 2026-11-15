package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httputil"
	"strings"

	"patai/console/internal/store"
	"patai/console/internal/workloads"
)

// AppsHandler is the gateway for apps (docs/apps.md, The gateway): how an app with a key of its own reaches its APIs.
// It answers plain HTTP on 18000, which the firewall lets only containers reach, and takes nothing but an app's key, as
// a bearer token or a password: owners' keys and people use the gateway on 443. An app reaches its own APIs that are
// on, each logged under the app's name as a key's requests are, with no limits but what admission gives its services.
func (s *Server) AppsHandler() http.Handler {
	s.persons.reaper.Do(func() { go s.reapPeople() })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Mode != ModeNormal {
			apiError(w, http.StatusServiceUnavailable, "server_error", "recovery", "this unit needs service")
			return
		}
		v, err := s.view(r.Context())
		if err != nil {
			apiError(w, http.StatusServiceUnavailable, "server_error", "unavailable", "the unit's root helper isn't answering")
			return
		}
		app, ok := v.keys[keyHash(r)]
		if !ok {
			// A WebSocket to a person's own container signs in with its first message (people.go).
			if isWebSocket(r) {
				for _, routes := range v.people {
					if _, _, ok := find(routes, r.URL.Path); ok {
						s.personSocket(w, r, v)
						return
					}
				}
			}
			apiError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "send the app's key")
			return
		}
		if service, prefix, ok := find(v.people[app], r.URL.Path); ok {
			s.personHTTP(w, r, app, service, prefix)
			return
		}
		routes := v.own[app]
		// Only what it chats with: its embeddings and image models aren't for a person to pick.
		if r.URL.Path == "/v1/models" || strings.HasPrefix(r.URL.Path, "/v1/models/") {
			chat := map[string]route{}
			if rt, ok := routes["/v1/chat/completions"]; ok {
				chat["/v1/chat/completions"] = rt
			}
			s.listModels(w, r, &view{routes: chat, models: v.models}, &store.Limits{})
			return
		}
		rt, endpoint, ok := find(routes, r.URL.Path)
		if !ok {
			apiError(w, http.StatusNotFound, "invalid_request_error", "unknown_url", "this app has no API that's on at that path")
			return
		}
		s.forward(w, r, v, rt, endpoint, keyRef{0, v.titles[app]}, &store.Limits{})
	})
}

// keyHash is the SHA-256 of the key a request has, in hex: a bearer token, or Basic's password for a client that can
// only put one in an address, as Open WebUI's search does.
func keyHash(r *http.Request) string {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		_, key, ok = r.BasicAuth()
	}
	if !ok || key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// AppUI is an app's own web pages, on a port of their own (docs/apps.md, Isolation), from the service its manifest
// names: there while the app is on, with streams and WebSockets passed through. People sign in with the app's own
// accounts, so it needs no console login, and the console's session never reaches it.
func (s *Server) AppUI(app workloads.AppManifest) http.Handler {
	target := serviceAddress(s.RecipesPath, app.UI.Service)
	port := app.UI.Port
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			fail(w, http.StatusBadGateway, "service-down", "it isn't answering")
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.Header.Get("X-Frame-Options") == "" {
				resp.Header.Set("X-Frame-Options", "SAMEORIGIN")
			}
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// As on 443: a name's HSTS is the same on every port.
		h.Set("Strict-Transport-Security", "max-age=0")
		if !s.usable(w) {
			return
		}
		v, err := s.view(r.Context())
		if err != nil || !v.ui[port] || target == nil {
			fail(w, http.StatusNotFound, "app-off", "its app is off")
			return
		}
		stripCookie(r, sessionCookie)
		proxy.ServeHTTP(w, r)
	})
}
