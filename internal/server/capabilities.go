package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"llmgateway/internal/config"
)

// Model capabilities: what each model can do, so the gateway can publish it
// (GET /v1/models), refuse requests a model can't serve with a clear error,
// route requests that need something specific (images, tools) only to GPUs
// that have it, and let the UI offer the right models.
//
// Sources, strongest first:
//   - config: an operator override (model_capabilities.<name>);
//   - engine: what the engine reports — the NInfer model card, llama.cpp
//     /props, Ollama's /api/show;
//   - name: a guess from the model id ("embed" → embeddings). Guesses are
//     used for the endpoint check only; inputs and features are enforced
//     only when an engine or the operator stated them.

// Capabilities of one model.
type Capabilities struct {
	Endpoints []string `json:"endpoints"`          // chat, completions, embeddings, images
	Input     []string `json:"input_modalities"`   // text, image, audio, video
	Output    []string `json:"output_modalities"`  // text, embeddings
	Features  []string `json:"features,omitempty"` // tools, thinking
	Source    string   `json:"source"`             // engine | config | name
}

func (c Capabilities) has(list []string, v string) bool { return slices.Contains(list, v) }

// nameCaps: the fallback guess from a model id.
func nameCaps(id string) Capabilities {
	if kindFor(id) == "images" {
		return Capabilities{Endpoints: []string{"images"}, Input: []string{"text"},
			Output: []string{"image"}, Source: "name"}
	}
	if kindFor(id) == "embeddings" {
		return Capabilities{Endpoints: []string{"embeddings"}, Input: []string{"text"},
			Output: []string{"embeddings"}, Source: "name"}
	}
	return Capabilities{Endpoints: []string{"chat", "completions"}, Input: []string{"text"},
		Output: []string{"text"}, Source: "name"}
}

// ninferCardCaps: capabilities from a NInfer model card.
func ninferCardCaps(card map[string]any) Capabilities {
	c := Capabilities{Endpoints: []string{"chat"}, Input: []string{"text"}, Output: []string{"text"},
		Features: []string{"tools"}, Source: "engine"}
	serving, _ := card["serving"].(map[string]any)
	mods, _ := serving["modalities"].(map[string]any)
	if v, _ := mods["vision"].(bool); v {
		c.Input = append(c.Input, "image")
	}
	if v, _ := mods["video"].(bool); v {
		c.Input = append(c.Input, "video")
	}
	if v, _ := mods["audio"].(bool); v {
		c.Input = append(c.Input, "audio")
	}
	switch t := mods["thinking"].(type) {
	case bool:
		if t {
			c.Features = append(c.Features, "thinking")
		}
	case string:
		if t != "" && !strings.HasPrefix(strings.ToLower(t), "off") {
			c.Features = append(c.Features, "thinking")
		}
	}
	return c
}

// detectCaps works out capabilities for each model an engine serves.
// Called from refreshMaxContext (every few minutes per engine).
func (s *Server) detectCaps(url string, ids []string, engineID string, card map[string]any) map[string]Capabilities {
	out := map[string]Capabilities{}
	engine := ""
	if l := s.tracker.Get(url); l != nil {
		engine = l.Engine
	}
	// llama.cpp: /props once per engine.
	var props map[string]any
	if engine == "llamacpp" {
		props, _ = s.backendJSON(url, "/props", 3*time.Second)
	}
	if len(ids) > 20 {
		ids = ids[:20]
	}
	for _, id := range ids {
		c := nameCaps(id)
		switch {
		case card != nil && id == engineID && strings.HasPrefix(fmt.Sprint(card["schema"]), "ninfer-model-card/"):
			c = ninferCardCaps(card)
		case props != nil:
			c = Capabilities{Endpoints: []string{"chat", "completions"}, Input: []string{"text"},
				Output: []string{"text"}, Source: "engine"}
			if mods, ok := props["modalities"].(map[string]any); ok {
				if v, _ := mods["vision"].(bool); v {
					c.Input = append(c.Input, "image")
				}
				if v, _ := mods["audio"].(bool); v {
					c.Input = append(c.Input, "audio")
				}
			}
		case engine != "ninfer" && engine != "llamacpp" && engine != "vllm":
			if oc, ok := s.ollamaCaps(url, id); ok {
				c = oc
			}
		}
		out[id] = c
	}
	return out
}

