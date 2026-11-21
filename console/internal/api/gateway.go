package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"patai/console/internal/rootd"
	"patai/console/internal/store"
	"patai/console/internal/workloads"
)

// The gateway is the only way in to an app's APIs (docs/apps.md, The gateway): the services listen on
// loopback. It takes an API key, or the session of someone who can run apps. It
// sends each path to the service behind the API that has it, if that API and its app are on, keeps each key to its
// limits, and logs each request with what it used. Never what was asked or answered.

// view is the apps as the gateway sees them: the APIs that are on, by path, and each service's models.
type view struct {
	routes   map[string]route             // the APIs that are on, for owners' keys and people
	known    map[string]string            // every such API's path, on or not, to its title
	own      map[string]map[string]route  // the APIs that are on of each app with a key of its own, by app
	keys     map[string]string            // each app's key, as its SHA-256 in hex, to the app
	titles   map[string]string            // each app's title
	models   map[string][]string          // each service's models, the one it runs first
	settings map[string]map[string]any    // each service's settings, for the model it runs
	ui       map[int]bool                 // the ports of web UIs whose apps are on
	people   map[string]map[string]string // each app's APIs that are on whose service is run for each person: route to service
	running  map[string][]string          // each service run for each person: the people whose runs
	node     map[string]bool              // the apps whose manifest asks for GET /node
}

// number is one of a service's settings that's a number, or fallback without it.
func (v *view) number(service, name string, fallback float64) float64 {
	if value, ok := v.settings[service][name].(float64); ok {
		return value
	}
	return fallback
}

// choice is one of a service's settings that's a word, or fallback without it.
func (v *view) choice(service, name, fallback string) string {
	if value, ok := v.settings[service][name].(string); ok && value != "" {
		return value
	}
	return fallback
}

type route struct {
	app, api, service string
	target            *url.URL
	model             string // the service its model is logged from: its own, or the app's chat for one with none
	unmetered         bool   // what it answers isn't a model's, such as search results, so it uses no tokens
}

