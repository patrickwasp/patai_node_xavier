package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A service's models as the console describes them to an owner choosing which the apps offer: what each takes and
// gives, whether it calls tools, its effort levels, its price, a measure of how capable it is, and whether it suits
// patai, which needs tool calling for search, memory and documents (docs/apps.md, Models from services; patai_chat
// docs/research/model-providers.md). Each service lists its models its own way:
//
//   - OpenRouter says the most, and its list for the owner's key leaves out providers their privacy settings refuse.
//   - OpenAI's names a model and nothing more: models.dev, an open table keyed by OpenAI's names, says the rest.
//   - Anthropic's has each model's capabilities, and models.dev its price.
//
// How capable a model is comes from OpenRouter's public list, which carries Artificial Analysis's Intelligence Index;
// models.dev ties a model's name at OpenAI or Anthropic to its name at OpenRouter. Both lists are public and need no key,
// and nothing about the unit or its people is sent to read them.

// outsideModel is one of a service's models, as the console describes it, and as the apps are told of a chosen one.
type outsideModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Maker     string   `json:"maker,omitempty"`
	Context   int      `json:"context,omitempty"`
	MaxOutput int      `json:"max_output,omitempty"`
	Input     []string `json:"input"`
	Output    []string `json:"output"`
	Tools     *bool    `json:"tools"`
	Reasoning *bool    `json:"reasoning"`
	// The effort levels patai can ask it for, lowest first, and the one it uses unasked, if known.
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	Structured    *bool    `json:"structured,omitempty"`
	// US dollars a million tokens, when known.
	PriceIn  *float64 `json:"price_in,omitempty"`
	PriceOut *float64 `json:"price_out,omitempty"`
	// Artificial Analysis's Intelligence Index, 0 to 100, as OpenRouter publishes it, when it has one.
	Intelligence *float64 `json:"intelligence,omitempty"`
	Created      int64    `json:"created,omitempty"`
	Deprecated   bool     `json:"deprecated,omitempty"`
	// good: it calls tools and writes text. openrouter: it calls tools only through an API patai doesn't speak to this
	// service yet, so use it through OpenRouter. no-tools: it can't call tools, so most of patai doesn't work with it.
	// not-chat: it doesn't write text, such as embeddings or speech. unknown: nothing says.
	Fit string `json:"fit"`
}

func yes(b bool) *bool { return &b }

// fit says whether a model suits patai, from what's known of it.
func (m *outsideModel) fit() {
	switch {
	case !slices.Contains(m.Output, "text") || notChat.MatchString(m.ID):
		m.Fit = "not-chat"
	case m.Tools == nil:
		m.Fit = "unknown"
	case *m.Tools:
		m.Fit = "good"
	default:
		m.Fit = "no-tools"
	}
}

// notChat matches the names of models that don't answer a chat, whatever a list says: embeddings, moderation, speech,
// transcription, image and realtime models.
var notChat = regexp.MustCompile(`(?i)(embed|moderation|tts|whisper|transcribe|dall-e|gpt-image|chatgpt-image|realtime|gpt-audio|gpt-live|-audio-|sora|babbage|davinci)`)

// levels are the effort levels patai offers, lowest first, as Chat names them: Low to Max.
var levels = []string{"low", "medium", "high", "xhigh", "max"}

// efforts are the levels of `offered` that patai offers, lowest first.
func efforts(offered []string) []string {
	var out []string
	for _, level := range levels {
		if slices.Contains(offered, level) {
			out = append(out, level)
		}
	}
	return out
}

// round keeps a price or score to three places.
func round(v float64) *float64 {
	v = float64(int64(v*1000+0.5)) / 1000
	return &v
}

