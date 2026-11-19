package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"patai/console/internal/durable"
	"patai/console/internal/store"
	"patai/console/internal/workloads"
)

// Models from services: chat models of outside services, such as OpenRouter, OpenAI and Anthropic, which an owner adds
// with a key of theirs, for the apps they choose (docs/apps.md, Models from services). An app asks for one by its name
// here, the service's id, a slash and the service's own name for it, such as openrouter/anthropic/claude-sonnet-4.5,
// and the gateway sends the request to the service with the key, which only the console holds. Each is logged as the
// unit's own models are, never with what was asked or answered. None is used until an owner adds a service and chooses
// an app for it: a unit is complete without them. What an app sends one is the app's to decide (Chat sends nothing
// personal unless the person types it).

// The services an owner can add by name, and their addresses. "custom" is any other that answers as OpenAI's does.
var outsideKinds = map[string]struct{ title, base string }{
	"openrouter": {"OpenRouter", "https://openrouter.ai/api/v1"},
	"openai":     {"OpenAI", "https://api.openai.com/v1"},
	"anthropic":  {"Anthropic", "https://api.anthropic.com/v1"},
	"custom":     {"", ""},
}

// The APIs an app can ask an outside model in, by their paths: chat completions, which every service takes, and the
// two makers' own, Anthropic's Messages and OpenAI's Responses, which their services and OpenRouter take. In its
// maker's own, a model is asked as it was made to be: Claude reads PDFs and takes its effort, and OpenAI's newest
// call tools, which over chat completions they don't.
var outsideAPIs = map[string]string{"/v1/chat/completions": "chat", "/v1/messages": "messages", "/v1/responses": "responses"}

// apiOf is the API a model is best asked in: its maker's own where its service takes it, or chat completions.
func apiOf(kind, model string) string {
	switch {
	case kind == "anthropic" || kind == "openrouter" && strings.HasPrefix(model, "anthropic/"):
		return "messages"
	case kind == "openai" || kind == "openrouter" && strings.HasPrefix(model, "openai/"):
		return "responses"
	}
	return "chat"
}

// takes says whether a kind of service takes an API: OpenRouter all three, for any model, Anthropic and OpenAI their
// own and chat completions, and any other chat completions.
func takes(kind, api string) bool {
	switch api {
	case "messages":
		return kind == "anthropic" || kind == "openrouter"
	case "responses":
		return kind == "openai" || kind == "openrouter"
	}
	return true
}

// The paths of each API at a service, after its address.
var apiPaths = map[string]string{"chat": "/chat/completions", "messages": "/messages", "responses": "/responses"}

// outsideAPI is the name outside models have where a key's limits name APIs, and in the request log.
const outsideAPI = "outside"

// A request to an outside model can carry photos, as data, so it's read whole before it's sent on, to this size.
const outsideBody = 32 << 20

var providerID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// provider is a service an owner added, and the models of it they chose, each as the service described it then.
type provider struct {
	ID     string         `json:"id"`
	Kind   string         `json:"kind"`
	Title  string         `json:"title"`
	Base   string         `json:"base"`
	Key    string         `json:"key,omitempty"`
	Models []outsideModel `json:"models"`
}

// chosen is the model of the service's called `name` that the owner chose, if they did.
func (p provider) chosen(name string) (outsideModel, bool) {
	at := slices.IndexFunc(p.Models, func(m outsideModel) bool { return m.ID == name })
	if at < 0 {
		return outsideModel{}, false
	}
	return p.Models[at], true
}

// outsideConfig is what's kept, in a file only the console reads: the services, and the apps that may use their models.
// "ai-api" among the apps is owners' keys, on 443.
type outsideConfig struct {
	Providers []provider `json:"providers"`
	Apps      []string   `json:"apps"`
}

type outside struct {
	mu   sync.Mutex
	read bool
	cfg  outsideConfig
}