// find is the route a path takes, and the route's own path: the path itself, or the longest route ending in / that it's
// under.
// stored is a route that takes or gives files rather than a model's answer.
func stored(path string) bool {
	for _, prefix := range []string{"/v1/audio/", "/v1/files", "/v1/containers"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func find[T any](routes map[string]T, path string) (T, string, bool) {
	if found, ok := routes[path]; ok {
		return found, path, true
	}
	var found T
	best := ""
	for prefix, candidate := range routes {
		if strings.HasSuffix(prefix, "/") && strings.HasPrefix(path, prefix) && len(prefix) > len(best) {
			found, best = candidate, prefix
		}
	}
	return found, best, best != ""
}

// How long what's chosen is taken as read. A change takes a job of seconds to minutes anyway.
const viewFor = 5 * time.Second

type views struct {
	mu   sync.Mutex
	at   time.Time
	last *view
}

// view reads what's chosen through patai-rootd, at most every few seconds.
func (s *Server) view(ctx context.Context) (*view, error) {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if s.views.last != nil && time.Since(s.views.at) < viewFor {
		return s.views.last, nil
	}
	ctx, cancel := context.WithTimeout(ctx, rootdWait)
	defer cancel()
	resp, err := s.Rootd.Call(ctx, rootd.Request{Verb: "apps-shown"})
	if err != nil {
		return nil, err
	}
	var shown workloads.Shown
	if err := json.Unmarshal([]byte(resp.Output), &shown); err != nil {
		return nil, err
	}
	v := &view{routes: map[string]route{}, known: map[string]string{}, own: map[string]map[string]route{},
		keys: map[string]string{}, titles: map[string]string{}, models: map[string][]string{},
		settings: map[string]map[string]any{}, ui: map[int]bool{}, people: map[string]map[string]string{},
		running: shown.People, node: map[string]bool{}}
	for _, app := range workloads.ReadApps(s.AppsPath) {
		on := slices.Contains(shown.Chosen, app.Name)
		v.titles[app.Name] = app.Title
		v.node[app.Name] = app.Node
		routes := v.routes
		if app.Key != "" {
			routes = map[string]route{}
			v.own[app.Name] = routes
			if hash, ok := shown.Keys[app.Name]; ok {
				v.keys[hash] = app.Name
			}
		}
		chat := ""
		for _, api := range app.APIs {
			if api.Always {
				chat = api.Service
			}
		}
		for _, api := range app.APIs {
			if app.Key == "" {
				for _, path := range api.Routes {
					v.known[path] = api.Title
				}
			}
			if !on || !api.Always && !slices.Contains(shown.APIs[app.Name], api.Name) {
				continue
			}
			// Only an app's own server, with its key, says whose a request is (people.go).
			if api.Person {
				if app.Key != "" {
					if v.people[app.Name] == nil {
						v.people[app.Name] = map[string]string{}
					}
					for _, path := range api.Routes {
						v.people[app.Name][path] = api.Service
					}
				}
				continue
			}
			target := serviceAddress(s.RecipesPath, api.Service)
			if target == nil {
				continue
			}
			model, unmetered := api.Service, false
			if lib, ok := shown.Models[model]; !ok || len(lib.Have) == 0 && lib.Use == "" {
				// A service with no model: the agentic API answers with chat's, and an app's own search with none.
				model = chat
				if app.Key != "" {
					model, unmetered = "", true
				}
			}
			// In the manifest's order, so a later API that's on takes a route an earlier one has. Audio is no tokens: a
			// recording goes one way, and text or another recording comes back. Nor are files, which are stored, not read
			// by a model: an upload would otherwise count as tokens by its size.
			for _, path := range api.Routes {
				routes[path] = route{app: app.Name, api: api.Name, service: api.Service, target: target, model: model,
					unmetered: unmetered || stored(path)}
			}
		}
		if on && app.UI != nil {
			v.ui[app.UI.Port] = true
		}
	}
	for name, lib := range shown.Models {
		models := []string{}
		if lib.Use != "" {
			models = append(models, lib.Use)
		}
		for _, model := range lib.Have {
			if !slices.Contains(models, model) {
				models = append(models, model)
			}
		}
		v.models[name] = models
	}
	for name, settings := range shown.Settings {
		v.settings[name] = settings.Values
	}
	s.views.last, s.views.at = v, time.Now()
	return v, nil
}

// serviceAddress is where a service answers, from its ready check, which is on loopback.
func serviceAddress(recipes, name string) *url.URL {
	m, err := workloads.ReadManifest(recipes, name)
	if err != nil || !strings.HasPrefix(m.Ready, "http://127.0.0.1:") {
		return nil
	}
	ready, err := url.Parse(m.Ready)
	if err != nil {
		return nil
	}
	return &url.URL{Scheme: ready.Scheme, Host: ready.Host}
}

// apiError answers in OpenAI's shape, which is what an API's clients read.
func apiError(w http.ResponseWriter, status int, kind, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": code}})
}

// caller is who a request to an API is from, and what they may use. A key has limits, and a person has none.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (*Principal, *store.Limits, bool) {
	if !s.usable(w) {
		return nil, nil, false
	}
	p := s.principal(r)
	if p == nil {
		apiError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "send an API key as a bearer token")
		return nil, nil, false
	}
	if !p.isKey() && !p.has("operator") {
		apiError(w, http.StatusForbidden, "invalid_request_error", "forbidden", "your role can't use the apps")
		return nil, nil, false
	}
	// Everything here can run a model, so a page on another site mustn't reach it through a session, and that
	// includes a WebSocket, which browsers open across origins.
	if p.Kind == "session" && r.Header.Get("Origin") != "" && !sameOrigin(r) {
		apiError(w, http.StatusForbidden, "invalid_request_error", "origin", "this request came from another site")
		return nil, nil, false
	}
	if p.Kind != "token" {
		return p, &store.Limits{}, true
	}
	limits, err := s.Store.KeyLimits(p.TokenID)
	if err != nil {
		log.Printf("store: %v", err)
		apiError(w, http.StatusServiceUnavailable, "server_error", "storage", "the unit can't read this key's limits")
		return nil, nil, false
	}
	return p, &limits, true
}

// strip takes the caller's credentials off a request, so no service sees them.
func strip(r *http.Request) {
	r.Header.Del("Authorization")
	stripCookie(r, sessionCookie)
}