// openRouterModel is a model as OpenRouter's GET /api/v1/models lists it.
type openRouterModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Created       int64  `json:"created"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	SupportedParameters []string `json:"supported_parameters"`
	// The API paths it serves; a model without chat completions among them answers only batches or other APIs.
	Endpoints []string `json:"endpoints"`
	Reasoning *struct {
		// Highest first; none at all means it takes any.
		SupportedEfforts []string `json:"supported_efforts"`
		DefaultEffort    string   `json:"default_effort"`
	} `json:"reasoning"`
	Benchmarks struct {
		ArtificialAnalysis struct {
			IntelligenceIndex *float64 `json:"intelligence_index"`
		} `json:"artificial_analysis"`
	} `json:"benchmarks"`
	ExpirationDate string `json:"expiration_date"`
}

// perMillion is a price a token, as OpenRouter writes it, a million tokens. A negative one is a router's, set by
// where it sends each request.
func perMillion(price string) *float64 {
	v, err := strconv.ParseFloat(price, 64)
	if err != nil || v < 0 {
		return nil
	}
	return round(v * 1e6)
}

// unlisted says whether an OpenRouter entry is one to leave out: one only for batches, a router, or one past its end.
func (m openRouterModel) unlisted(now time.Time) bool {
	if strings.HasSuffix(m.ID, ":batch") || strings.HasPrefix(m.Pricing.Prompt, "-") {
		return true
	}
	if m.Endpoints != nil && !slices.Contains(m.Endpoints, "/api/v1/chat/completions") {
		return true
	}
	if end, err := time.Parse("2006-01-02", m.ExpirationDate); err == nil && end.Before(now) {
		return true
	}
	return false
}

func fromOpenRouter(m openRouterModel) outsideModel {
	out := outsideModel{ID: m.ID, Name: m.Name, Created: m.Created, Context: m.ContextLength,
		MaxOutput: m.TopProvider.MaxCompletionTokens, Input: m.Architecture.InputModalities,
		Output: m.Architecture.OutputModalities, PriceIn: perMillion(m.Pricing.Prompt), PriceOut: perMillion(m.Pricing.Completion),
		Intelligence: m.Benchmarks.ArtificialAnalysis.IntelligenceIndex}
	if maker, _, ok := strings.Cut(strings.TrimPrefix(m.ID, "~"), "/"); ok {
		out.Maker = maker
	}
	if out.Name == "" {
		out.Name = m.ID
	}
	// A maker's name before the model's, "Anthropic: Claude Opus 5.5", is the maker column's.
	if _, name, ok := strings.Cut(out.Name, ": "); ok {
		out.Name = name
	}
	if out.Intelligence != nil {
		out.Intelligence = round(*out.Intelligence)
	}
	if out.Input == nil {
		out.Input = []string{"text"}
	}
	if out.Output == nil {
		out.Output = []string{"text"}
	}
	params := m.SupportedParameters
	out.Tools = yes(slices.Contains(params, "tools"))
	out.Reasoning = yes(m.Reasoning != nil || slices.Contains(params, "reasoning"))
	if m.Reasoning != nil {
		if m.Reasoning.SupportedEfforts == nil {
			out.Efforts = []string{"low", "medium", "high"}
		} else {
			out.Efforts = efforts(m.Reasoning.SupportedEfforts)
		}
		if slices.Contains(out.Efforts, m.Reasoning.DefaultEffort) {
			out.DefaultEffort = m.Reasoning.DefaultEffort
		}
	}
	out.Structured = yes(slices.Contains(params, "structured_outputs") || slices.Contains(params, "response_format"))
	out.fit()
	return out
}

// devModel is a model as models.dev describes it.
type devModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Reasoning        bool   `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	ToolCall         bool   `json:"tool_call"`
	StructuredOutput bool   `json:"structured_output"`
	ReleaseDate      string `json:"release_date"`
	Status           string `json:"status"`
	CanonicalModelID string `json:"canonical_model_id"`
	Modalities       struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
}