func (s *Server) outsideNow() outsideConfig {
	s.outside.mu.Lock()
	defer s.outside.mu.Unlock()
	if !s.outside.read && s.OutsidePath != "" {
		if data, err := os.ReadFile(s.OutsidePath); err == nil {
			if err := json.Unmarshal(data, &s.outside.cfg); err != nil {
				log.Printf("models from services: %v", err)
			}
		}
	}
	s.outside.read = true
	cfg := s.outside.cfg
	cfg.Providers = slices.Clone(cfg.Providers)
	cfg.Apps = slices.Clone(cfg.Apps)
	return cfg
}

// changeOutside changes what's kept, under the lock, so two owners' changes don't undo each other.
func (s *Server) changeOutside(change func(*outsideConfig) error) (outsideConfig, error) {
	s.outsideNow()
	s.outside.mu.Lock()
	defer s.outside.mu.Unlock()
	cfg := s.outside.cfg
	cfg.Providers = slices.Clone(cfg.Providers)
	if err := change(&cfg); err != nil {
		return outsideConfig{}, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return outsideConfig{}, err
	}
	if err := durable.Write(s.OutsidePath, data, 0o600); err != nil {
		return outsideConfig{}, err
	}
	s.outside.cfg = cfg
	return cfg, nil
}

// find is the service and the model of it asked for by its name here, if it's one chosen.
func (c outsideConfig) find(model string) (provider, outsideModel, bool) {
	id, name, ok := strings.Cut(model, "/")
	if !ok {
		return provider{}, outsideModel{}, false
	}
	for _, p := range c.Providers {
		if m, ok := p.chosen(name); ok && p.ID == id {
			return p, m, true
		}
	}
	return provider{}, outsideModel{}, false
}

func (c outsideConfig) allows(app string) bool { return slices.Contains(c.Apps, app) }

// modelEntry is a model as /v1/models lists it. An outside one says so, whose it is, its name, the API it's best
// asked in (chat, messages or responses), and the effort levels it takes.
type modelEntry struct {
	ID           string          `json:"id"`
	Object       string          `json:"object"`
	Created      int64           `json:"created"`
	OwnedBy      string          `json:"owned_by"`
	Name         string          `json:"name,omitempty"`
	Architecture json.RawMessage `json:"architecture,omitempty"`
	Outside      bool            `json:"outside,omitempty"`
	Efforts      []string        `json:"efforts,omitempty"`
	// The level it answers at unasked, if known.
	DefaultEffort string `json:"default_effort,omitempty"`
	API           string `json:"api,omitempty"`
}

// outsideModels are the outside models an app may use, or owners' keys as "ai-api".
func (s *Server) outsideModels(app string) []modelEntry {
	cfg := s.outsideNow()
	if !cfg.allows(app) {
		return nil
	}
	list := []modelEntry{}
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			arch, _ := json.Marshal(map[string][]string{"input_modalities": m.Input, "output_modalities": m.Output})
			list = append(list, modelEntry{ID: p.ID + "/" + m.ID, Object: "model", Created: m.Created, OwnedBy: p.Title,
				Name: m.Name, Architecture: arch, Outside: true, Efforts: m.Efforts, DefaultEffort: m.DefaultEffort,
				API: apiOf(p.Kind, m.ID)})
		}
	}
	return list
}

