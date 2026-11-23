package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"patai/console/internal/store"
	"patai/console/internal/workloads"
)

// platform sets the rig up with an app of three APIs, each served by a stand-in at its own address, and what
// patai-workload would show with these APIs on.
func (r *rig) platform(t *testing.T, chat, embed, images http.Handler, on ...string) {
	r.platformWith(t, chat, embed, images, nil, on...)
}

// platformWith adds the agentic API, behind its own stand-in.
func (r *rig) platformWith(t *testing.T, chat, embed, images, agent http.Handler, on ...string) {
	t.Helper()
	apps := filepath.Join(r.dir, "apps")
	os.MkdirAll(filepath.Join(apps, "ai-api"), 0o755)
	os.WriteFile(filepath.Join(apps, "ai-api", "manifest.json"), []byte(`{"name": "ai-api", "title": "AI API",
		"about": "x", "apis": [
		{"name": "chat", "title": "Chat", "service": "llm", "always": true, "routes": ["/v1/chat/completions", "/v1/completions", "/v1/responses"]},
		{"name": "embeddings", "title": "Embeddings", "service": "embed", "default": true, "routes": ["/v1/embeddings"]},
		{"name": "images", "title": "Images", "service": "sd", "routes": ["/v1/images/generations"]},
		{"name": "agentic", "title": "Agentic API", "service": "agent", "routes": ["/v1/responses", "/v1/responses/", "/v1/messages", "/v1/files", "/v1/files/", "/v1/containers", "/v1/containers/"]}]}`), 0o644)
	r.s.AppsPath = apps
	for name, handler := range map[string]http.Handler{"llm": chat, "embed": embed, "sd": images, "agent": agent} {
		if handler == nil {
			handler = http.NotFoundHandler()
		}
		backend := httptest.NewServer(handler)
		t.Cleanup(backend.Close)
		folder := filepath.Join(r.s.RecipesPath, name)
		os.MkdirAll(folder, 0o755)
		os.WriteFile(filepath.Join(folder, "compose.yaml"), []byte("services:\n  server:\n    image: x\n"), 0o644)
		os.WriteFile(filepath.Join(folder, "manifest.json"), []byte(`{"name": "`+name+`", "ready": "`+backend.URL+`/health"}`), 0o644)
	}
	shown, _ := json.Marshal(map[string]any{"chosen": []string{"ai-api"}, "apis": map[string][]string{"ai-api": on},
		"models": map[string]any{"llm": map[string]any{"have": []string{"a.gguf", "b.gguf"}, "use": "b.gguf"},
			"embed": map[string]any{"have": []string{"e.gguf"}, "use": "e.gguf"},
			"sd":    map[string]any{"have": []string{"klein.gguf", "vae.safetensors", "llm.gguf"}, "use": ""}},
		"settings": map[string]any{"sd": map[string]any{"values": map[string]any{"size": 768, "steps": 6, "guidance": 1.5,
			"sampler": "euler_a", "waiting": 2}}}})
	r.rootd.outputs = map[string]string{"apps-shown": string(shown)}
	r.s.views = views{}
}

func (r *rig) key(cookie, name string, limits store.Limits) (string, int64) {
	r.t.Helper()
	w := r.do("POST", "/api/v1/keys", map[string]any{"name": name, "limits": limits, "current_password": password}, opts{cookie: cookie})
	want(r.t, w, 201)
	var out struct {
		Secret string
		Key    store.Key
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.Secret, out.Key.ID
}

func (r *rig) logged() []store.APIRequest {
	r.t.Helper()
	r.s.book.flush(r.s.Store)
	list, err := r.s.Store.APIRequests(100, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return list
}

func chatAnswer(seen **http.Request) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		*seen = req
		body, _ := io.ReadAll(req.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"model\":\"b.gguf\"}\n\n")
			w.(http.Flusher).Flush()
			io.WriteString(w, "data: {\"choices\":[],\"model\":\"b.gguf\",\"timings\":{\"cache_n\":4,\"prompt_n\":3,\"predicted_n\":3}}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// llama.cpp repeats the request's model, which can be anything.
		io.WriteString(w, `{"choices":[],"model":"`+strings.Repeat("m", 200000)+`","usage":{"prompt_tokens":12,"completion_tokens":30,"total_tokens":42}}`)
	})
}

func TestGatewayWhoGetsIn(t *testing.T) {
	var seen *http.Request
	r := newRig(t)
	r.platform(t, chatAnswer(&seen), nil, nil, "chat")
	owner, _ := r.claim()
	load, loadID := r.key(owner, "app", store.Limits{})
	viewerToken := r.token(owner, "watch", "viewer")
	viewer := r.user(owner, "vi", "viewer")

	want(t, r.do("POST", "/v1/chat/completions", nil, opts{noOrigin: true}), 401)
	want(t, r.do("POST", "/v1/chat/completions", nil, opts{bearer: viewerToken, noOrigin: true}), 403)
	want(t, r.do("POST", "/v1/chat/completions", nil, opts{cookie: viewer}), 403)
	// The service is told who it's from, whatever the client says.
	forged := map[string]string{callerHeader: "app:chat"}
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: load, noOrigin: true, headers: forged}), 200)
	if seen.Header.Get("Authorization") != "" {
		t.Fatal("the key reached the service")
	}
	if got := seen.Header.Values(callerHeader); len(got) != 1 || got[0] != fmt.Sprintf("key:%d", loadID) {
		t.Fatalf("caller %q", got)
	}
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{cookie: owner, headers: forged}), 200)
	if strings.Contains(seen.Header.Get("Cookie"), sessionCookie) {
		t.Fatal("the session reached the service")
	}
	if got := seen.Header.Get(callerHeader); !strings.HasPrefix(got, "person:") {
		t.Fatalf("caller %q", got)
	}
	want(t, r.do("GET", "/v1/models", nil, opts{cookie: owner, origin: "https://evil.example"}), 403)
	w := r.do("POST", "/v1/chat/completions", nil, opts{noOrigin: true})
	if !strings.Contains(w.Body.String(), `"invalid_api_key"`) {
		t.Fatalf("not in OpenAI's shape: %s", w.Body.String())
	}
}