// apis serves /v1/.
func (s *Server) apis() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, limits, ok := s.caller(w, r)
		if !ok {
			return
		}
		v, err := s.view(r.Context())
		if err != nil {
			apiError(w, http.StatusServiceUnavailable, "server_error", "unavailable", "the unit's root helper isn't answering")
			return
		}
		if r.URL.Path == "/v1/models" || strings.HasPrefix(r.URL.Path, "/v1/models/") {
			var outside []modelEntry
			if len(limits.APIs) == 0 || slices.Contains(limits.APIs, outsideAPI) {
				outside = s.outsideModels("ai-api")
			}
			s.listModels(w, r, v, limits, outside)
			return
		}
		if r.URL.Path == voicesPath {
			s.listVoices(w, v, v.routes)
			return
		}
		// Owners' keys reach models from services while the owner lets "ai-api" use them.
		if s.toOutside(w, r, "ai-api", keyOf(p), limits) {
			return
		}
		rt, endpoint, ok := find(v.routes, r.URL.Path)
		if !ok {
			if title, _, known := find(v.known, r.URL.Path); known {
				apiError(w, http.StatusNotFound, "invalid_request_error", "api_off", title+" is off on this unit")
			} else {
				apiError(w, http.StatusNotFound, "invalid_request_error", "unknown_url", "no API here has that path")
			}
			return
		}
		s.forward(w, r, v, rt, endpoint, keyOf(p), limits)
	})
}

// forward sends a request on its route to the service behind it, held to the key's limits. From here on, a request is
// logged, refused or not, by its route: a stored response's id isn't for the log. The model is the one the service runs,
// not what the answer says: llama.cpp repeats the request's.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, v *view, rt route, endpoint string, key keyRef, limits *store.Limits) {
	rec := &store.APIRequest{KeyID: key.id, Key: key.name, API: rt.api, Endpoint: endpoint}
	if models := v.models[rt.model]; len(models) > 0 && rt.api != "images" {
		rec.Model = models[0]
	}
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	// Deferred, as ReverseProxy ends a stream whose client has gone with a panic, which net/http expects.
	defer func() {
		if rec.Status == 0 {
			rec.Status = sw.status
		}
		if r.Context().Err() != nil && rec.Status < 400 {
			rec.Status = 499 // the client went before the end
		}
		if rec.Status == 0 {
			rec.Status = http.StatusOK
		}
		rec.At = start.UTC().Format(time.RFC3339Nano)
		rec.Millis = time.Since(start).Milliseconds()
		s.book.add(*rec)
	}()
	if len(limits.APIs) > 0 && !slices.Contains(limits.APIs, rt.api) {
		apiError(sw, http.StatusForbidden, "invalid_request_error", "api_not_allowed", "this key can't use "+rt.api)
		return
	}
	release, ok := s.admit(sw, r, key, limits, rt.api)
	if !ok {
		return
	}
	defer release()
	strip(r)
	r.Header.Set(callerHeader, key.caller)
	pinBackground(r, v, rt, endpoint)
	// A WebSocket's frames would pass the meter uncounted.
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		apiError(sw, http.StatusBadRequest, "invalid_request_error", "websocket", "use HTTP here, streamed if you like")
		return
	}
	if rt.api == "images" {
		s.images(sw, r, rt, v, rec)
		return
	}
	if rt.unmetered {
		s.proxy(rt.target).ServeHTTP(sw, r)
		return
	}
	sent := &counted{ReadCloser: r.Body}
	r.Body = sent
	defer func() {
		// A stream cut short has no usage: its tokens are counted from what was sent each way.
		if rec.PromptTokens == 0 && rec.CompletionTokens == 0 && (sent.n > 0 || rec.Chunks > 0) {
			rec.PromptTokens, rec.CompletionTokens = (sent.n+3)/4, rec.Chunks
		}
	}()
	s.proxy(rt.target).ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), meteredKey{}, metered{rec, sent})))
}

// backgroundHeader marks a request nobody is waiting on, such as Chat's scheduled work, patbot's and its side calls
// (patai_chat docs/patbot.md, Thinking ahead): it goes to the model server's last slot, so the conversations people are
// in keep the others, and llama-server's cache of them.
const backgroundHeader = "X-Patai-Priority"