// toOutside sends a request for an outside model, in any of the APIs, to its service, if it's one and the app may use
// it, and says whether it did. Any other request is left as it came, to go to the unit's own model.
func (s *Server) toOutside(w http.ResponseWriter, r *http.Request, app string, key keyRef, limits *store.Limits) bool {
	api, known := outsideAPIs[r.URL.Path]
	if r.Method != http.MethodPost || !known {
		return false
	}
	cfg := s.outsideNow()
	if len(cfg.Providers) == 0 || !cfg.allows(app) {
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, outsideBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "that request is too big")
		return true
	}
	var head struct {
		Model string `json:"model"`
	}
	json.Unmarshal(data, &head)
	p, chosen, ok := cfg.find(head.Model)
	if !ok {
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(data)), int64(len(data))
		return false
	}
	rec := &store.APIRequest{KeyID: key.id, Key: key.name, API: outsideAPI, Endpoint: r.URL.Path, Model: head.Model}
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		if rec.Status == 0 {
			rec.Status = sw.status
		}
		if r.Context().Err() != nil && rec.Status < 400 {
			rec.Status = 499
		}
		if rec.Status == 0 {
			rec.Status = http.StatusOK
		}
		rec.At = start.UTC().Format(time.RFC3339Nano)
		rec.Millis = time.Since(start).Milliseconds()
		s.book.add(*rec)
	}()
	if len(limits.APIs) > 0 && !slices.Contains(limits.APIs, outsideAPI) {
		apiError(sw, http.StatusForbidden, "invalid_request_error", "api_not_allowed", "this key can't use models from services")
		return true
	}
	if !takes(p.Kind, api) {
		apiError(sw, http.StatusBadRequest, "invalid_request_error", "api_not_taken", "this model's service doesn't take this API")
		return true
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil {
		apiError(sw, http.StatusBadRequest, "invalid_request_error", "bad_json", "that request isn't JSON")
		return true
	}
	body["model"], _ = json.Marshal(chosen.ID)
	switch {
	case api == "chat":
		effort(body, p.Kind, chosen)
		// OpenAI keeps what's sent to chat completions unless told not to, on accounts made since 2025.
		if _, set := body["store"]; p.Kind == "openai" && !set {
			body["store"] = json.RawMessage("false")
		}
	case api == "responses":
		// Nothing is kept at the service: OpenAI keeps a response unless told not to, and OpenRouter refuses one asked to.
		body["store"] = json.RawMessage("false")
	}
	out, _ := json.Marshal(body)
	strip(r)
	sent := &counted{ReadCloser: io.NopCloser(bytes.NewReader(out))}
	r.Body, r.ContentLength = sent, int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	defer func() {
		if rec.PromptTokens == 0 && rec.CompletionTokens == 0 && (sent.n > 0 || rec.Chunks > 0) {
			rec.PromptTokens, rec.CompletionTokens = (sent.n+3)/4, rec.Chunks
		}
	}()
	outsideProxy(p, api).ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), meteredKey{}, metered{rec, sent})))
	return true
}

// outsideProxy sends a request to a service's API with its key, and nothing of who on the unit asked: no cookies, no
// forwarded addresses, no key of the app's. Anthropic takes its key as X-Api-Key, and its Messages only that way.
func outsideProxy(p provider, api string) *httputil.ReverseProxy {
	target, _ := url.Parse(strings.TrimSuffix(p.Base, "/") + apiPaths[api])
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			for _, name := range []string{"Cookie", "Origin", "Referer", "X-User-Id", "X-Api-Key", "Authorization"} {
				pr.Out.Header.Del(name)
			}
			if p.Key != "" && (p.Kind != "anthropic" || api == "chat") {
				pr.Out.Header.Set("Authorization", "Bearer "+p.Key)
			}
			if p.Kind == "anthropic" {
				pr.Out.Header.Set("X-Api-Key", p.Key)
				pr.Out.Header.Set("Anthropic-Version", anthropicVersion)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				w.WriteHeader(499)
				return
			}
			apiError(w, http.StatusBadGateway, "server_error", "service_down", "the service isn't answering")
		},
		ModifyResponse: func(resp *http.Response) error {
			if m, ok := resp.Request.Context().Value(meteredKey{}).(metered); ok {
				resp.Body = newMeter(resp.Body, m.rec, m.sent, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"))
			}
			return nil
		},
	}
}

// checkBase is a custom service's address: https, or http on the unit's own network, by its address.
func checkBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("that isn't an address")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		return nil
	}
	return errors.New("use https, or http with an address on this network")
}