func TestGatewayRoutesOnlyWhatsOn(t *testing.T) {
	var seen *http.Request
	embedded := false
	r := newRig(t)
	r.platform(t, chatAnswer(&seen), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		embedded = true
		io.WriteString(w, `{"model":"e.gguf","data":[],"usage":{"prompt_tokens":5,"total_tokens":5}}`)
	}), nil, "chat")
	owner, _ := r.claim()
	load, _ := r.key(owner, "app", store.Limits{})
	w := r.do("POST", "/v1/embeddings", map[string]any{}, opts{bearer: load, noOrigin: true})
	want(t, w, 404)
	if !strings.Contains(w.Body.String(), "Embeddings is off") {
		t.Fatal(w.Body.String())
	}
	want(t, r.do("POST", "/v1/rerank", map[string]any{}, opts{bearer: load, noOrigin: true}), 404)
	want(t, r.do("POST", "/v1/completions", map[string]any{}, opts{bearer: load, noOrigin: true}), 200)

	r.platform(t, chatAnswer(&seen), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		embedded = true
		io.WriteString(w, `{"model":"e.gguf","data":[],"usage":{"prompt_tokens":5,"total_tokens":5}}`)
	}), nil, "embeddings")
	want(t, r.do("POST", "/v1/embeddings", map[string]any{}, opts{bearer: load, noOrigin: true}), 200)
	if !embedded {
		t.Fatal("embeddings weren't asked")
	}
	w = r.do("GET", "/v1/models", nil, opts{bearer: load, noOrigin: true})
	want(t, w, 200)
	var models struct {
		Data []struct{ ID string }
	}
	json.Unmarshal(w.Body.Bytes(), &models)
	if len(models.Data) != 2 || models.Data[0].ID != "b.gguf" || models.Data[1].ID != "e.gguf" {
		t.Fatalf("%+v", models)
	}
	want(t, r.do("GET", "/v1/models/e.gguf", nil, opts{bearer: load, noOrigin: true}), 200)
	want(t, r.do("GET", "/v1/models/a.gguf", nil, opts{bearer: load, noOrigin: true}), 404)

	// The app off turns them all off.
	r.rootd.outputs["apps-shown"] = `{"chosen": [], "apis": {"ai-api": ["embeddings"]}}`
	r.s.views = views{}
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: load, noOrigin: true}), 404)
}

func TestGatewayModelsSayWhatTheyTake(t *testing.T) {
	r := newRig(t)
	r.platform(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/models" {
			http.NotFound(w, req)
			return
		}
		io.WriteString(w, `{"data":[{"id":"a.gguf","architecture":{"input_modalities":["text"]}},
			{"id":"b.gguf","architecture":{"input_modalities":["text","image"],"output_modalities":["text"]}}]}`)
	}), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"data":[{"id":"e.gguf","architecture":"not an object"}]}`)
	}), nil, "chat", "embeddings")
	owner, _ := r.claim()
	load, _ := r.key(owner, "app", store.Limits{})
	w := r.do("GET", "/v1/models", nil, opts{bearer: load, noOrigin: true})
	want(t, w, 200)
	var models struct {
		Data []struct {
			ID           string
			Architecture *struct {
				InputModalities []string `json:"input_modalities"`
			}
		}
	}
	json.Unmarshal(w.Body.Bytes(), &models)
	if len(models.Data) != 2 || models.Data[0].ID != "b.gguf" || models.Data[0].Architecture == nil ||
		strings.Join(models.Data[0].Architecture.InputModalities, ",") != "text,image" || models.Data[1].Architecture != nil {
		t.Fatalf("%s", w.Body.String())
	}
}

func TestGatewayMetersAndLogs(t *testing.T) {
	var seen *http.Request
	r := newRig(t)
	r.platform(t, chatAnswer(&seen), nil, nil, "chat")
	owner, _ := r.claim()
	load, id := r.key(owner, "meter", store.Limits{})
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{"messages": []any{}}, opts{bearer: load, noOrigin: true}), 200)
	w := r.do("POST", "/v1/chat/completions", map[string]any{"stream": true}, opts{bearer: load, noOrigin: true})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("the stream didn't come through: %s", w.Body.String())
	}
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{cookie: owner}), 200)
	list := r.logged()
	if len(list) != 3 {
		t.Fatalf("%+v", list)
	}
	person, stream, plain := list[0], list[1], list[2]
	if plain.KeyID != id || plain.Key != "meter" || plain.API != "chat" || plain.Endpoint != "/v1/chat/completions" ||
		plain.Model != "b.gguf" || plain.Status != 200 || plain.PromptTokens != 12 || plain.CompletionTokens != 30 {
		t.Fatalf("%+v", plain)
	}
	if stream.PromptTokens != 7 || stream.CompletionTokens != 3 {
		t.Fatalf("the stream's timings weren't counted: %+v", stream)
	}
	if person.KeyID != 0 || person.Key != "ada" {
		t.Fatalf("%+v", person)
	}
	w = r.do("GET", "/api/v1/api-usage?days=7", nil, opts{cookie: owner})
	want(t, w, 200)
	var usage []store.Usage
	json.Unmarshal(w.Body.Bytes(), &usage)
	if len(usage) != 2 || usage[1].KeyID != id || usage[1].Requests != 2 || usage[1].PromptTokens != 19 {
		t.Fatalf("%+v", usage)
	}
	w = r.do("GET", "/api/v1/keys", nil, opts{cookie: owner})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"completion_tokens":33`) {
		t.Fatalf("today's use isn't on the key: %s", w.Body.String())
	}
}

