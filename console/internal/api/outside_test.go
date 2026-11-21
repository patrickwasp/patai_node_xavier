package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// An outside service's stand-in: it answers chat completions as OpenAI's does, says what it was sent, and lists two
// models, one as OpenRouter describes them and one by its name alone.
func outsideService(t *testing.T) (*httptest.Server, chan *http.Request, chan string) {
	t.Helper()
	asked := make(chan *http.Request, 4)
	bodies := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data": [{"id": "m2"}, {"id": "m1", "name": "Maker: Model One", "context_length": 128000,
				"architecture": {"input_modalities": ["text", "image"], "output_modalities": ["text"]},
				"pricing": {"prompt": "0.000002", "completion": "0.00001"}, "supported_parameters": ["tools", "reasoning"],
				"reasoning": {"supported_efforts": ["high", "medium", "low"]}}]}`)
			return
		}
		data, _ := io.ReadAll(r.Body)
		asked <- r
		bodies <- string(data)
		io.WriteString(w, `{"choices": [{"message": {"content": "from outside"}}], "usage": {"prompt_tokens": 11, "completion_tokens": 2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, asked, bodies
}

func TestAnAppReachesOnlyTheOutsideModelsAnOwnerChoseForIt(t *testing.T) {
	r := newRig(t)
	local := make(chan string, 4)
	chat := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		local <- string(data)
		io.WriteString(w, `{"usage": {"prompt_tokens": 1, "completion_tokens": 1}}`)
	})
	key := r.chatApp(t, chat, nil, http.NotFoundHandler(), false)
	r.s.OutsidePath = filepath.Join(r.dir, "outside.json")
	owner, _ := r.claim()
	viewer := r.user(owner, "bo", "viewer")
	svc, asked, bodies := outsideService(t)

	// A service is added with its key, which is never shown again.
	want(t, r.do("PUT", "/api/v1/outside/services/lan", map[string]string{"kind": "custom", "title": "LAN", "base": svc.URL + "/v1", "key": "sk-test"}, opts{cookie: viewer}), 403)
	want(t, r.do("PUT", "/api/v1/outside/services/lan", map[string]string{"kind": "custom", "base": "http://93.184.215.14/v1"}, opts{cookie: owner}), 400)
	want(t, r.do("PUT", "/api/v1/outside/services/or", map[string]string{"kind": "openrouter"}, opts{cookie: owner}), 400)
	added := r.do("PUT", "/api/v1/outside/services/lan", map[string]string{"kind": "custom", "title": "LAN", "base": svc.URL + "/v1", "key": "sk-test"}, opts{cookie: owner})
	want(t, added, 200)
	if strings.Contains(added.Body.String(), "sk-test") || !strings.Contains(added.Body.String(), `"has_key":true`) {
		t.Fatal(added.Body.String())
	}
	listed := r.do("GET", "/api/v1/outside/services/lan/catalogue", nil, opts{cookie: owner})
	want(t, listed, 200)
	// Described, the best fits first.
	var catalogue struct {
		Models []outsideModel `json:"models"`
	}
	json.Unmarshal(listed.Body.Bytes(), &catalogue)
	if len(catalogue.Models) != 2 {
		t.Fatal(listed.Body.String())
	}
	if m := catalogue.Models[0]; m.ID != "m1" || m.Name != "Model One" || m.Fit != "good" || m.Context != 128000 ||
		*m.PriceIn != 2 || *m.PriceOut != 10 || !slices.Equal(m.Input, []string{"text", "image"}) ||
		!slices.Equal(m.Efforts, []string{"low", "medium", "high"}) {
		t.Fatal(listed.Body.String())
	}
	if m := catalogue.Models[1]; m.ID != "m2" || m.Fit != "unknown" || m.Tools != nil {
		t.Fatal(listed.Body.String())
	}
	want(t, r.do("PUT", "/api/v1/outside/services/lan/models", map[string]any{"models": []string{"m1"}}, opts{cookie: owner}), 200)

	fromApp := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://172.17.0.1:18000"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Cookie", sessionCookie+"=x")
		w := httptest.NewRecorder()
		r.s.AppsHandler().ServeHTTP(w, req)
		return w
	}
	ask := func(model string) *httptest.ResponseRecorder {
		return fromApp("POST", "/v1/chat/completions", `{"model": "`+model+`", "messages": [{"role": "user", "content": "hi"}]}`)
	}

	// Not until the owner chooses an app for them: until then it's the unit's own model that answers.
	want(t, ask("lan/m1"), 200)
	if got := <-local; !strings.Contains(got, `"lan/m1"`) {
		t.Fatal(got)
	}
	if strings.Contains(fromApp("GET", "/v1/models", "").Body.String(), "lan/m1") {
		t.Fatal("listed before it's chosen")
	}
	want(t, r.do("PUT", "/api/v1/outside/apps", map[string]any{"apps": []string{"nope"}}, opts{cookie: owner}), 400)
	want(t, r.do("PUT", "/api/v1/outside/apps", map[string]any{"apps": []string{"chat"}}, opts{cookie: owner}), 200)

	models := fromApp("GET", "/v1/models", "")
	var list struct {
		Data []modelEntry `json:"data"`
	}
	json.Unmarshal(models.Body.Bytes(), &list)
	if len(list.Data) != 2 || list.Data[1].ID != "lan/m1" || !list.Data[1].Outside || list.Data[1].OwnedBy != "LAN" ||
		list.Data[1].Name != "Model One" || !slices.Equal(list.Data[1].Efforts, []string{"low", "medium", "high"}) ||
		!strings.Contains(string(list.Data[1].Architecture), `"input_modalities":["text","image"]`) {
		t.Fatal(models.Body.String())
	}

	// The service gets its own name for the model and the key, and nothing of the app's or the unit's.
	w := ask("lan/m1")
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), "from outside") {
		t.Fatal(w.Body.String())
	}
	req, body := <-asked, <-bodies
	if req.Header.Get("Authorization") != "Bearer sk-test" || req.Header.Get("Cookie") != "" || req.Header.Get("X-Forwarded-For") != "" ||
		req.URL.Path != "/v1/chat/completions" {
		t.Fatalf("%s %v", req.URL.Path, req.Header)
	}
	if !strings.Contains(body, `"model":"m1"`) || !strings.Contains(body, `"content":"hi"`) {
		t.Fatal(body)
	}
	// An effort the model takes goes on; one it doesn't is left out.
	for effort, sent := range map[string]bool{"high": true, "max": false} {
		want(t, fromApp("POST", "/v1/chat/completions", `{"model": "lan/m1", "reasoning_effort": "`+effort+`", "messages": []}`), 200)
		<-asked
		if got := <-bodies; strings.Contains(got, `"reasoning_effort":"`+effort+`"`) != sent || (!sent && strings.Contains(got, "reasoning_effort")) {
			t.Fatal(effort, got)
		}
	}
	// A model of the service's that wasn't chosen is the unit's own model's to answer.
	want(t, ask("lan/m2"), 200)
	<-local
	r.s.book.flush(r.s.Store)
	logged := r.logged()
	found := false
	for _, line := range logged {
		if line.API == "outside" && line.Model == "lan/m1" && line.PromptTokens == 11 && line.CompletionTokens == 2 && line.Key == "Chat" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", logged)
	}

	// Removed, it's gone from the list.
	want(t, r.do("DELETE", "/api/v1/outside/services/lan", nil, opts{cookie: owner}), 200)
	if strings.Contains(fromApp("GET", "/v1/models", "").Body.String(), "lan/m1") {
		t.Fatal("still listed")
	}
}