type providerJSON struct {
	ID     string         `json:"id"`
	Kind   string         `json:"kind"`
	Title  string         `json:"title"`
	Base   string         `json:"base"`
	HasKey bool           `json:"has_key"`
	Models []outsideModel `json:"models"`
}

type outsideJSON struct {
	Providers []providerJSON `json:"providers"`
	Apps      []string       `json:"apps"`
}

// shown is what an owner sees of what's kept: never a key, only whether there is one.
func (c outsideConfig) shown() outsideJSON {
	out := outsideJSON{Providers: []providerJSON{}, Apps: c.Apps}
	if out.Apps == nil {
		out.Apps = []string{}
	}
	for _, p := range c.Providers {
		models := p.Models
		if models == nil {
			models = []outsideModel{}
		}
		out.Providers = append(out.Providers, providerJSON{ID: p.ID, Kind: p.Kind, Title: p.Title, Base: p.Base,
			HasKey: p.Key != "", Models: models})
	}
	return out
}

func (s *Server) getOutside(w http.ResponseWriter, r *http.Request, p *Principal) {
	writeJSON(w, http.StatusOK, s.outsideNow().shown())
}

// outsideChange is an owner's change to models from services, kept and audited.
func (s *Server) outsideChange(w http.ResponseWriter, r *http.Request, p *Principal, action, target string, change func(*outsideConfig) error) {
	if s.OutsidePath == "" {
		fail(w, http.StatusConflict, "recovery", "the unit can't keep this change in recovery mode")
		return
	}
	cfg, err := s.changeOutside(change)
	var bad badRequest
	if errors.As(err, &bad) {
		fail(w, http.StatusBadRequest, bad.code, bad.message)
		return
	}
	if err != nil {
		s.failStore(w, err)
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, action, target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg.shown())
}

type badRequest struct{ code, message string }

func (b badRequest) Error() string { return b.message }