func TestGatewayHoldsKeysToTheirLimits(t *testing.T) {
	var seen *http.Request
	r := newRig(t)
	r.platform(t, chatAnswer(&seen), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{}`)
	}), nil, "chat", "embeddings")
	owner, _ := r.claim()

	only, _ := r.key(owner, "only-embed", store.Limits{APIs: []string{"embeddings"}})
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: only, noOrigin: true}), 403)
	want(t, r.do("POST", "/v1/embeddings", map[string]any{}, opts{bearer: only, noOrigin: true}), 200)

	small, _ := r.key(owner, "small", store.Limits{MaxBody: 64})
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{"text": strings.Repeat("x", 100)}, opts{bearer: small, noOrigin: true}), 413)
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: small, noOrigin: true}), 200)

	daily, id := r.key(owner, "daily", store.Limits{TokensPerDay: 50})
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: daily, noOrigin: true}), 200)
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: daily, noOrigin: true}), 200)
	w := r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: daily, noOrigin: true})
	want(t, w, 429)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// Counted from the store after a restart, as well as here.
	r.logged()
	r.s.book = book{}
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: daily, noOrigin: true}), 429)
	want(t, r.do("PUT", "/api/v1/keys/"+itoa(id)+"/limits", store.Limits{TokensPerDay: 1000}, opts{cookie: owner}), 204)
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: daily, noOrigin: true}), 200)

	want(t, r.do("POST", "/api/v1/keys", map[string]any{"name": "odd", "limits": store.Limits{APIs: []string{"video"}},
		"current_password": password}, opts{cookie: owner}), 400)
	want(t, r.do("POST", "/api/v1/keys", map[string]any{"name": "odd", "limits": store.Limits{MaxConcurrent: -1},
		"current_password": password}, opts{cookie: owner}), 400)
	operator := r.user(owner, "op", "operator")
	want(t, r.do("GET", "/api/v1/keys", nil, opts{cookie: operator}), 403)
	want(t, r.do("GET", "/api/v1/api-requests", nil, opts{cookie: operator}), 200)
}

// slotRig is a gateway in front of a stand-in llama-server that answers once a request without "hold" in it comes in, or
// one with it is let go.
type slotRig struct {
	*rig
	key     string
	bodies  chan map[string]any
	headers chan http.Header
	hold    chan struct{}
}

func newSlotRig(t *testing.T) *slotRig {
	sr := &slotRig{bodies: make(chan map[string]any, 8), headers: make(chan http.Header, 8), hold: make(chan struct{})}
	chat := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		sr.bodies <- body
		sr.headers <- req.Header.Clone()
		if body["hold"] != nil {
			<-sr.hold
		}
		io.WriteString(w, `{"usage": {"prompt_tokens": 7, "completion_tokens": 3}}`)
	})
	sr.rig = newRig(t)
	sr.platform(t, chat, nil, nil, "chat")
	owner, _ := sr.claim()
	sr.key, _ = sr.rig.key(owner, "slots", store.Limits{})
	return sr
}

// parallel sets the model server's requests at once, or with nil takes the setting away.
func (sr *slotRig) parallel(n any) {
	var shown map[string]any
	json.Unmarshal([]byte(sr.rootd.outputs["apps-shown"]), &shown)
	settings := shown["settings"].(map[string]any)
	if n == nil {
		delete(settings, "llm")
	} else {
		settings["llm"] = map[string]any{"values": map[string]any{"parallel": n, "context": 65536}}
	}
	out, _ := json.Marshal(shown)
	sr.rootd.outputs["apps-shown"] = string(out)
	sr.s.views = views{}
}

func (sr *slotRig) send(path string, body map[string]any, headers map[string]string) {
	sr.t.Helper()
	want(sr.t, sr.do("POST", path, body, opts{bearer: sr.key, noOrigin: true, headers: headers}), 200)
}

// slotOf is the slot the next request the model server had named, if any.
func (sr *slotRig) slotOf() any {
	sr.t.Helper()
	body := <-sr.bodies
	if h := <-sr.headers; h.Get("X-Patai-Priority") != "" {
		sr.t.Fatal("the mark reached the service")
	}
	if body["messages"] == nil && body["input"] == nil && body["prompt"] == nil {
		sr.t.Fatalf("the body changed: %v", body)
	}
	return body["id_slot"]
}

var background = map[string]string{"X-Patai-Priority": "background"}

// What nobody is waiting on goes to llama-server's last slot while it answers two or more at once, so people's
// conversations keep the others; the mark itself never reaches it.
func TestGatewayPinsBackgroundRequests(t *testing.T) {
	r := newSlotRig(t)
	r.parallel(3)
	r.send("/v1/chat/completions", map[string]any{"messages": []any{}, "stream": true}, background)
	if got := r.slotOf(); got != float64(2) {
		t.Fatalf("a background request went to slot %v, not the last", got)
	}
	r.send("/v1/responses", map[string]any{"input": "hello"}, background)
	if got := r.slotOf(); got != float64(2) {
		t.Fatalf("a background response went to slot %v, not the last", got)
	}
	// A slot it names is its own.
	r.send("/v1/chat/completions", map[string]any{"messages": []any{}, "id_slot": 0}, background)
	if got := r.slotOf(); got != float64(0) {
		t.Fatalf("slot %v", got)
	}
	// One request at once, or a service with no such setting, has no slot to spare.
	r.parallel(1)
	r.send("/v1/chat/completions", map[string]any{"messages": []any{}}, background)
	if got := r.slotOf(); got != nil {
		t.Fatalf("slot %v with one request at once", got)
	}
	r.parallel(nil)
	r.send("/v1/chat/completions", map[string]any{"messages": []any{}}, background)
	if got := r.slotOf(); got != nil {
		t.Fatalf("slot %v with no setting", got)
	}
}

// A person's request never goes to the background slot, where the next background call would take its conversation's
// cache, and goes back to the slot that holds its conversation (patai_chat docs/prompt-cache.md).
func TestGatewayKeepsConversationsOffTheBackgroundSlot(t *testing.T) {
	r := newSlotRig(t)
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "search_web"}}}
	system := map[string]any{"role": "system", "content": "You are patai."}
	say := func(role, content string) map[string]any { return map[string]any{"role": role, "content": content} }
	chat := func(messages ...map[string]any) map[string]any {
		return map[string]any{"tools": tools, "messages": append([]map[string]any{system}, messages...)}
	}
	slot := func(path string, body map[string]any, wanted float64, what string) {
		t.Helper()
		r.send(path, body, nil)
		if got := r.slotOf(); got != wanted {
			t.Fatalf("%s went to slot %v, not %v", what, got, wanted)
		}
	}

	// With two at once, people have slot 0, whatever the path, and what nobody is waiting on slot 1.
	r.parallel(2)
	slot("/v1/chat/completions", chat(say("user", "Hi.")), 0, "a new chat")
	slot("/v1/chat/completions", chat(say("user", "Something else entirely.")), 0, "another chat")
	slot("/v1/completions", map[string]any{"prompt": "Once upon a time"}, 0, "a completion")
	slot("/v1/responses", map[string]any{"input": "Hello."}, 0, "a response")
	r.send("/v1/chat/completions", chat(say("user", "Hi.")), background)
	if got := r.slotOf(); got != float64(1) {
		t.Fatalf("a background request went to slot %v", got)
	}
	// A slot a request names is its own.
	r.send("/v1/chat/completions", map[string]any{"messages": []any{}, "id_slot": 1}, nil)
	if got := r.slotOf(); got != float64(1) {
		t.Fatalf("a named slot became %v", got)
	}
	// A body that isn't a JSON object goes as it is.
	want(t, r.do("POST", "/v1/chat/completions", "not JSON", opts{bearer: r.key, noOrigin: true}), 200)
	<-r.headers
	if body := <-r.bodies; body != nil {
		t.Fatalf("a body that isn't an object became %v", body)
	}

	// With three, each conversation goes back to its own of slots 0 and 1, and a new one to the one used least lately.
	r.parallel(3)
	a := []map[string]any{say("user", "What's the weather in Toronto?")}
	b := []map[string]any{say("user", "Write me a poem about the sea, please.")}
	slot("/v1/chat/completions", chat(a...), 0, "a new chat")
	slot("/v1/chat/completions", chat(b...), 1, "a second chat, which shares only the start")
	a = append(a, say("assistant", "Sunny, 21 °C."), say("user", "And tomorrow?"))
	slot("/v1/chat/completions", chat(a...), 0, "the first chat's next message")
	b = append(b, say("assistant", "The sea is wide."), say("user", "Longer."))
	slot("/v1/chat/completions", chat(b...), 1, "the second chat's next message")
	r.send("/v1/chat/completions", chat(a...), background)
	if got := r.slotOf(); got != float64(2) {
		t.Fatalf("a background request went to slot %v", got)
	}
	// The same message again, as for another version of its answer, or an earlier one, as for a branch.
	slot("/v1/chat/completions", chat(a...), 0, "another version")
	slot("/v1/chat/completions", chat(a[:1]...), 0, "a branch")
	// The second chat's slot was used least lately.
	slot("/v1/chat/completions", chat(say("user", "A third chat.")), 1, "a third chat")
	// A long conversation whose last message is changed holds nearly all of it.
	long := []map[string]any{say("user", strings.Repeat("Tell me about the sea. ", 200)),
		say("assistant", strings.Repeat("The sea is wide and deep. ", 200)), say("user", "Thanks.")}
	slot("/v1/chat/completions", chat(long...), 0, "a long chat")
	long[2] = say("user", "Thank you.")
	slot("/v1/chat/completions", chat(long...), 0, "the long chat with its last message changed")
	// Without its tools it shares too little.
	slot("/v1/chat/completions", map[string]any{"messages": append([]map[string]any{system}, long...)}, 1,
		"the long chat without its tools")
	// A text completion's prompt that goes on from one holds it all.
	slot("/v1/completions", map[string]any{"prompt": "Once upon a time"}, 0, "a story")
	slot("/v1/completions", map[string]any{"prompt": "Once upon a time there was a node."}, 0, "the story's next part")

	// A new conversation takes a slot with no request now over one used less lately.
	var let sync.Once
	release := func() { let.Do(func() { close(r.hold) }) }
	t.Cleanup(release) // before the stand-in closes, which waits for it
	done := make(chan int)
	go func() {
		done <- r.do("POST", "/v1/chat/completions", map[string]any{"messages": []any{say("user", "Wait.")}, "hold": true},
			opts{bearer: r.key, noOrigin: true}).Code
	}()
	if got := r.slotOf(); got != float64(1) {
		t.Fatalf("a new chat went to slot %v", got)
	}
	slot("/v1/completions", map[string]any{"prompt": "Once upon a time there was a node. It answered."}, 0,
		"the story's next part")
	slot("/v1/chat/completions", chat(say("user", "Meanwhile.")), 0, "a new chat while slot 1, used less lately, answers")
	release()
	if code := <-done; code != 200 {
		t.Fatalf("the held request: %d", code)
	}
}

func TestGatewayHoldsRequestsAtOnce(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	r := newRig(t)
	r.platform(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started <- struct{}{}
		<-release
		io.WriteString(w, `{}`)
	}), nil, nil, "chat")
	owner, _ := r.claim()
	one, _ := r.key(owner, "one", store.Limits{MaxConcurrent: 1})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: one, noOrigin: true})
	}()
	<-started
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: one, noOrigin: true}), 429)
	close(release)
	wg.Wait()
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: one, noOrigin: true}), 200)
}

// sdServer stands in for stable-diffusion.cpp's sd-server: each image is a job, done on its next look unless it's
// waiting, and a PNG.
type sdServer struct {
	mu        sync.Mutex
	jobs      []map[string]any
	waiting   chan struct{} // if set, a job isn't done until it's closed
	cancelled []string
}

func (f *sdServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case req.URL.Path == "/sdcpp/v1/img_gen" && req.Method == http.MethodPost:
		var params map[string]any
		json.NewDecoder(req.Body).Decode(&params)
		f.jobs = append(f.jobs, params)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"id": "job`+itoa(int64(len(f.jobs)))+`", "status": "queued"}`)
	case strings.HasSuffix(req.URL.Path, "/cancel"):
		f.cancelled = append(f.cancelled, strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/sdcpp/v1/jobs/"), "/cancel"))
		io.WriteString(w, `{"status": "generating", "cancel_requested": true}`)
	case strings.HasPrefix(req.URL.Path, "/sdcpp/v1/jobs/"):
		id := strings.TrimPrefix(req.URL.Path, "/sdcpp/v1/jobs/")
		if f.waiting != nil {
			select {
			case <-f.waiting:
			default:
				io.WriteString(w, `{"id": "`+id+`", "status": "generating"}`)
				return
			}
		}
		png := base64.StdEncoding.EncodeToString([]byte("PNG:" + id))
		io.WriteString(w, `{"id": "`+id+`", "status": "completed", "result": {"output_format": "png", "images": [{"index": 0, "b64_json": "`+png+`"}]}}`)
	default:
		http.NotFound(w, req)
	}
}

