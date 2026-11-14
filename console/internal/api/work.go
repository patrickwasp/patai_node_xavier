package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"

	"patai/console/internal/rootd"
	"patai/console/internal/store"
	"patai/console/internal/workloads"
)

// startJob queues work under the caller's Idempotency-Key, so a retry after a dropped connection
// can't reboot a unit twice.
func (s *Server) startJob(w http.ResponseWriter, r *http.Request, p *Principal, kind, target string) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		fail(w, http.StatusBadRequest, "idempotency-key", "send an Idempotency-Key header")
		return
	}
	j, created, err := s.Store.CreateJob(store.Job{Kind: kind, Target: target, Actor: p.Name, BootID: s.BootID}, key,
		s.audit(r, p.Name, kind, target, "queued"))
	if err != nil {
		s.failStore(w, err)
		return
	}
	if created && s.Jobs != nil {
		s.Jobs.Wake()
	}
	writeJSON(w, http.StatusAccepted, j)
}

// reboot says no at once during an update. Jobs run one at a time, so a restart queued behind an install
// would otherwise wait for it and then happen by itself, long after anyone asked.
func (s *Server) reboot(w http.ResponseWriter, r *http.Request, p *Principal) {
	if status, ok := s.readUpdates(w, r); !ok || !s.free(w, status) {
		return
	}
	s.startJob(w, r, p, "reboot", s.Hostname)
}

// appJob turns an app on or off, or gets what it needs.
func (s *Server) appJob(kind string) authed {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) {
		app := r.PathValue("app")
		if !s.appExists(app) {
			fail(w, http.StatusNotFound, "not-found", "no such app")
			return
		}
		if (kind == "workload-start" || kind == "workload-fetch") && !s.diskReady(w, r) {
			return
		}
		s.startJob(w, r, p, kind, app)
	}
}

// apiJob turns one of an app's APIs on or off, which starts or stops the service behind it.
func (s *Server) apiJob(kind string) authed {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) {
		m, err := workloads.ReadApp(s.AppsPath, r.PathValue("app"))
		api, ok := workloads.API{}, false
		if err == nil {
			api, ok = m.API(r.PathValue("api"))
		}
		if !ok {
			fail(w, http.StatusNotFound, "not-found", "no such API")
			return
		}
		if api.Always && kind == "app-api-off" {
			fail(w, http.StatusConflict, "always-on", api.Title+" is always on")
			return
		}
		if kind == "app-api-on" && !s.diskReady(w, r) {
			return
		}
		s.startJob(w, r, p, kind, m.Name+"/"+api.Name)
	}
}

func (s *Server) workloadStatus(w http.ResponseWriter, r *http.Request, p *Principal) {
	if output, ok := s.ask(w, r, rootd.Request{Verb: "workload-status"}); ok {
		writeJSON(w, http.StatusOK, json.RawMessage(output))
	}
}

// resources is how busy the unit is now. The Status page asks every few seconds, which doesn't count as using a login.
func (s *Server) resources(w http.ResponseWriter, r *http.Request, p *Principal) {
	if output, ok := s.ask(w, r, rootd.Request{Verb: "resources"}); ok {
		writeJSON(w, http.StatusOK, json.RawMessage(output))
	}
}

func (s *Server) stackExists(stack string) bool {
	return workloads.IsService(s.RecipesPath, stack)
}

func (s *Server) appExists(app string) bool {
	_, err := workloads.ReadApp(s.AppsPath, app)
	return err == nil
}

// workloads lists the services the signed image can run. How each one is doing comes from the status.
func (s *Server) workloads(w http.ResponseWriter, r *http.Request, p *Principal) {
	writeJSON(w, http.StatusOK, workloads.ServiceNames(s.RecipesPath))
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, p *Principal) {
	list, err := s.Store.Jobs(50)
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	j, err := s.Store.Job(id)
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not-found", "no such job")
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// webUI is an app's own web UI, on a port of its own because it doesn't work under a path prefix: ComfyUI's, on 8188.
// It's there while its app is on, for someone logged in who can run apps, and passes streams and WebSockets through.
// Not for an API key: the UI does what the key's limits would hold it from. The session stops here.
func (s *Server) webUI(port int, target *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		fail(w, http.StatusBadGateway, "service-down", "it isn't answering")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _, ok := s.caller(w, r)
		if !ok {
			return
		}
		if p.Kind != "session" {
			fail(w, http.StatusForbidden, "forbidden", "log in to the console to use this")
			return
		}
		v, err := s.view(r.Context())
		if err != nil || !v.ui[port] {
			fail(w, http.StatusNotFound, "app-off", "its app is off")
			return
		}
		strip(r)
		proxy.ServeHTTP(w, r)
	})
}

func stripCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

// ComfyHandler is the ComfyUI app's listener.
func (s *Server) ComfyHandler() http.Handler { return s.guard(s.webUI(8188, s.Comfy)) }