// OpenRouter takes effort as reasoning.effort, which it passes on to each service as that service's own.
func TestEffortGoesToOpenRouterAsReasoning(t *testing.T) {
	m := outsideModel{ID: "openai/gpt-x", Efforts: []string{"low", "medium", "high"}}
	body := map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"medium"`)}
	effort(body, "openrouter", m)
	if _, left := body["reasoning_effort"]; left || string(body["reasoning"]) != `{"effort":"medium"}` {
		t.Fatalf("%s", body)
	}
}

// Anthropic's list gives each model's capabilities as a tree, and models.dev its price; OpenAI's names a model only, and
// models.dev says what it can do. OpenRouter's public list scores them, by models.dev's canonical names.
func TestEachServiceListIsDescribed(t *testing.T) {
	dev := &devList{models: map[string]map[string]devModel{"openai": {}, "anthropic": {}}, scores: map[string]float64{"anthropic/claude-x": 61.25}}
	var d devModel
	json.Unmarshal([]byte(`{"name": "Claude X", "canonical_model_id": "anthropic/claude-x", "cost": {"input": 4, "output": 20}}`), &d)
	dev.models["anthropic"]["claude-x"] = d
	json.Unmarshal([]byte(`{"name": "GPT X", "tool_call": true, "reasoning": true,
		"reasoning_options": [{"type": "effort", "values": ["none", "minimal", "low", "medium", "high", "xhigh"]}],
		"modalities": {"input": ["text", "image", "pdf"], "output": ["text"]}, "limit": {"context": 400000, "output": 128000}}`), &d)
	dev.models["openai"]["gpt-5.2"] = d
	dev.models["openai"]["gpt-6-sol"] = d

	var claude anthropicModel
	json.Unmarshal([]byte(`{"id": "claude-x", "display_name": "Claude X", "lifecycle": "active", "max_input_tokens": 1000000,
		"max_tokens": 128000, "capabilities": {"image_input": {"supported": true}, "pdf_input": {"supported": true},
		"thinking": {"supported": true}, "effort": {"supported": true, "low": {"supported": true}, "high": {"supported": true},
		"max": {"supported": false}}}}`), &claude)
	// In Anthropic's own Messages: photos, PDFs, and the effort levels it takes, at high unasked.
	m, ok := fromAnthropic(claude, dev)
	if !ok || m.Fit != "good" || !slices.Equal(m.Input, []string{"text", "image", "file"}) ||
		!slices.Equal(m.Efforts, []string{"low", "high"}) || m.DefaultEffort != "high" || *m.PriceIn != 4 || *m.Intelligence != 61.25 {
		t.Fatalf("%+v", m)
	}
	claude.Lifecycle = "retired"
	if _, ok := fromAnthropic(claude, dev); ok {
		t.Fatal("a retired model listed")
	}

	now := time.Now()
	if m, _ := fromOpenAI(openAIModel{ID: "gpt-5.2"}, dev, now); m.Fit != "good" || m.Name != "GPT X" ||
		!slices.Equal(m.Efforts, []string{"low", "medium", "high", "xhigh"}) || !slices.Equal(m.Input, []string{"text", "image", "file"}) {
		t.Fatalf("%+v", m)
	}
	// GPT-6 calls tools in OpenAI's own Responses, which the gateway asks it in.
	if m, _ := fromOpenAI(openAIModel{ID: "gpt-6-sol"}, dev, now); m.Fit != "good" || len(m.Efforts) != 4 {
		t.Fatalf("%+v", m)
	}
	if m, _ := fromOpenAI(openAIModel{ID: "text-embedding-3-large"}, dev, now); m.Fit != "not-chat" {
		t.Fatalf("%+v", m)
	}
	if m, _ := fromOpenAI(openAIModel{ID: "o9-tiny"}, dev, now); m.Fit != "unknown" {
		t.Fatalf("%+v", m)
	}
	if _, ok := fromOpenAI(openAIModel{ID: "gpt-4", ShutdownDate: "2020-01-01"}, dev, now); ok {
		t.Fatal("a model past its shutdown listed")
	}

	var routed openRouterModel
	json.Unmarshal([]byte(`{"id": "anthropic/claude-x", "name": "Anthropic: Claude X", "supported_parameters": ["tools", "reasoning"],
		"endpoints": ["/api/v1/chat/completions"], "pricing": {"prompt": "0.000004", "completion": "0.00002"},
		"reasoning": {"mandatory": true, "supported_efforts": ["max", "xhigh", "high", "medium", "low", "minimal"], "default_effort": "high"},
		"benchmarks": {"artificial_analysis": {"intelligence_index": 59.1}}}`), &routed)
	if m := fromOpenRouter(routed); m.Fit != "good" || m.Name != "Claude X" || m.Maker != "anthropic" || *m.Intelligence != 59.1 ||
		!slices.Equal(m.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) || m.DefaultEffort != "high" {
		t.Fatalf("%+v", m)
	}
	for _, id := range []string{"anthropic/claude-x:batch", "openrouter/auto"} {
		gone := routed
		gone.ID = id
		if id == "openrouter/auto" {
			gone.Pricing.Prompt = "-1"
		}
		if !gone.unlisted(now) {
			t.Fatal(id, "listed")
		}
	}
}

// Claude is asked in Anthropic's Messages and OpenAI's models in its Responses, at their service or OpenRouter, with the
// service's key the way it takes it; the rest in chat completions. A service is never asked in an API it doesn't take,
// and nothing sent to Responses is kept there.
func TestEachModelIsAskedInItsMakersAPI(t *testing.T) {
	r := newRig(t)
	key := r.chatApp(t, http.NotFoundHandler(), nil, http.NotFoundHandler(), false)
	r.s.OutsidePath = filepath.Join(r.dir, "outside.json")
	svc, asked, bodies := outsideService(t)
	_, err := r.s.changeOutside(func(c *outsideConfig) error {
		c.Providers = []provider{
			{ID: "cl", Kind: "anthropic", Title: "Anthropic", Base: svc.URL + "/v1", Key: "sk-ant", Models: []outsideModel{{ID: "claude-x"}}},
			{ID: "oa", Kind: "openai", Title: "OpenAI", Base: svc.URL + "/v1", Key: "sk-oa", Models: []outsideModel{{ID: "gpt-x"}}},
			{ID: "or", Kind: "openrouter", Title: "OpenRouter", Base: svc.URL + "/v1", Key: "sk-or",
				Models: []outsideModel{{ID: "anthropic/claude-x"}, {ID: "openai/gpt-x"}, {ID: "google/gemini-x"}}},
		}
		c.Apps = []string{"chat"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fromApp := func(path, model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://172.17.0.1:18000"+path, strings.NewReader(`{"model": "`+model+`", "store": true}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Api-Key", key)
		w := httptest.NewRecorder()
		r.s.AppsHandler().ServeHTTP(w, req)
		return w
	}

	listed := httptest.NewRequest("GET", "http://172.17.0.1:18000/v1/models", nil)
	listed.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	r.s.AppsHandler().ServeHTTP(w, listed)
	var list struct {
		Data []modelEntry `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	apis := map[string]string{}
	for _, m := range list.Data {
		apis[m.ID] = m.API
	}
	if apis["cl/claude-x"] != "messages" || apis["oa/gpt-x"] != "responses" || apis["or/anthropic/claude-x"] != "messages" ||
		apis["or/openai/gpt-x"] != "responses" || apis["or/google/gemini-x"] != "chat" {
		t.Fatal(w.Body.String())
	}

	for _, c := range []struct{ path, model, at, auth, apiKey, store string }{
		{"/v1/messages", "cl/claude-x", "/v1/messages", "", "sk-ant", `"store":true`},
		{"/v1/chat/completions", "cl/claude-x", "/v1/chat/completions", "Bearer sk-ant", "sk-ant", `"store":true`},
		{"/v1/responses", "oa/gpt-x", "/v1/responses", "Bearer sk-oa", "", `"store":false`},
		{"/v1/messages", "or/anthropic/claude-x", "/v1/messages", "Bearer sk-or", "", `"store":true`},
		{"/v1/responses", "or/openai/gpt-x", "/v1/responses", "Bearer sk-or", "", `"store":false`},
		{"/v1/chat/completions", "or/google/gemini-x", "/v1/chat/completions", "Bearer sk-or", "", `"store":true`},
	} {
		want(t, fromApp(c.path, c.model), 200)
		req, body := <-asked, <-bodies
		if req.URL.Path != c.at || req.Header.Get("Authorization") != c.auth || req.Header.Get("X-Api-Key") != c.apiKey ||
			!strings.Contains(body, c.store) {
			t.Fatalf("%s %s: %s %v %s", c.path, c.model, req.URL.Path, req.Header, body)
		}
		if c.apiKey == "sk-ant" && req.Header.Get("Anthropic-Version") == "" {
			t.Fatalf("%s: no Anthropic-Version", c.model)
		}
	}
	// Not in an API its service doesn't take.
	want(t, fromApp("/v1/messages", "oa/gpt-x"), 400)
	want(t, fromApp("/v1/responses", "cl/claude-x"), 400)
}

// What a service may have: only the chat until the owner says more, kinds of data they tick, or Like this node, which
// only a custom service can be. /v1/models tells the apps, and the same server at a new address starts again.
func TestWhatAServiceMayHave(t *testing.T) {
	r := newRig(t)
	chat := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"usage": {"prompt_tokens": 1, "completion_tokens": 1}}`)
	})
	key := r.chatApp(t, chat, nil, http.NotFoundHandler(), false)
	r.s.OutsidePath = filepath.Join(r.dir, "outside.json")
	owner, _ := r.claim()
	viewer := r.user(owner, "bo", "viewer")
	svc, _, _ := outsideService(t)
	want(t, r.do("PUT", "/api/v1/outside/services/lan", map[string]string{"kind": "custom", "title": "LAN", "base": svc.URL + "/v1"}, opts{cookie: owner}), 200)
	want(t, r.do("PUT", "/api/v1/outside/services/or", map[string]string{"kind": "openrouter", "key": "sk-or"}, opts{cookie: owner}), 200)
	want(t, r.do("PUT", "/api/v1/outside/services/lan/models", map[string]any{"models": []string{"m1"}}, opts{cookie: owner}), 200)
	want(t, r.do("PUT", "/api/v1/outside/apps", map[string]any{"apps": []string{"chat"}}, opts{cookie: owner}), 200)

	shown := func() map[string]json.RawMessage {
		w := r.do("GET", "/api/v1/outside", nil, opts{cookie: owner})
		want(t, w, 200)
		var out struct {
			Providers []struct {
				ID   string          `json:"id"`
				Data json.RawMessage `json:"data"`
			} `json:"providers"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		data := map[string]json.RawMessage{}
		for _, p := range out.Providers {
			data[p.ID] = p.Data
		}
		return data
	}
	listed := func() modelEntry {
		req := httptest.NewRequest("GET", "http://172.17.0.1:18000/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		r.s.AppsHandler().ServeHTTP(w, req)
		var list struct {
			Data []modelEntry `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &list)
		at := slices.IndexFunc(list.Data, func(m modelEntry) bool { return m.ID == "lan/m1" })
		if at < 0 {
			t.Fatal(w.Body.String())
		}
		return list.Data[at]
	}

	// Only the chat, at first.
	if got := shown(); string(got["lan"]) != "[]" || string(got["or"]) != "[]" {
		t.Fatalf("%s", got)
	}
	if m := listed(); m.Provider != "lan" || m.Level != "chosen" || m.Kinds == nil || len(*m.Kinds) != 0 {
		t.Fatalf("%+v", m)
	}

	// The owner ticks kinds: known ones, each once, in their order.
	put := func(id string, data any, who string) *httptest.ResponseRecorder {
		return r.do("PUT", "/api/v1/outside/services/"+id+"/data", map[string]any{"data": data}, opts{cookie: who})
	}
	want(t, put("lan", []string{"memories"}, viewer), 403)
	want(t, put("lan", []string{"memories", "passwords"}, owner), 400)
	want(t, put("lan", "everything", owner), 400)
	want(t, r.do("PUT", "/api/v1/outside/services/lan/data", map[string]any{}, opts{cookie: owner}), 400)
	want(t, put("nope", []string{}, owner), 400)
	want(t, put("lan", []string{"computer", "memories", "memories"}, owner), 200)
	if m := listed(); m.Level != "chosen" || !slices.Equal(*m.Kinds, []string{"memories", "computer"}) {
		t.Fatalf("%+v", m)
	}

	// Like this node is for a server the owner runs, said by them: never a cloud's.
	if w := put("or", "node", owner); w.Code != 400 || !strings.Contains(w.Body.String(), "not-yours") {
		t.Fatal(w.Body.String())
	}
	want(t, put("lan", "node", owner), 200)
	if got := shown(); string(got["lan"]) != `"node"` {
		t.Fatalf("%s", got)
	}
	if m := listed(); m.Level != "node" || m.Kinds != nil {
		t.Fatalf("%+v", m)
	}

	// A new key keeps it; the same name at another address is another server, and starts at only the chat.
	want(t, r.do("PUT", "/api/v1/outside/services/lan", map[string]string{"kind": "custom", "title": "LAN", "base": svc.URL + "/v1", "key": "sk-new"}, opts{cookie: owner}), 200)
	if got := shown(); string(got["lan"]) != `"node"` {
		t.Fatalf("%s", got)
	}
	want(t, r.do("PUT", "/api/v1/outside/services/lan", map[string]string{"kind": "custom", "title": "LAN", "base": "http://10.0.0.9/v1"}, opts{cookie: owner}), 200)
	if got := shown(); string(got["lan"]) != "[]" {
		t.Fatalf("%s", got)
	}
}

// A custom service is https, or http at an address on the unit's network or the owner's tailnet.
func TestACustomServiceIsHTTPOnlyNearby(t *testing.T) {
	for base, ok := range map[string]bool{
		"https://models.example.com/v1":    true,
		"http://192.168.1.20:8080/v1":      true,
		"http://127.0.0.1:8080/v1":         true,
		"http://100.101.102.103:8080/v1":   true,
		"http://[fd7a:115c:a1e0::1]/v1":    true,
		"http://100.128.0.1/v1":            false,
		"http://93.184.215.14/v1":          false,
		"http://gpu-box.local:8080/v1":     false,
		"http://user@192.168.1.20:8080/v1": false,
	} {
		if err := checkBase(base); (err == nil) != ok {
			t.Errorf("%s: %v", base, err)
		}
	}
}