func TestImages(t *testing.T) {
	fake := &sdServer{}
	r := newRig(t)
	r.platform(t, nil, nil, fake, "images")
	owner, _ := r.claim()
	load, _ := r.key(owner, "pics", store.Limits{ImagesPerDay: 3})
	w := r.do("POST", "/v1/images/generations", map[string]any{"prompt": "a red fox", "n": 2, "size": "768x512", "seed": 7,
		"model": "dall-e-2"}, opts{bearer: load, noOrigin: true})
	want(t, w, 200)
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		}
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Data) != 2 {
		t.Fatal(w.Body.String())
	}
	if png, _ := base64.StdEncoding.DecodeString(out.Data[1].B64); string(png) != "PNG:job2" {
		t.Fatalf("%q", png)
	}
	if job := fake.jobs[1]; job["width"] != 768.0 || job["height"] != 512.0 || job["seed"] != 8.0 || job["prompt"] != "a red fox" {
		t.Fatalf("%v", job)
	}
	// The engine's settings go with each image.
	params, _ := json.Marshal(fake.jobs[0]["sample_params"])
	if string(params) != `{"guidance":{"txt_cfg":1.5},"sample_method":"euler_a","sample_steps":6}` {
		t.Fatalf("%s", params)
	}
	want(t, r.do("POST", "/v1/images/generations", map[string]any{"prompt": "x"}, opts{bearer: load, noOrigin: true}), 200)
	// Three made, which is the day's.
	want(t, r.do("POST", "/v1/images/generations", map[string]any{"prompt": "x"}, opts{bearer: load, noOrigin: true}), 429)
	list := r.logged()
	// The service's model is what's logged, whatever the request named.
	if list[1].Images != 1 || list[2].Images != 2 || list[2].Model != "klein.gguf" || list[0].Status != 429 {
		t.Fatalf("%+v", list)
	}
	for _, bad := range []map[string]any{{"prompt": ""}, {"prompt": "x", "n": 5}, {"prompt": "x", "size": "big"},
		{"prompt": "x", "size": "8x8"}, {"prompt": "x", "response_format": "url"}} {
		want(t, r.do("POST", "/v1/images/generations", bad, opts{cookie: owner}), 400)
	}
	// A size it can't make is made as near as it can: its shape, then its size, up to the largest the settings allow,
	// which has a portrait and a landscape of as many pixels.
	for asked, made := range map[string]string{"1024x1024": "768x768", "800x600": "864x672", "400x400": "512x512",
		"600x800": "672x864", "auto": "512x512", "1024x768": "864x672", "300x400": "448x576", "1200x800": "768x512"} {
		w := r.do("POST", "/v1/images/generations", map[string]any{"prompt": "x", "size": asked}, opts{cookie: owner})
		want(t, w, 200)
		if job := fake.jobs[len(fake.jobs)-1]; !strings.Contains(w.Body.String(), `"size":"`+made+`"`) ||
			itoa(int64(job["width"].(float64)))+"x"+itoa(int64(job["height"].(float64))) != made {
			t.Fatalf("%s: %s %v", asked, w.Body.String(), job)
		}
	}
	// /v1/models lists the image model, not its parts.
	w = r.do("GET", "/v1/models", nil, opts{bearer: load, noOrigin: true})
	if !strings.Contains(w.Body.String(), "klein.gguf") || strings.Contains(w.Body.String(), "vae.safetensors") {
		t.Fatal(w.Body.String())
	}
}