// pdf is what models.dev calls a document a model reads; patai calls it a file.
func renamePDF(kinds []string) []string {
	out := []string{}
	for _, kind := range kinds {
		if kind == "pdf" {
			kind = "file"
		}
		if !slices.Contains(out, kind) {
			out = append(out, kind)
		}
	}
	return out
}

// fromDev describes a model by what models.dev says of it.
func fromDev(id, maker string, created int64, d devModel) outsideModel {
	out := outsideModel{ID: id, Name: d.Name, Maker: maker, Created: created, Context: d.Limit.Context, MaxOutput: d.Limit.Output,
		Input: renamePDF(d.Modalities.Input), Output: d.Modalities.Output, Tools: yes(d.ToolCall), Reasoning: yes(d.Reasoning),
		Structured: yes(d.StructuredOutput), Deprecated: d.Status == "deprecated"}
	if out.Name == "" {
		out.Name = id
	}
	if len(out.Input) == 0 {
		out.Input = []string{"text"}
	}
	if len(out.Output) == 0 {
		out.Output = []string{"text"}
	}
	for _, option := range d.ReasoningOptions {
		if option.Type == "effort" {
			out.Efforts = efforts(option.Values)
		}
	}
	if d.Cost != nil {
		out.PriceIn, out.PriceOut = round(d.Cost.Input), round(d.Cost.Output)
	}
	if out.Created == 0 {
		if t, err := time.Parse("2006-01-02", d.ReleaseDate); err == nil {
			out.Created = t.Unix()
		}
	}
	return out
}

// openAIModel is a model as OpenAI's GET /v1/models lists it, which says only its name, when it was made, and when it
// stops.
type openAIModel struct {
	ID           string `json:"id"`
	Created      int64  `json:"created"`
	ShutdownDate string `json:"shutdown_date"`
}

func fromOpenAI(m openAIModel, dev *devList, now time.Time) (outsideModel, bool) {
	if strings.HasPrefix(m.ID, "ft:") || m.ID == "chat-latest" || strings.HasSuffix(m.ID, "-chat-latest") {
		return outsideModel{}, false
	}
	if end, err := time.Parse("2006-01-02", m.ShutdownDate); err == nil && end.Before(now) {
		return outsideModel{}, false
	}
	var out outsideModel
	if d, ok := dev.models["openai"][m.ID]; ok {
		out = fromDev(m.ID, "openai", m.Created, d)
		out.Intelligence = dev.intelligence(d.CanonicalModelID)
	} else {
		out = outsideModel{ID: m.ID, Name: m.ID, Maker: "openai", Created: m.Created, Input: []string{"text"}, Output: []string{"text"}}
	}
	if m.ShutdownDate != "" {
		out.Deprecated = true
	}
	out.fit()
	return out, true
}

// anthropicModel is a model as Anthropic's GET /v1/models lists it: its capabilities a tree with "supported" at each
// leaf.
type anthropicModel struct {
	ID             string                     `json:"id"`
	DisplayName    string                     `json:"display_name"`
	CreatedAt      string                     `json:"created_at"`
	Lifecycle      string                     `json:"lifecycle"`
	MaxInputTokens int                        `json:"max_input_tokens"`
	MaxTokens      int                        `json:"max_tokens"`
	Capabilities   map[string]json.RawMessage `json:"capabilities"`
}

// supported is whether a capability's leaf says supported.
func supported(raw json.RawMessage) bool {
	var leaf struct {
		Supported bool `json:"supported"`
	}
	return json.Unmarshal(raw, &leaf) == nil && leaf.Supported
}