// ollamaCaps: Ollama's POST /api/show lists capabilities (completion,
// vision, tools, thinking, embedding).
func (s *Server) ollamaCaps(url, id string) (Capabilities, bool) {
	body, _ := json.Marshal(map[string]string{"model": id})
	status, b, err := s.backendRequest(http.MethodPost, url, "/api/show", body, 3*time.Second)
	if err != nil || status != http.StatusOK {
		return Capabilities{}, false
	}
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(b, &show) != nil || len(show.Capabilities) == 0 {
		return Capabilities{}, false
	}
	c := Capabilities{Input: []string{"text"}, Source: "engine"}
	for _, k := range show.Capabilities {
		switch k {
		case "completion":
			c.Endpoints = append(c.Endpoints, "chat", "completions")
			c.Output = append(c.Output, "text")
		case "embedding":
			c.Endpoints = append(c.Endpoints, "embeddings")
			c.Output = append(c.Output, "embeddings")
		case "vision":
			c.Input = append(c.Input, "image")
		case "tools", "thinking":
			c.Features = append(c.Features, k)
		}
	}
	return c, len(c.Endpoints) > 0
}

// engineCaps: detected capabilities of modelID on one backend (name guess
// until the engine has been inspected).
func (s *Server) engineCaps(url, modelID string) Capabilities {
	s.mu.Lock()
	info := s.maxCtx[url]
	s.mu.Unlock()
	if c, ok := info.caps[modelID]; ok {
		return c
	}
	return nameCaps(modelID)
}

// withOverride applies model_capabilities.<name> when set.
func (s *Server) withOverride(name string, c Capabilities) Capabilities {
	s.mu.Lock()
	o, ok := s.cfg.ModelCapabilities[name]
	s.mu.Unlock()
	if !ok {
		return c
	}
	return applyOverride(c, o)
}

func applyOverride(c Capabilities, o config.CapabilityOverride) Capabilities {
	changed := false
	if len(o.Endpoints) > 0 {
		c.Endpoints, changed = o.Endpoints, true
	}
	if len(o.Input) > 0 {
		c.Input, changed = o.Input, true
	}
	if len(o.Output) > 0 {
		c.Output, changed = o.Output, true
	}
	if o.Features != nil {
		c.Features, changed = o.Features, true
	}
	if changed {
		c.Source = "config"
	}
	return c
}

// unionCaps merges members' capabilities (a shared model can do what any of
// its GPUs can; requests needing something are routed to GPUs that have it).
func unionCaps(cs []Capabilities) Capabilities {
	add := func(dst []string, src []string) []string {
		for _, v := range src {
			if !slices.Contains(dst, v) {
				dst = append(dst, v)
			}
		}
		return dst
	}
	var u Capabilities
	u.Source = "name"
	for _, c := range cs {
		u.Endpoints = add(u.Endpoints, c.Endpoints)
		u.Input = add(u.Input, c.Input)
		u.Output = add(u.Output, c.Output)
		u.Features = add(u.Features, c.Features)
		if c.Source != "name" {
			u.Source = c.Source
		}
	}
	sort.Strings(u.Endpoints)
	sort.Strings(u.Features)
	sortModalities(u.Input)
	sortModalities(u.Output)
	return u
}