func TestImagesQueueAndCancel(t *testing.T) {
	fake := &sdServer{waiting: make(chan struct{})}
	r := newRig(t)
	r.platform(t, nil, nil, fake, "images")
	owner, _ := r.claim()
	srv := httptest.NewServer(r.s.Handler())
	defer srv.Close()
	ask := func(cancel <-chan struct{}) (*http.Response, error) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/images/generations", strings.NewReader(`{"prompt": "x"}`))
		req.Host = "10.0.0.5"
		req.Header.Set("Cookie", sessionCookie+"="+owner)
		if cancel != nil {
			req.Cancel = cancel
		}
		return http.DefaultClient.Do(req)
	}
	gone := make(chan struct{})
	first := make(chan error, 1)
	go func() { _, err := ask(gone); first <- err }()
	for range 50 {
		fake.mu.Lock()
		n := len(fake.jobs)
		fake.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// One makes and two wait: a fourth is turned away.
	results := make(chan int, 2)
	for range 2 {
		go func() {
			resp, err := ask(nil)
			if err == nil {
				results <- resp.StatusCode
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	resp, err := ask(nil)
	if err != nil || resp.StatusCode != 429 {
		t.Fatalf("%v %v", resp, err)
	}
	// The first one's client goes, and its job is cancelled.
	close(gone)
	<-first
	for range 50 {
		fake.mu.Lock()
		cancelled := len(fake.cancelled)
		fake.mu.Unlock()
		if cancelled > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fake.mu.Lock()
	if len(fake.cancelled) == 0 || fake.cancelled[0] != "job1" {
		t.Fatalf("%v", fake.cancelled)
	}
	close(fake.waiting)
	fake.mu.Unlock()
	if a, b := <-results, <-results; a != 200 || b != 200 {
		t.Fatalf("%d %d", a, b)
	}
}

// chatApp sets the rig up with an app of its own key, as Chat is: chat and search, each served by a stand-in, and its
// pages by another. With the AI API too, if ai is true. It returns the app's key.
func (r *rig) chatApp(t *testing.T, chat, search, pages http.Handler, ai bool, on ...string) string {
	t.Helper()
	r.platform(t, chat, nil, nil, "chat")
	apps := filepath.Join(r.dir, "apps")
	os.MkdirAll(filepath.Join(apps, "chat"), 0o755)
	os.WriteFile(filepath.Join(apps, "chat", "manifest.json"), []byte(`{"name": "chat", "title": "Chat", "about": "x",
		"apis": [
		{"name": "chat", "title": "Chat", "service": "llm", "always": true, "routes": ["/v1/chat/completions"]},
		{"name": "embeddings", "title": "Documents", "service": "embed", "always": true, "routes": ["/v1/embeddings"]},
		{"name": "search", "title": "Web search", "service": "search", "routes": ["/search"]}],
		"services": ["pages"], "key": "PATAI_GATEWAY_KEY", "node": true, "ui": {"port": 8443, "service": "pages"}}`), 0o644)
	for name, handler := range map[string]http.Handler{"search": search, "pages": pages} {
		backend := httptest.NewServer(handler)
		t.Cleanup(backend.Close)
		folder := filepath.Join(r.s.RecipesPath, name)
		os.MkdirAll(folder, 0o755)
		os.WriteFile(filepath.Join(folder, "compose.yaml"), []byte("services:\n  server:\n    image: x\n"), 0o644)
		os.WriteFile(filepath.Join(folder, "manifest.json"), []byte(`{"name": "`+name+`", "ready": "`+backend.URL+`/health"}`), 0o644)
	}
	key := "patai_app_key"
	sum := sha256.Sum256([]byte(key))
	chosen := []string{"chat"}
	if ai {
		chosen = append(chosen, "ai-api")
	}
	shown, _ := json.Marshal(map[string]any{"chosen": chosen, "apis": map[string][]string{"chat": on, "ai-api": {"chat"}},
		"models": map[string]any{"llm": map[string]any{"have": []string{"a.gguf"}, "use": "a.gguf"},
			"embed": map[string]any{"have": []string{"e.gguf"}, "use": "e.gguf"}},
		"keys": map[string]string{"chat": hex.EncodeToString(sum[:])}})
	r.rootd.outputs = map[string]string{"apps-shown": string(shown)}
	r.s.views = views{}
	return key
}

func TestAppsGatewayTakesOnlyAnAppsKeyForItsOwnAPIs(t *testing.T) {
	r := newRig(t)
	got := make(chan *http.Request, 4)
	chat := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got <- req
		io.WriteString(w, `{"usage": {"prompt_tokens": 7, "completion_tokens": 3}}`)
	})
	search := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got <- req
		io.WriteString(w, `{"results": [`+strings.Repeat(`{"url": "https://example.com/"},`, 200)+`{}]}`)
	})
	key := r.chatApp(t, chat, search, http.NotFoundHandler(), false, "search")
	owner, _ := r.claim()
	ownersKey, _ := r.key(owner, "mine", store.Limits{})
	do := func(method, path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://172.17.0.1:18000"+path, strings.NewReader(`{"messages": []}`))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set(callerHeader, "key:1")
		w := httptest.NewRecorder()
		r.s.AppsHandler().ServeHTTP(w, req)
		return w
	}
	want(t, do("POST", "/v1/chat/completions", ""), 401)
	want(t, do("POST", "/v1/chat/completions", ownersKey), 401)
	want(t, do("POST", "/v1/chat/completions", key), 200)
	if req := <-got; req.Header.Get("Authorization") != "" || req.Header.Get(callerHeader) != "app:chat" {
		t.Fatalf("the app's key reached the service, or it's not said to be the app's: %v", req.Header)
	}
	// Search answers with no tokens. A key as Basic's password isn't one.
	want(t, do("GET", "/search?q=x&format=json", key), 200)
	if req := <-got; req.URL.Path != "/search" || req.Header.Get("Authorization") != "" {
		t.Fatalf("%s %v", req.URL, req.Header)
	}
	basic := httptest.NewRequest("GET", "http://172.17.0.1:18000/search?q=x&format=json", nil)
	basic.SetBasicAuth("chat", key)
	refused := httptest.NewRecorder()
	r.s.AppsHandler().ServeHTTP(refused, basic)
	want(t, refused, 401)
	// Its embeddings are always on, but a service this rig has no stand-in for doesn't answer; its models are only chat's.
	w := do("GET", "/v1/models", key)
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"a.gguf"`) || strings.Contains(w.Body.String(), "e.gguf") {
		t.Fatal(w.Body.String())
	}
	want(t, do("POST", "/v1/completions", key), 404)
	logged := r.logged()
	if len(logged) != 2 {
		t.Fatalf("%+v", logged)
	}
	for _, rec := range logged {
		if rec.Key != "Chat" || rec.KeyID != 0 {
			t.Fatalf("%+v", rec)
		}
		if rec.API == "chat" && (rec.PromptTokens != 7 || rec.Model != "a.gguf") || rec.API == "search" && (rec.PromptTokens != 0 || rec.CompletionTokens != 0 || rec.Model != "") {
			t.Fatalf("%+v", rec)
		}
	}
	// Its APIs are its own: an owner's key reaches chat only while the AI API is on, and the app's key isn't one on 443.
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: ownersKey}), 404)
	r.chatApp(t, chat, search, http.NotFoundHandler(), true, "search")
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: ownersKey}), 200)
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: key}), 401)
}

// Audio is passed on uncounted, and the voices are listed from the recipe, on 443 and to an app, each with its key.
func TestVoice(t *testing.T) {
	r := newRig(t)
	got := make(chan string, 4)
	audio := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		got <- req.URL.Path + " " + string(body)
		io.WriteString(w, `{"text": "`+strings.Repeat("word ", 400)+`"}`)
	})
	key := r.chatApp(t, nil, http.NotFoundHandler(), http.NotFoundHandler(), true, "speech")
	apps := filepath.Join(r.dir, "apps")
	for _, app := range []string{"chat", "ai-api"} {
		manifest, _ := os.ReadFile(filepath.Join(apps, app, "manifest.json"))
		os.WriteFile(filepath.Join(apps, app, "manifest.json"), []byte(strings.Replace(string(manifest), `"apis": [`,
			`"apis": [{"name": "speech", "title": "Text to speech", "service": "text-to-speech", "routes": ["/v1/audio/speech", "/v1/audio/language", "/v1/audio/sano/"]},`, 1)), 0o644)
	}
	backend := httptest.NewServer(audio)
	t.Cleanup(backend.Close)
	folder := filepath.Join(r.s.RecipesPath, "text-to-speech")
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, "compose.yaml"), []byte("services:\n  server:\n    image: x\n"), 0o644)
	// Voices whose model isn't kept aren't listed.
	os.WriteFile(filepath.Join(folder, "manifest.json"), []byte(`{"name": "text-to-speech", "ready": "`+backend.URL+`/health",
		"models": [{"name": "a.onnx", "title": "A"}, {"name": "b.onnx", "title": "B"}, {"name": "c.onnx", "title": "C"}],
		"voices": [{"id": "piper-en", "title": "Ann", "locale": "en-US", "engine": "piper", "model": "a.onnx"},
			{"id": "piper-fr", "title": "Cy", "locale": "fr-FR", "engine": "piper", "model": "c.onnx"},
			{"id": "sano-en", "title": "Bo", "locale": "en-US", "engine": "sano", "model": "a.onnx"},
			{"id": "piper-de", "title": "Di", "locale": "de-DE", "engine": "piper", "model": "b.onnx"}]}`), 0o644)
	sum := sha256.Sum256([]byte(key))
	shown, _ := json.Marshal(map[string]any{"chosen": []string{"chat", "ai-api"},
		"apis":   map[string][]string{"chat": {"speech"}, "ai-api": {"chat"}},
		"models": map[string]any{"text-to-speech": map[string]any{"have": []string{"a.onnx", "b.onnx"}}},
		"keys":   map[string]string{"chat": hex.EncodeToString(sum[:])}})
	r.rootd.outputs["apps-shown"] = string(shown)
	r.s.views = views{}
	fromApp := func(method, path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://172.17.0.1:18000"+path, strings.NewReader(`{"input": "hello"}`))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		r.s.AppsHandler().ServeHTTP(w, req)
		return w
	}
	w := fromApp("GET", "/v1/audio/voices", key)
	want(t, w, 200)
	if w.Body.String() != `{"voices":[{"id":"piper-en","name":"Ann","locale":"en-US","engine":"piper"},`+
		`{"id":"sano-en","name":"Bo","locale":"en-US","engine":"sano"},{"id":"piper-de","name":"Di","locale":"de-DE","engine":"piper"}]}`+"\n" {
		t.Fatal(w.Body.String())
	}
	// Nothing is answered without the key.
	want(t, fromApp("GET", "/v1/audio/voices", ""), 401)
	want(t, fromApp("POST", "/v1/audio/speech", ""), 401)
	want(t, fromApp("POST", "/v1/audio/speech", key), 200)
	if seen := <-got; seen != `/v1/audio/speech {"input": "hello"}` {
		t.Fatal(seen)
	}
	// A text's language, and a sano voice's files for a browser, go to the same service.
	want(t, fromApp("POST", "/v1/audio/language", key), 200)
	if seen := <-got; seen != `/v1/audio/language {"input": "hello"}` {
		t.Fatal(seen)
	}
	want(t, fromApp("GET", "/v1/audio/sano/voices/sano-en/meta.json", key), 200)
	if seen := <-got; !strings.HasPrefix(seen, "/v1/audio/sano/voices/sano-en/meta.json") {
		t.Fatal(seen)
	}
	logged := r.logged()
	if len(logged) != 3 || logged[0].API != "speech" || logged[0].PromptTokens != 0 || logged[0].CompletionTokens != 0 ||
		logged[0].Model != "a.onnx" {
		t.Fatalf("%+v", logged)
	}
	// The AI API has no speech on: an owner's key finds it off.
	owner, _ := r.claim()
	ownersKey, _ := r.key(owner, "mine", store.Limits{})
	want(t, r.do("GET", "/v1/audio/voices", nil, opts{bearer: ownersKey}), 404)
}

func TestAppUIIsThereWhileItsAppIsOn(t *testing.T) {
	r := newRig(t)
	seen := make(chan *http.Request, 1)
	pages := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen <- req
		io.WriteString(w, "chat")
	})
	r.chatApp(t, nil, nil, pages, false)
	owner, _ := r.claim()
	app, _ := workloads.ReadApp(r.s.AppsPath, "chat")
	do := func(cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "https://10.0.0.5:8443/api/chats", nil)
		req.Host = "10.0.0.5:8443"
		req.Header.Set("Cookie", sessionCookie+"="+cookie+"; token=its-own")
		req.Header.Set("Authorization", "Bearer its-own")
		w := httptest.NewRecorder()
		r.s.AppUI(*app).ServeHTTP(w, req)
		return w
	}
	// No console login needed, and none passed on: its own sign-in is what it has.
	w := do(owner)
	want(t, w, 200)
	req := <-seen
	if cookie, err := req.Cookie(sessionCookie); err == nil || req.Header.Get("Authorization") != "Bearer its-own" {
		t.Fatalf("%v %v", cookie, req.Header)
	}
	if token, err := req.Cookie("token"); err != nil || token.Value != "its-own" || req.Host != "10.0.0.5:8443" ||
		req.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("%s %v", req.Host, req.Header)
	}
	if w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatal(w.Header())
	}
	r.rootd.outputs["apps-shown"] = `{"chosen": []}`
	r.s.views = views{}
	want(t, do(""), 404)
}

func TestAppUISendsARoutedPathToItsService(t *testing.T) {
	r := newRig(t)
	pages := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, "pages "+req.URL.Path) })
	r.chatApp(t, nil, nil, pages, false)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "api "+req.URL.Path+" "+req.Host)
	}))
	t.Cleanup(backend.Close)
	folder := filepath.Join(r.s.RecipesPath, "api")
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, "compose.yaml"), []byte("services:\n  server:\n    image: x\n"), 0o644)
	os.WriteFile(filepath.Join(folder, "manifest.json"), []byte(`{"name": "api", "ready": "`+backend.URL+`/health"}`), 0o644)
	app, _ := workloads.ReadApp(r.s.AppsPath, "chat")
	app.Services = append(app.Services, "api")
	app.UI.Routes = map[string]string{"/api/": "api"}
	handler := r.s.AppUI(*app)
	for path, answer := range map[string]string{
		"/api/v1/chats": "api /api/v1/chats 10.0.0.5:8443",
		"/api":          "pages /api",
		"/c/1":          "pages /c/1",
	} {
		req := httptest.NewRequest("GET", "https://10.0.0.5:8443"+path, nil)
		req.Host = "10.0.0.5:8443"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		want(t, w, 200)
		if w.Body.String() != answer {
			t.Errorf("%s: %q", path, w.Body.String())
		}
	}
}

func TestGatewayMetersAClientThatGoes(t *testing.T) {
	sent := make(chan struct{})
	r := newRig(t)
	r.platform(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 5 {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}],\"n\":"+itoa(int64(i))+"}\n\n")
		}
		w.(http.Flusher).Flush()
		close(sent)
		<-req.Context().Done()
	}), nil, nil, "chat")
	owner, _ := r.claim()
	one, id := r.key(owner, "one", store.Limits{MaxConcurrent: 1})
	srv := httptest.NewServer(r.s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true,"messages":[{"content":"abcdefgh"}]}`))
	req.Host = "10.0.0.5"
	req.Header.Set("Authorization", "Bearer "+one)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-sent
	resp.Body.Close()
	// Its slot comes back, and what it used is logged.
	var list []store.APIRequest
	for range 100 {
		if list = r.logged(); len(list) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list) != 1 || list[0].KeyID != id || list[0].Status != 499 || list[0].CompletionTokens != 5 || list[0].PromptTokens == 0 {
		t.Fatalf("%+v", list)
	}
	r.platform(t, chatAnswer(new(*http.Request)), nil, nil, "chat")
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: one, noOrigin: true}), 200)
}