// The effort levels Claude can take, lowest first, as Anthropic's list names them.
var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// fromAnthropic describes a Claude model as Anthropic lists it, asked in Anthropic's own Messages API: photos and
// PDFs as it reads them, and the effort levels it takes, at high unasked.
func fromAnthropic(m anthropicModel, dev *devList) (outsideModel, bool) {
	if m.Lifecycle == "retired" {
		return outsideModel{}, false
	}
	out := outsideModel{ID: m.ID, Name: m.DisplayName, Maker: "anthropic", Context: m.MaxInputTokens,
		MaxOutput: m.MaxTokens, Input: []string{"text"}, Output: []string{"text"}, Deprecated: m.Lifecycle == "deprecated",
		// Every Claude model calls tools; the list doesn't say so.
		Tools: yes(true)}
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		out.Created = t.Unix()
	}
	if out.Name == "" {
		out.Name = m.ID
	}
	caps := m.Capabilities
	if supported(caps["image_input"]) {
		out.Input = append(out.Input, "image")
	}
	if supported(caps["pdf_input"]) {
		out.Input = append(out.Input, "file")
	}
	var levels map[string]json.RawMessage
	if json.Unmarshal(caps["effort"], &levels) == nil && supported(caps["effort"]) {
		for _, level := range claudeEfforts {
			if supported(levels[level]) {
				out.Efforts = append(out.Efforts, level)
			}
		}
		if slices.Contains(out.Efforts, "high") {
			out.DefaultEffort = "high"
		}
	}
	if raw, ok := caps["structured_outputs"]; ok {
		out.Structured = yes(supported(raw))
	}
	if raw, ok := caps["thinking"]; ok {
		out.Reasoning = yes(supported(raw))
	}
	if d, ok := dev.models["anthropic"][m.ID]; ok {
		if d.Cost != nil {
			out.PriceIn, out.PriceOut = round(d.Cost.Input), round(d.Cost.Output)
		}
		out.Intelligence = dev.intelligence(d.CanonicalModelID)
	}
	out.fit()
	return out, true
}

// devList is what the console reads of models.dev and OpenRouter's public list: models.dev's OpenAI and Anthropic
// models, and Artificial Analysis's index of each OpenRouter model, by models.dev's canonical name for it.
type devList struct {
	models map[string]map[string]devModel
	scores map[string]float64
}

// intelligence is the Intelligence Index of the model models.dev calls `canonical`, if OpenRouter has one.
func (d *devList) intelligence(canonical string) *float64 {
	if v, ok := d.scores[canonical]; ok && canonical != "" {
		return round(v)
	}
	return nil
}

// The two public lists, kept for an hour.
var public struct {
	mu     sync.Mutex
	at     time.Time
	router map[string]openRouterModel
	dev    *devList
}

// The public lists' addresses. Tests point them at stand-ins.
var (
	openRouterURL = "https://openrouter.ai/api/v1/models"
	modelsDevURL  = "https://models.dev/api.json"
)

func fetchJSON(ctx context.Context, url string, limit int64, into any) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(into) == nil
}

// publicLists reads OpenRouter's public list and models.dev, or keeps what it read within the hour. Either can be
// missing: a model is then described with less.
func publicLists(ctx context.Context) (map[string]openRouterModel, *devList) {
	public.mu.Lock()
	defer public.mu.Unlock()
	if public.dev != nil && time.Since(public.at) < time.Hour {
		return public.router, public.dev
	}
	var routed struct {
		Data []openRouterModel `json:"data"`
	}
	router := map[string]openRouterModel{}
	if fetchJSON(ctx, openRouterURL, 32<<20, &routed) {
		for _, m := range routed.Data {
			router[m.ID] = m
		}
	}
	var all map[string]struct {
		Models map[string]devModel `json:"models"`
	}
	dev := &devList{models: map[string]map[string]devModel{}, scores: map[string]float64{}}
	if fetchJSON(ctx, modelsDevURL, 64<<20, &all) {
		for _, name := range []string{"openai", "anthropic"} {
			dev.models[name] = all[name].Models
		}
		// OpenRouter's name for each canonical model, to find its score.
		for id, d := range all["openrouter"].Models {
			if m, ok := router[id]; ok && d.CanonicalModelID != "" && m.Benchmarks.ArtificialAnalysis.IntelligenceIndex != nil {
				dev.scores[d.CanonicalModelID] = *m.Benchmarks.ArtificialAnalysis.IntelligenceIndex
			}
		}
	}
	if len(router) > 0 || len(dev.models["openai"]) > 0 {
		public.router, public.dev, public.at = router, dev, time.Now()
	}
	return router, dev
}