// sortModalities: conventional order (text first), as clients print them.
func sortModalities(l []string) {
	rank := map[string]int{"text": 0, "image": 1, "audio": 2, "video": 3, "embeddings": 4}
	// (output "image" = generated images; ranks like the input modality)
	sort.SliceStable(l, func(i, j int) bool {
		ri, ok := rank[l[i]]
		if !ok {
			ri = 9
		}
		rj, ok := rank[l[j]]
		if !ok {
			rj = 9
		}
		return ri < rj
	})
}

// capsFor: capabilities of a name a client calls (shared model or alias,
// single engine, or a private link the caller can use). ok is false when
// nothing serves the name.
func (s *Server) capsFor(name, user string) (Capabilities, bool) {
	if poolName, pool, ok := s.poolFor(name); ok {
		var cs []Capabilities
		for _, m := range pool.Members {
			cs = append(cs, s.engineCaps(m.Backend, m.ModelID))
		}
		c := s.withOverride(poolName, unionCaps(cs))
		if name != poolName {
			c = s.withOverride(name, c)
		}
		return c, true
	}
	if url, _ := s.resolve(name); url != "" {
		return s.withOverride(name, s.engineCaps(url, name)), true
	}
	if pm, ok := s.resolvePrivate(user, name); ok {
		return s.withOverride(name, s.engineCaps(pm.URL, pm.ModelID)), true
	}
	return Capabilities{}, false
}

// capNeeds: what one request requires of the model.
type capNeeds struct {
	Endpoint string   // chat | completions | embeddings
	Input    []string // non-text inputs present (image, audio, video)
	Features []string // tools
}

// requestNeeds reads the endpoint and body.
func requestNeeds(path string, body []byte) capNeeds {
	n := capNeeds{Endpoint: "chat"}
	switch {
	case strings.HasSuffix(path, "/embeddings"):
		n.Endpoint = "embeddings"
		return n
	case strings.HasSuffix(path, "/images/generations"):
		n.Endpoint = "images"
		return n
	case strings.HasSuffix(path, "/chat/completions"):
	case strings.HasSuffix(path, "/completions"):
		n.Endpoint = "completions"
		return n
	}
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return n
	}
	if len(req.Tools) > 0 {
		n.Features = append(n.Features, "tools")
	}
	for _, m := range req.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue // plain string content
		}
		for _, p := range parts {
			in := ""
			switch p.Type {
			case "image_url", "input_image", "image":
				in = "image"
			case "input_audio", "audio":
				in = "audio"
			case "video_url", "video":
				in = "video"
			}
			if in != "" && !slices.Contains(n.Input, in) {
				n.Input = append(n.Input, in)
			}
		}
	}
	return n
}

// supports reports whether c can serve n, and if not, what's missing in
// words. Guessed (name-sourced) capabilities only refuse text generation on
// embedding-looking names, so an engine nobody has described never loses
// traffic to a wrong guess.
func (c Capabilities) supports(n capNeeds) (bool, string) {
	missingEndpoint := map[string]string{
		"chat": "chat", "completions": "text completions", "embeddings": "embeddings",
		"images": "image generation",
	}[n.Endpoint]
	if c.Source == "name" {
		// A guess is only trusted in its one confident case: a name that
		// looks like an embedding or image model can't generate text.
		// Anything else goes to the engine, which knows.
		textGen := n.Endpoint == "chat" || n.Endpoint == "completions"
		if textGen && !c.has(c.Endpoints, n.Endpoint) && (c.has(c.Endpoints, "embeddings") || c.has(c.Endpoints, "images")) {
			return false, missingEndpoint
		}
		return true, ""
	}
	if !c.has(c.Endpoints, n.Endpoint) {
		return false, missingEndpoint
	}
	for _, in := range n.Input {
		if !c.has(c.Input, in) {
			return false, in + " input"
		}
	}
	for _, f := range n.Features {
		if !c.has(c.Features, f) {
			return false, f
		}
	}
	return true, ""
}

// capabilityError: the 400 for a request the model can't serve.
func capabilityError(w http.ResponseWriter, model, missing string) {
	capabilityErrorHint(w, model, missing, "")
}