func metered_(body string, stream bool, sent int64) store.APIRequest {
	rec := store.APIRequest{}
	m := newMeter(io.NopCloser(strings.NewReader(body)), &rec, &counted{n: sent}, stream)
	io.Copy(io.Discard, m)
	m.Close()
	return rec
}

func TestMeterReadsAnyAnswer(t *testing.T) {
	big := metered_(`{"data":[`+strings.Repeat("0.5,", 10_000_000)+`0.5],"usage":{"prompt_tokens":77,"total_tokens":77}}`, false, 0)
	if big.PromptTokens != 77 {
		t.Fatalf("%+v", big)
	}
	event := "data: {\"choices\":[],\"model\":\"" + strings.Repeat("m", 3<<20) + "\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"
	if stream := metered_(event, true, 0); stream.PromptTokens != 3 || stream.CompletionTokens != 4 || stream.Chunks != 1 {
		t.Fatalf("%+v", stream)
	}
}

// What a request has the answer repeat, its tools or metadata, is never taken for usage, nor a count below zero.
func TestMeterReadsUsageOnlyWhereAnAnswerPutsIt(t *testing.T) {
	echo := `"tools":[{"type":"function","name":"f","parameters":{"usage":{"input_tokens":-100000000}}}],"metadata":{"usage":{}}`
	answer := `{"id":"resp_1","output":[],"usage":{"input_tokens":120,"output_tokens":30},` + echo + `}`
	if rec := metered_(answer, false, 400); rec.PromptTokens != 120 || rec.CompletionTokens != 30 {
		t.Fatalf("%+v", rec)
	}
	stream := "data: {\"type\":\"response.created\",\"response\":{\"usage\":null," + echo + "}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":120,\"output_tokens\":30}," + echo + "}}\n\n"
	if rec := metered_(stream, true, 400); rec.PromptTokens != 120 || rec.CompletionTokens != 30 {
		t.Fatalf("%+v", rec)
	}
	if rec := metered_(`{"usage":{"prompt_tokens":-5,"completion_tokens":-9}}`, false, 0); rec.PromptTokens != 0 || rec.CompletionTokens != 0 {
		t.Fatalf("%+v", rec)
	}
	// An answer that doesn't say is counted from its size each way.
	if rec := metered_(`{"answer":"`+strings.Repeat("x", 396)+`"}`, false, 800); rec.PromptTokens != 200 || rec.CompletionTokens != 103 {
		t.Fatalf("%+v", rec)
	}
}