// pinBackground puts a background request for a completion on llama-server's last slot, as `id_slot`, when its
// service answers two or more requests at once. One that names a slot already, or isn't JSON, goes as it is. The header
// never reaches the service.
func pinBackground(r *http.Request, v *view, rt route, endpoint string) {
	background := strings.EqualFold(r.Header.Get(backgroundHeader), "background")
	r.Header.Del(backgroundHeader)
	if !background || r.Body == nil || endpoint != "/v1/chat/completions" && endpoint != "/v1/completions" {
		return
	}
	parallel, ok := v.settings[rt.service]["parallel"].(float64)
	if !ok || parallel < 2 {
		return
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	set := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(body, &fields) != nil || fields == nil || fields["id_slot"] != nil {
		set(body)
		return
	}
	fields["id_slot"] = json.RawMessage(strconv.Itoa(int(parallel) - 1))
	pinned, err := json.Marshal(fields)
	if err != nil {
		set(body)
		return
	}
	set(pinned)
}

// counted is a request body, and how much of it was read.
type counted struct {
	io.ReadCloser
	n int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

type keyRef struct {
	id   int64
	name string
	// caller is who the service is told the request is from (callerHeader): a key by its id, a person by name, or an
	// app by its name.
	caller string
}

// callerHeader tells a service who a request is from, so what it keeps can be the caller's alone, as agentic-api keeps
// a code interpreter's containers. The gateway sets it on every request it sends on, over whatever a client sent.
const callerHeader = "X-Patai-Caller"

// keyOf is how a request is logged: a key by its id and name, and a person by name, with no id.
func keyOf(p *Principal) keyRef {
	if p.Kind == "token" {
		return keyRef{p.TokenID, strings.TrimPrefix(p.Name, "token:"), fmt.Sprintf("key:%d", p.TokenID)}
	}
	return keyRef{0, p.Name, "person:" + p.Name}
}

// appRef is an app's own key, as the gateway for apps logs it.
func appRef(app, title string) keyRef {
	return keyRef{0, title, "app:" + app}
}

// admit holds a key to its limits: requests at once, the size of one, and what it may use in a day. A request can
// take a key past a day's limit, as what it uses is only known once it's answered, and the next is refused.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, key keyRef, limits *store.Limits, api string) (func(), bool) {
	if key.id == 0 {
		return func() {}, true
	}
	if limits.MaxBody > 0 {
		if r.ContentLength > limits.MaxBody {
			apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
				"this key's requests can be "+strconv.FormatInt(limits.MaxBody, 10)+" bytes at most")
			return nil, false
		}
		r.Body = http.MaxBytesReader(w, r.Body, limits.MaxBody)
	}
	if limits.TokensPerDay > 0 || limits.ImagesPerDay > 0 {
		tokens, images, err := s.book.used(s.Store, key.id)
		if err != nil {
			log.Printf("store: %v", err)
			apiError(w, http.StatusServiceUnavailable, "server_error", "storage", "the unit can't read this key's usage")
			return nil, false
		}
		if limits.TokensPerDay > 0 && api != "images" && tokens >= limits.TokensPerDay ||
			limits.ImagesPerDay > 0 && api == "images" && images >= limits.ImagesPerDay {
			w.Header().Set("Retry-After", strconv.Itoa(untilTomorrow()))
			apiError(w, http.StatusTooManyRequests, "rate_limit_error", "daily_limit", "this key has used its day's limit")
			return nil, false
		}
	}
	s.inflight.Lock()
	defer s.inflight.Unlock()
	if s.inflight.count == nil {
		s.inflight.count = map[int64]int64{}
	}
	if limits.MaxConcurrent > 0 && s.inflight.count[key.id] >= limits.MaxConcurrent {
		apiError(w, http.StatusTooManyRequests, "rate_limit_error", "too_many_at_once",
			"this key can make "+strconv.FormatInt(limits.MaxConcurrent, 10)+" requests at once")
		return nil, false
	}
	s.inflight.count[key.id]++
	return func() {
		s.inflight.Lock()
		defer s.inflight.Unlock()
		if s.inflight.count[key.id]--; s.inflight.count[key.id] <= 0 {
			delete(s.inflight.count, key.id)
		}
	}, true
}

func untilTomorrow() int {
	now := time.Now().UTC()
	return int(now.Truncate(24*time.Hour).Add(24*time.Hour).Sub(now).Seconds()) + 1
}

type inflight struct {
	sync.Mutex
	count map[int64]int64
}