// putProvider adds a service, or changes one: its key is kept unless a new one is given.
func (s *Server) putProvider(w http.ResponseWriter, r *http.Request, p *Principal) {
	id := r.PathValue("id")
	var body struct {
		Kind  string `json:"kind"`
		Title string `json:"title"`
		Base  string `json:"base"`
		Key   string `json:"key"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	s.outsideChange(w, r, p, "outside-service", id, func(cfg *outsideConfig) error {
		kind, ok := outsideKinds[body.Kind]
		if !providerID.MatchString(id) || !ok {
			return badRequest{"bad-service", "give a service's id and kind: openrouter, openai, anthropic or custom"}
		}
		next := provider{ID: id, Kind: body.Kind, Title: strings.TrimSpace(body.Title), Base: kind.base, Key: strings.TrimSpace(body.Key)}
		if body.Kind == "custom" {
			next.Base = strings.TrimSuffix(strings.TrimSpace(body.Base), "/")
			if err := checkBase(next.Base); err != nil {
				return badRequest{"bad-address", err.Error()}
			}
		}
		if next.Title == "" {
			next.Title = kind.title
		}
		if next.Title == "" || len(next.Title) > 60 {
			return badRequest{"bad-title", "give the service a name"}
		}
		at := slices.IndexFunc(cfg.Providers, func(had provider) bool { return had.ID == id })
		if at >= 0 {
			had := cfg.Providers[at]
			if next.Key == "" {
				next.Key = had.Key
			}
			if had.Kind == next.Kind && had.Base == next.Base {
				next.Models = had.Models
			}
			cfg.Providers[at] = next
		} else {
			if next.Key == "" && next.Kind != "custom" {
				return badRequest{"no-key", "give the service's API key"}
			}
			cfg.Providers = append(cfg.Providers, next)
		}
		return nil
	})
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request, p *Principal) {
	id := r.PathValue("id")
	s.outsideChange(w, r, p, "outside-service-remove", id, func(cfg *outsideConfig) error {
		at := slices.IndexFunc(cfg.Providers, func(had provider) bool { return had.ID == id })
		if at < 0 {
			return badRequest{"not-found", "no such service"}
		}
		cfg.Providers = slices.Delete(cfg.Providers, at, at+1)
		return nil
	})
}

// putProviderModels chooses which of a service's models the apps are offered, by their names: each is kept as the
// service describes it now, or as it was when chosen before if the service doesn't list it or doesn't answer.
func (s *Server) putProviderModels(w http.ResponseWriter, r *http.Request, p *Principal) {
	id := r.PathValue("id")
	var body struct {
		Models []string `json:"models"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	names := []string{}
	for _, name := range body.Models {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if len(name) > 200 || strings.ContainsAny(name, " \t\n") {
			fail(w, http.StatusBadRequest, "bad-model", "that isn't a model's name")
			return
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) > 100 {
		fail(w, http.StatusBadRequest, "too-many", "choose 100 models at most")
		return
	}
	cfg := s.outsideNow()
	at := slices.IndexFunc(cfg.Providers, func(had provider) bool { return had.ID == id })
	if at < 0 {
		fail(w, http.StatusNotFound, "not-found", "no such service")
		return
	}
	listed, _ := listModels(r.Context(), cfg.Providers[at])
	s.outsideChange(w, r, p, "outside-models", id, func(cfg *outsideConfig) error {
		at := slices.IndexFunc(cfg.Providers, func(had provider) bool { return had.ID == id })
		if at < 0 {
			return badRequest{"not-found", "no such service"}
		}
		models := []outsideModel{}
		for _, name := range names {
			if i := slices.IndexFunc(listed, func(m outsideModel) bool { return m.ID == name }); i >= 0 {
				models = append(models, listed[i])
			} else if had, ok := cfg.Providers[at].chosen(name); ok {
				models = append(models, had)
			} else {
				unknown := outsideModel{ID: name, Name: name, Input: []string{"text"}, Output: []string{"text"}}
				unknown.fit()
				models = append(models, unknown)
			}
		}
		cfg.Providers[at].Models = models
		return nil
	})
}

// putOutsideApps chooses the apps that may use models from services. "ai-api" is owners' keys.
func (s *Server) putOutsideApps(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Apps []string `json:"apps"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	s.outsideChange(w, r, p, "outside-apps", strings.Join(body.Apps, ", "), func(cfg *outsideConfig) error {
		apps := []string{}
		for _, name := range body.Apps {
			if _, err := workloads.ReadApp(s.AppsPath, name); err != nil {
				return badRequest{"no-app", "there's no app called " + name}
			}
			if !slices.Contains(apps, name) {
				apps = append(apps, name)
			}
		}
		cfg.Apps = apps
		return nil
	})
}

// catalogue is the models a service has, described, to choose from.
func (s *Server) catalogue(w http.ResponseWriter, r *http.Request, p *Principal) {
	id := r.PathValue("id")
	cfg := s.outsideNow()
	at := slices.IndexFunc(cfg.Providers, func(had provider) bool { return had.ID == id })
	if at < 0 {
		fail(w, http.StatusNotFound, "not-found", "no such service")
		return
	}
	models, err := listModels(r.Context(), cfg.Providers[at])
	var failed serviceFailure
	if errors.As(err, &failed) {
		fail(w, http.StatusBadGateway, failed.code, failed.message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// effort puts the effort level an app asked for, as OpenAI's reasoning_effort, into the shape the model's service
// takes, if the model takes it; one it doesn't take is left out, and the service's default is used.
func effort(body map[string]json.RawMessage, kind string, m outsideModel) {
	raw, ok := body["reasoning_effort"]
	if !ok {
		return
	}
	delete(body, "reasoning_effort")
	var level string
	if json.Unmarshal(raw, &level) != nil || !slices.Contains(m.Efforts, level) {
		return
	}
	switch kind {
	case "openrouter":
		body["reasoning"], _ = json.Marshal(map[string]string{"effort": level})
	default:
		body["reasoning_effort"] = raw
	}
}