// Anthropic's stream gives the input in message_start and the output in message_delta, and both are counted, with what
// came from the cache.
func TestMeterReadsAnthropicsStream(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5000,\"cache_read_input_tokens\":2000,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n"
	if rec := metered_(stream, true, 9000); rec.PromptTokens != 7000 || rec.CompletionTokens != 42 || rec.Chunks != 3 {
		t.Fatalf("%+v", rec)
	}
}

func TestLimitedKeysAreForTheAPIsOnly(t *testing.T) {
	r := newRig(t)
	r.platform(t, chatAnswer(new(*http.Request)), nil, nil, "chat")
	owner, _ := r.claim()
	held, _ := r.key(owner, "held", store.Limits{TokensPerDay: 1000})
	want(t, r.do("POST", "/v1/chat/completions", map[string]any{}, opts{bearer: held, noOrigin: true}), 200)
	want(t, r.do("GET", "/api/v1/status", nil, opts{bearer: held, noOrigin: true}), 403)
	w := r.do("GET", "/api/v1/keys", nil, opts{cookie: owner})
	if !strings.Contains(w.Body.String(), `"scope":"key"`) {
		t.Fatal(w.Body.String())
	}
}

// The agentic API takes Responses from chat while it's on, with every path under it, and its requests are logged with
// chat's model.
func TestAgenticTakesResponsesWhileItsOn(t *testing.T) {
	where := func(name string, used int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"from": "`+name+` `+req.URL.Path+`", "usage": {"input_tokens": `+itoa(int64(used))+`, "output_tokens": 1}}`)
		})
	}
	r := newRig(t)
	r.platformWith(t, where("chat", 1), nil, nil, where("agent", 9), "agentic")
	owner, _ := r.claim()
	load, _ := r.key(owner, "app", store.Limits{})
	for _, ask := range []struct{ path, from, endpoint string }{
		{"/v1/responses", "agent /v1/responses", "/v1/responses"},
		{"/v1/responses/resp_1", "agent /v1/responses/resp_1", "/v1/responses/"},
		{"/v1/messages", "agent /v1/messages", "/v1/messages"},
		{"/v1/chat/completions", "chat /v1/chat/completions", "/v1/chat/completions"},
	} {
		w := r.do("POST", ask.path, map[string]any{}, opts{bearer: load, noOrigin: true})
		if w.Code != 200 || !strings.Contains(w.Body.String(), ask.from) {
			t.Fatalf("%s: %d %s", ask.path, w.Code, w.Body.String())
		}
		// Logged by its route, not the id under it, with chat's model, and the agentic API's usage.
		got := r.logged()[0]
		used := map[bool]int64{true: 10, false: 2}[strings.HasPrefix(ask.from, "agent")]
		if got.Endpoint != ask.endpoint || got.Model != "b.gguf" || got.PromptTokens+got.CompletionTokens != used {
			t.Fatalf("%s: %+v", ask.path, got)
		}
	}
	// Files and containers are stored, not a model's: an upload counts no tokens, however big.
	for _, path := range []string{"/v1/files", "/v1/containers/cntr_1/files"} {
		w := r.do("POST", path, map[string]any{"data": strings.Repeat("x", 40000)}, opts{bearer: load, noOrigin: true})
		if w.Code != 200 || !strings.Contains(w.Body.String(), "agent "+path) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if got := r.logged()[0]; got.PromptTokens+got.CompletionTokens != 0 {
			t.Fatalf("%s counted tokens: %+v", path, got)
		}
	}
	// Off, Responses is chat's again, and what's only the agentic API's is off.
	r.platformWith(t, where("chat", 1), nil, nil, where("agent", 9))
	w := r.do("POST", "/v1/responses", map[string]any{}, opts{bearer: load, noOrigin: true})
	if !strings.Contains(w.Body.String(), "chat /v1/responses") {
		t.Fatal(w.Body.String())
	}
	want(t, r.do("GET", "/v1/responses/resp_1", nil, opts{bearer: load, noOrigin: true}), 404)
	// A WebSocket's frames would go uncounted, so it's refused.
	r.platformWith(t, where("chat", 1), nil, nil, where("agent", 9), "agentic")
	want(t, r.do("GET", "/v1/responses", nil, opts{bearer: load, noOrigin: true, headers: map[string]string{
		"Upgrade": "websocket", "Connection": "Upgrade"}}), 400)
}

// An operator changes a service's settings and an owner its secrets, which reach patai-rootd as given and never the
// audit log.
func TestServiceSettingsAndSecrets(t *testing.T) {
	r := newRig(t)
	r.platform(t, nil, nil, nil)
	owner, _ := r.claim()
	op := r.token(owner, "ci", "operator")
	viewer := r.token(owner, "watch", "viewer")
	load, _ := r.key(owner, "app", store.Limits{})

	settings := "/api/v1/workloads/llm/settings"
	want(t, r.do("PUT", settings, map[string]any{"context": 16384, "temperature": nil}, opts{bearer: op, noOrigin: true}), 204)
	if got := r.rootd.called("service-settings"); len(got) != 1 || got[0].Target != "llm" ||
		string(got[0].Values) != `{"context":16384,"temperature":null}` {
		t.Fatalf("%+v", got)
	}
	want(t, r.do("PUT", settings, map[string]any{"context": 1}, opts{bearer: viewer, noOrigin: true}), 403)
	want(t, r.do("PUT", settings, map[string]any{"context": 1}, opts{bearer: load, noOrigin: true}), 403)
	want(t, r.do("PUT", settings, map[string]any{}, opts{bearer: op, noOrigin: true}), 400)
	want(t, r.do("PUT", "/api/v1/workloads/nothing/settings", map[string]any{"x": 1}, opts{bearer: op, noOrigin: true}), 404)
	// patai-workload's reason comes back as it gave it.
	r.rootd.refuse, r.rootd.only = "that needs 27 GiB, past the 26 GiB for apps", "service-settings"
	w := r.do("PUT", settings, map[string]any{"context": 131072}, opts{bearer: op, noOrigin: true})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "past the 26 GiB") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	r.rootd.refuse, r.rootd.only = "", ""

	secret := "/api/v1/workloads/agent/secrets/brave"
	want(t, r.do("PUT", secret, map[string]any{"value": "sk-not-for-the-log"}, opts{bearer: op, noOrigin: true}), 403)
	want(t, r.do("PUT", secret, map[string]any{"value": " sk-not-for-the-log\n"}, opts{cookie: owner}), 204)
	if got := r.rootd.called("service-secret"); len(got) != 1 || got[0].Target != "agent" ||
		string(got[0].Values) != `{"name":"brave","value":"sk-not-for-the-log"}` {
		t.Fatalf("%+v", got)
	}
	want(t, r.do("PUT", secret, map[string]any{"value": ""}, opts{cookie: owner}), 204)
	audit := r.do("GET", "/api/v1/audit", nil, opts{cookie: owner})
	if !strings.Contains(audit.Body.String(), "service-secret") || strings.Contains(audit.Body.String(), "sk-not-for-the-log") {
		t.Fatal(audit.Body.String())
	}
}