// listModels is /v1/models: each model an API that's on runs, and the images API's checkpoints. A model's architecture
// is what its service says, as llama.cpp does: what it takes in, so an app knows whether the chat model sees photos.
// listModels is the models the unit runs for the APIs the caller may use, and `outside`, those of services it may.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request, v *view, limits *store.Limits, outside []modelEntry) {
	type model = modelEntry
	list := []model{}
	seen := map[string]bool{}
	for _, path := range sortedKeys(v.routes) {
		rt := v.routes[path]
		if seen[rt.service] || len(limits.APIs) > 0 && !slices.Contains(limits.APIs, rt.api) {
			continue
		}
		seen[rt.service] = true
		// Each service runs one model, its first: the rest are kept, or are its parts.
		names := v.models[rt.service]
		if len(names) > 1 {
			names = names[:1]
		}
		for _, name := range names {
			list = append(list, model{ID: name, Object: "model", OwnedBy: s.Hostname,
				Architecture: architecture(r.Context(), rt.target, name)})
		}
	}
	list = append(list, outside...)
	if id := strings.TrimPrefix(r.URL.Path, "/v1/models/"); id != r.URL.Path {
		for _, m := range list {
			if m.ID == id {
				writeJSON(w, http.StatusOK, m)
				return
			}
		}
		apiError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "no model here is called that")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list})
}