// listModels asks a service for its models and describes each, the best fits first, then the most capable, then the
// newest.
func listModels(ctx context.Context, prov provider) ([]outsideModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path := "/models"
	switch prov.Kind {
	case "anthropic":
		path = "/models?limit=1000"
	case "openrouter":
		// The models the owner's settings at OpenRouter allow, such as which providers may keep what's sent.
		path = "/models/user"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(prov.Base, "/")+path, nil)
	if prov.Key != "" {
		req.Header.Set("Authorization", "Bearer "+prov.Key)
	}
	if prov.Kind == "anthropic" {
		req.Header.Set("X-Api-Key", prov.Key)
		req.Header.Set("Anthropic-Version", anthropicVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, serviceFailure{"service-down", "the service isn't answering"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, serviceFailure{"service-key", "the service refused the key"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, serviceFailure{"service-down", "the service answered " + resp.Status}
	}
	var listed struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&listed); err != nil {
		return nil, serviceFailure{"service-answer", "the service's list of models couldn't be read"}
	}
	now := time.Now()
	models := []outsideModel{}
	switch prov.Kind {
	case "anthropic":
		_, dev := publicLists(ctx)
		var list []anthropicModel
		json.Unmarshal(listed.Data, &list)
		for _, m := range list {
			if out, ok := fromAnthropic(m, dev); ok {
				models = append(models, out)
			}
		}
	case "openai":
		_, dev := publicLists(ctx)
		var list []openAIModel
		json.Unmarshal(listed.Data, &list)
		for _, m := range list {
			if out, ok := fromOpenAI(m, dev, now); ok {
				models = append(models, out)
			}
		}
	default:
		// OpenRouter's, and any other service's that lists them as OpenAI's or OpenRouter's do.
		var list []openRouterModel
		json.Unmarshal(listed.Data, &list)
		// The list for a key leaves out the scores; the public list has them.
		var public map[string]openRouterModel
		if prov.Kind == "openrouter" {
			public, _ = publicLists(ctx)
		}
		for _, m := range list {
			if m.ID == "" || m.unlisted(now) {
				continue
			}
			if score := &m.Benchmarks.ArtificialAnalysis.IntelligenceIndex; *score == nil {
				*score = public[m.ID].Benchmarks.ArtificialAnalysis.IntelligenceIndex
			}
			if prov.Kind == "openrouter" || m.SupportedParameters != nil || m.Architecture.OutputModalities != nil {
				models = append(models, fromOpenRouter(m))
				continue
			}
			out := outsideModel{ID: m.ID, Name: m.Name, Input: []string{"text"}, Output: []string{"text"}}
			if out.Name == "" {
				out.Name = m.ID
			}
			out.fit()
			models = append(models, out)
		}
	}
	rank := map[string]int{"good": 0, "openrouter": 1, "unknown": 2, "no-tools": 3, "not-chat": 4}
	score := func(m outsideModel) float64 {
		if m.Intelligence == nil {
			return -1
		}
		return *m.Intelligence
	}
	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if rank[a.Fit] != rank[b.Fit] {
			return rank[a.Fit] < rank[b.Fit]
		}
		if score(a) != score(b) {
			return score(a) > score(b)
		}
		return a.Created > b.Created
	})
	return models, nil
}

type serviceFailure struct{ code, message string }

func (f serviceFailure) Error() string { return f.message }

// anthropicVersion is the version of Anthropic's API the console asks for. Its OpenAI compatibility, which the gateway
// uses, keeps to it.
const anthropicVersion = "2023-06-01"
