package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"patai/console/internal/rootd"
	"patai/console/internal/workloads"
)

// An owner's model can't be bigger than this, which is more than the biggest the unit could run.
const maxModel = 64 << 30

// model is the app and model a request names, as a job's target, if both could be names and the app is one this
// release has. patai-workload checks the rest.
func (s *Server) model(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	stack := r.PathValue("stack")
	target := stack + "/" + name
	if _, _, ok := workloads.ModelTarget(target); !ok || !s.stackExists(stack) {
		fail(w, http.StatusNotFound, "not-found", "no such model")
		return "", false
	}
	return target, true
}

// modelJob runs a model, downloads one, or takes one off the unit.
func (s *Server) modelJob(kind string) authed {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) {
		target, ok := s.model(w, r, r.PathValue("model"))
		if !ok || kind != "model-remove" && !s.diskReady(w, r) {
			return
		}
		s.startJob(w, r, p, kind, target)
	}
}

// addModel downloads an owner's own model from an address, in a service of its own: the Workloads page follows it.
func (s *Server) addModel(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	target, ok := s.model(w, r, body.Name)
	if !ok {
		return
	}
	if len(body.URL) > 2048 || !strings.HasPrefix(body.URL, "https://") && !strings.HasPrefix(body.URL, "http://") {
		fail(w, http.StatusBadRequest, "bad-url", "give an http or https address")
		return
	}
	if !s.diskReady(w, r) {
		return
	}
	if _, ok := s.ask(w, r, rootd.Request{Verb: "model-add-url", Target: target, URL: body.URL}); !ok {
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "model-add", target, "started")); err != nil {
		log.Printf("audit: %v", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"name": body.Name})
}

// uploadModel takes an owner's own model as the request's body, into a pipe patai-rootd gave it. The helper writes the
// file, on the data disk where this account can't reach, and no more of it than the size given here. patai-fetch
// copies it from there before it checks it.
func (s *Server) uploadModel(w http.ResponseWriter, r *http.Request, p *Principal) {
	target, ok := s.model(w, r, r.PathValue("model"))
	if !ok {
		return
	}
	size := r.ContentLength
	if size <= 0 {
		fail(w, http.StatusLengthRequired, "no-length", "the upload has to say how big it is")
		return
	}
	if size > maxModel {
		fail(w, http.StatusRequestEntityTooLarge, "too-big", "that file is too big to be a model")
		return
	}
	if !s.diskReady(w, r) {
		return
	}
	uploader, ok := s.Rootd.(rootd.Uploader)
	if !ok {
		fail(w, http.StatusNotImplemented, "no-upload", "this unit can't take uploads")
		return
	}
	if !s.uploading.TryLock() {
		fail(w, http.StatusConflict, "upload-running", "another upload is running")
		return
	}
	defer s.uploading.Unlock()
	// It needs room twice: for the upload, and for patai-fetch's copy.
	if !roomOnDisk(s.DataPath, 2*size) {
		fail(w, http.StatusInsufficientStorage, "no-room", "the data disk doesn't have room for that file")
		return
	}
	out, id, err := uploader.OpenUpload(r.Context(), size)
	if err != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't open a file for it: "+err.Error())
		return
	}
	if err := receiveInto(w, r, out, size); err != nil {
		log.Printf("model upload: %v", err)
		s.discardUpload(r, id)
		fail(w, http.StatusBadRequest, "upload-failed", "the file didn't arrive whole")
		return
	}
	if _, ok := s.ask(w, r, rootd.Request{Verb: "model-upload-take", Target: target, Upload: id}); !ok {
		s.discardUpload(r, id)
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "model-upload", target, "started")); err != nil {
		log.Printf("audit: %v", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"name": r.PathValue("model")})
}

// discardUpload has the helper delete an upload that won't be used, which this account can't reach to do itself.
func (s *Server) discardUpload(r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rootdWait)
	defer cancel()
	if _, err := s.Rootd.Call(ctx, rootd.Request{Verb: "model-upload-discard", Upload: id}); err != nil {
		log.Printf("model upload: discarding it: %v", err)
	}
}

// serviceSettings changes a service's settings for the model it runs: {"name": value}, null for its default.
// patai-workload checks each against the recipe, and restarts the service if it has to.
func (s *Server) serviceSettings(w http.ResponseWriter, r *http.Request, p *Principal) {
	stack := r.PathValue("stack")
	if !s.stackExists(stack) {
		fail(w, http.StatusNotFound, "not-found", "no such service")
		return
	}
	var values map[string]any
	if !readJSON(w, r, &values) {
		return
	}
	data, _ := json.Marshal(values)
	if len(values) == 0 || len(data) > 4096 {
		fail(w, http.StatusBadRequest, "bad-settings", `give settings as {"name": value}`)
		return
	}
	// It can wait for another change to the apps, and for the apps to stop, as patai-rootd can.
	if _, ok := s.askWithin(w, r, rootd.Request{Verb: "service-settings", Target: stack, Values: data}, 4*time.Minute); !ok {
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "service-settings", stack, string(data))); err != nil {
		log.Printf("audit: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// serviceSecret sets one of a service's secrets, such as a search provider's key, or removes it with an empty value.
// It's never shown again, and the audit log says only that it changed.
func (s *Server) serviceSecret(w http.ResponseWriter, r *http.Request, p *Principal) {
	stack, name := r.PathValue("stack"), r.PathValue("name")
	if !s.stackExists(stack) {
		fail(w, http.StatusNotFound, "not-found", "no such service")
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	data, _ := json.Marshal(map[string]string{"name": name, "value": strings.TrimSpace(body.Value)})
	if _, ok := s.askWithin(w, r, rootd.Request{Verb: "service-secret", Target: stack, Values: data}, time.Minute); !ok {
		return
	}
	outcome := "set"
	if strings.TrimSpace(body.Value) == "" {
		outcome = "removed"
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "service-secret", stack+"/"+name, outcome)); err != nil {
		log.Printf("audit: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