// architecture is the model's own /v1/models entry's, which llama.cpp gives as {"input_modalities": ["text", "image"],
// "output_modalities": ["text"]}, or nil from a service that doesn't say, or isn't answering yet.
func architecture(ctx context.Context, service *url.URL, name string) json.RawMessage {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, service.JoinPath("/v1/models").String(), nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID           string          `json:"id"`
			Architecture json.RawMessage `json:"architecture"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list) != nil {
		return nil
	}
	for _, m := range list.Data {
		if m.ID == name && json.Valid(m.Architecture) && bytes.HasPrefix(bytes.TrimSpace(m.Architecture), []byte("{")) {
			return m.Architecture
		}
	}
	return nil
}

// voicesPath lists the text-to-speech voices, which OpenAI's API has no path for.
const voicesPath = "/v1/audio/voices"

// listVoices is the voices the speech API that's on has, its default first, by their titles in the recipe.
func (s *Server) listVoices(w http.ResponseWriter, v *view, routes map[string]route) {
	rt, ok := routes["/v1/audio/speech"]
	if !ok {
		apiError(w, http.StatusNotFound, "invalid_request_error", "api_off", "text to speech is off on this unit")
		return
	}
	titles := map[string]string{}
	if m, err := workloads.ReadManifest(s.RecipesPath, rt.service); err == nil {
		for _, model := range m.Models {
			titles[model.Name] = model.Title
		}
	}
	type voice struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	list := []voice{}
	for _, name := range v.models[rt.service] {
		title := titles[name]
		if title == "" {
			title = name
		}
		list = append(list, voice{ID: name, Name: title})
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": list})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

type meteredKey struct{}

// metered is what the meter needs of a request: its line in the log, and how much it sent.
type metered struct {
	rec  *store.APIRequest
	sent *counted
}

// proxy passes a request to a service as it is, streamed, and meters the answer on its way back.
func (s *Server) proxy(target *url.URL) *httputil.ReverseProxy {
	s.proxies.Lock()
	defer s.proxies.Unlock()
	if s.proxies.byHost == nil {
		s.proxies.byHost = map[string]*httputil.ReverseProxy{}
	}
	if proxy, ok := s.proxies.byHost[target.Host]; ok {
		return proxy
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if r.Context().Err() != nil {
			// The client went: nobody reads this, and the log says so.
			w.WriteHeader(499)
			return
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "that request is too big for this key")
			return
		}
		apiError(w, http.StatusBadGateway, "server_error", "service_down", "the service behind this API isn't answering")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if m, ok := resp.Request.Context().Value(meteredKey{}).(metered); ok {
			resp.Body = newMeter(resp.Body, m.rec, m.sent, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"))
		}
		return nil
	}
	s.proxies.byHost[target.Host] = proxy
	return proxy
}

type proxies struct {
	sync.Mutex
	byHost map[string]*httputil.ReverseProxy
}

// meter reads what an answer used, as it passes. It reads usage only where an answer puts it: at the top of the JSON, or
// of a stream's event, in a Responses event's response, or in Anthropic's message_start's message. So nothing a request
// has the answer repeat, such as its tools or metadata, can stand in for it. A JSON answer is read as it passes, keeping
// none of it, so one of any size is counted. A stream's counts are taken field by field: Anthropic's give the input at
// the start and the output at the end. llama.cpp's timings stand in where there's no usage. An answer with neither is
// counted from its size, a token for each 4 bytes each way, and a stream's from its events, a token each, as is an event
// too big to keep.
type meter struct {
	body    io.ReadCloser
	rec     *store.APIRequest
	sent    *counted // the request's body, for an answer that says nothing
	stream  bool
	buf     []byte // the stream's line so far
	over    bool   // it outgrew meterKeep
	size    int64  // the answer's bytes
	used    counts // from usage
	timed   counts // from timings
	done    bool
	pipe    *io.PipeWriter // to scan, which reads a JSON answer
	scanned chan struct{}
}

func newMeter(body io.ReadCloser, rec *store.APIRequest, sent *counted, stream bool) *meter {
	m := &meter{body: body, rec: rec, sent: sent, stream: stream}
	if !stream {
		reader, writer := io.Pipe()
		m.pipe, m.scanned = writer, make(chan struct{})
		go m.scan(reader)
	}
	return m
}

// scan reads a JSON answer as it passes, for its top-level usage and timings, skipping the rest token by token.
func (m *meter) scan(r io.Reader) {
	defer close(m.scanned)
	// Whatever's left, so the answer never waits on this.
	defer io.Copy(io.Discard, r)
	dec := json.NewDecoder(r)
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		return
	}
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return
		}
		if key == "usage" || key == "timings" {
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				return
			}
			m.take(append(append([]byte(`{"`+key+`":`), raw...), '}'))
		} else if skip(dec) != nil {
			return
		}
	}
}

// skip passes over one JSON value.
func skip(dec *json.Decoder) error {
	depth := 0
	for {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

type counts struct{ prompt, completion *int64 }

const meterKeep = 16 << 20

func (m *meter) Read(p []byte) (int, error) {
	n, err := m.body.Read(p)
	if n > 0 {
		m.size += int64(n)
		if m.stream {
			m.lines(p[:n])
		} else {
			m.pipe.Write(p[:n])
		}
	}
	return n, err
}

func (m *meter) keep(p []byte) {
	if m.over {
		return
	}
	if len(m.buf)+len(p) > meterKeep {
		m.over, m.buf = true, nil
		return
	}
	m.buf = append(m.buf, p...)
}

func (m *meter) lines(p []byte) {
	for len(p) > 0 {
		line, rest, found := bytes.Cut(p, []byte("\n"))
		m.keep(line)
		if !found {
			return
		}
		m.event()
		m.buf, m.over = m.buf[:0], false
		p = rest
	}
}

func (m *meter) event() {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(m.buf), []byte("data:"))
	if !ok && !m.over {
		return
	}
	if data = bytes.TrimSpace(data); bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	m.rec.Chunks++
	if !m.over {
		m.take(data)
	}
}

// usage is an answer's, in OpenAI's shapes or Anthropic's, whose input leaves out what it read from its cache.
type usage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheRead        *int64 `json:"cache_read_input_tokens"`
	CacheCreation    *int64 `json:"cache_creation_input_tokens"`
}

// llama.cpp's: the prompt tokens it read, less those it had cached.
type timings struct {
	CacheN     *int64 `json:"cache_n"`
	PromptN    *int64 `json:"prompt_n"`
	PredictedN *int64 `json:"predicted_n"`
}

// answer is where usage can be in an answer or an event.
type answer struct {
	Usage    *usage   `json:"usage"`
	Timings  *timings `json:"timings"`
	Response *struct {
		Usage *usage `json:"usage"`
	} `json:"response"`
	Message *struct {
		Usage *usage `json:"usage"`
	} `json:"message"`
}

func (m *meter) take(data []byte) {
	var a answer
	if json.Unmarshal(data, &a) != nil {
		return
	}
	for _, u := range []*usage{a.Usage, responseUsage(a), messageUsage(a)} {
		if u == nil {
			continue
		}
		if u.PromptTokens != nil {
			m.used.prompt = nonNegative(*u.PromptTokens)
		} else if u.InputTokens != nil {
			m.used.prompt = nonNegative(*u.InputTokens + value(u.CacheRead) + value(u.CacheCreation))
		}
		if u.CompletionTokens != nil {
			m.used.completion = nonNegative(*u.CompletionTokens)
		} else if u.OutputTokens != nil {
			m.used.completion = nonNegative(*u.OutputTokens)
		}
	}
	if t := a.Timings; t != nil {
		if t.CacheN != nil || t.PromptN != nil {
			m.timed.prompt = nonNegative(value(t.CacheN) + value(t.PromptN))
		}
		if t.PredictedN != nil {
			m.timed.completion = nonNegative(*t.PredictedN)
		}
	}
}

func responseUsage(a answer) *usage {
	if a.Response == nil {
		return nil
	}
	return a.Response.Usage
}

func messageUsage(a answer) *usage {
	if a.Message == nil {
		return nil
	}
	return a.Message.Usage
}

func value(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

func nonNegative(n int64) *int64 {
	n = max(n, 0)
	return &n
}

// finish puts the counts in the log: usage's, timings' without it, or the sizes without either.
func (m *meter) finish() {
	switch {
	case m.used.prompt != nil || m.used.completion != nil:
		m.rec.PromptTokens, m.rec.CompletionTokens = value(m.used.prompt), value(m.used.completion)
	case m.timed.prompt != nil || m.timed.completion != nil:
		m.rec.PromptTokens, m.rec.CompletionTokens = value(m.timed.prompt), value(m.timed.completion)
	default:
		if m.sent != nil {
			m.rec.PromptTokens = (m.sent.n + 3) / 4
		}
		m.rec.CompletionTokens = (m.size + 3) / 4
		if m.stream {
			m.rec.CompletionTokens = m.rec.Chunks
		}
	}
}

func (m *meter) Close() error {
	if !m.done {
		m.done = true
		if m.stream {
			m.event()
		} else {
			m.pipe.Close()
			<-m.scanned
		}
		m.finish()
		m.buf = nil
	}
	return m.body.Close()
}

// statusWriter keeps the status an answer had, and still flushes a stream as it goes.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// book is the request log on its way to the store, and each key's use today, for its limits. Lines are written a
// batch at a time, every few seconds: a power cut loses those seconds, and a request doesn't wait on the disk.
type book struct {
	mu      sync.Mutex
	day     string
	byKey   map[int64]*dayUse
	pending []store.APIRequest
	once    sync.Once
}

type dayUse struct{ tokens, images int64 }

const (
	bookEvery   = 2 * time.Second
	bookPending = 10000 // what waits for the store at most, if it's failing
)

func (b *book) today() {
	if day := store.Day(time.Now()); day != b.day {
		b.day, b.byKey = day, map[int64]*dayUse{}
	}
}

// used is what the key has used today, from the store the first time it's asked, then counted here.
func (b *book) used(st *store.Store, key int64) (int64, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.today()
	if u, ok := b.byKey[key]; ok {
		return u.tokens, u.images, nil
	}
	tokens, images, err := st.UsedOn(b.day, key)
	if err != nil {
		return 0, 0, err
	}
	// What's still to be written counts as well.
	for _, r := range b.pending {
		if r.KeyID == key && strings.HasPrefix(r.At, b.day) {
			tokens += r.PromptTokens + r.CompletionTokens
			images += r.Images
		}
	}
	b.byKey[key] = &dayUse{tokens, images}
	return tokens, images, nil
}

func (b *book) add(r store.APIRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.today()
	if u, ok := b.byKey[r.KeyID]; ok && strings.HasPrefix(r.At, b.day) {
		u.tokens += r.PromptTokens + r.CompletionTokens
		u.images += r.Images
	}
	if len(b.pending) < bookPending {
		b.pending = append(b.pending, r)
	}
}

// KeepRequestLog writes the gateway's request log to the store, every few seconds, for as long as the console runs.
func (s *Server) KeepRequestLog() { s.book.keep(s.Store) }

func (b *book) keep(st *store.Store) {
	b.once.Do(func() {
		go func() {
			for range time.Tick(bookEvery) {
				b.flush(st)
			}
		}()
	})
}

func (b *book) flush(st *store.Store) {
	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := st.AddAPIRequests(batch); err != nil {
		log.Printf("request log: %v", err)
		b.mu.Lock()
		if len(b.pending)+len(batch) <= bookPending {
			b.pending = append(batch, b.pending...)
		}
		b.mu.Unlock()
	}
}