func capabilityErrorHint(w http.ResponseWriter, model, missing, hint string) {
	msg := fmt.Sprintf("%s doesn't support %s", model, missing)
	if hint != "" {
		msg += "; " + hint
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"message": msg, "type": "invalid_request_error", "code": "model_capability",
	}})
}

// endpointPath: where a capability's requests go.
var endpointPath = map[string]string{
	"chat": "/v1/chat/completions", "completions": "/v1/completions", "embeddings": "/v1/embeddings",
	"images": "/v1/images/generations",
}

// checkCaps writes a 400 and returns false when c can't serve this request
// (single engines and private links; pools filter their GPUs instead).
func checkCaps(w http.ResponseWriter, r *http.Request, name string, c Capabilities, body []byte) bool {
	needs := requestNeeds(r.URL.Path, body)
	ok, missing := c.supports(needs)
	if ok {
		return true
	}
	hint := ""
	if !c.has(c.Endpoints, needs.Endpoint) {
		for _, e := range []string{"chat", "embeddings", "images", "completions"} {
			if c.has(c.Endpoints, e) {
				hint = "call it at " + endpointPath[e]
				break
			}
		}
	}
	capabilityErrorHint(w, name, missing, hint)
	return false
}

// describe fills a catalog entry's capabilities, architecture and context
// window.
func (s *Server) describe(e ModelEntry, user string) ModelEntry {
	c, ok := s.capsFor(e.ID, user)
	if !ok {
		return e
	}
	e.Capabilities = &c
	e.Architecture = &Architecture{
		Modality:         strings.Join(c.Input, "+") + "->" + strings.Join(c.Output, "+"),
		InputModalities:  c.Input,
		OutputModalities: c.Output,
	}
	e.ContextLength = s.contextLength(e.ID, user)
	return e
}

// contextLength: the context window of a model name (0 when unknown).
// A shared model's window is its members' largest (config max_context
// override beats the engine-reported max_model_len), so clients see the
// size requests are actually fit-tested against; a standalone or private
// model uses its own backend's value.
func (s *Server) contextLength(name, user string) int {
	if _, pool, ok := s.poolFor(name); ok {
		best := 0
		for _, m := range pool.Members {
			n := m.MaxContext
			if n == 0 {
				n = s.maxContext(m.Backend)
			}
			if n > best {
				best = n
			}
		}
		return best
	}
	if url, _ := s.resolve(name); url != "" {
		return s.maxContext(url)
	}
	if pm, ok := s.resolvePrivate(user, name); ok {
		return s.maxContext(pm.URL)
	}
	return 0
}

// detectedCaps: capabilities of a name before any admin override (for the
// Routing editor to show what was detected next to what is set).
func (s *Server) detectedCaps(name string) *Capabilities {
	if _, pool, ok := s.poolFor(name); ok {
		var cs []Capabilities
		for _, m := range pool.Members {
			cs = append(cs, s.engineCaps(m.Backend, m.ModelID))
		}
		c := unionCaps(cs)
		return &c
	}
	if url, _ := s.resolve(name); url != "" {
		c := s.engineCaps(url, name)
		return &c
	}
	return nil
}

// imageModels: names the user can generate images with (shared models and
// engines, then their own and shared GPUs), for the Images page.
func (s *Server) imageModels(user string) []string { return s.modelsFor(user, "images") }

// embeddingModels: names the user can call /v1/embeddings with.
func (s *Server) embeddingModels(user string) []string { return s.modelsFor(user, "embeddings") }

// modelsFor: catalog and private names whose capabilities include endpoint.
func (s *Server) modelsFor(user, endpoint string) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if c, ok := s.capsFor(name, user); ok && c.has(c.Endpoints, endpoint) {
			out = append(out, name)
		}
	}
	for _, e := range s.catalog() {
		add(e.ID)
	}
	for _, pm := range s.privateModels(user) {
		add(pm.Name)
	}
	return out
}
