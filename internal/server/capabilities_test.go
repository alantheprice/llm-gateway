package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeEngine serves one model id with a chosen metadata style and records
// which backend answered chat.
type fakeEngine struct {
	*httptest.Server
	mu    sync.Mutex
	chats int
}

func newFakeEngine(t *testing.T, id, style string) *fakeEngine {
	t.Helper()
	fe := &fakeEngine{}
	fe.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, id)
		case r.URL.Path == "/slots" && style == "llamacpp":
			fmt.Fprint(w, `[{"id":0,"is_processing":false},{"id":1,"is_processing":false}]`)
		case r.URL.Path == "/props" && style == "llamacpp":
			fmt.Fprint(w, `{"modalities":{"vision":true,"audio":false}}`)
		case r.URL.Path == "/api/show" && style == "ollama":
			fmt.Fprint(w, `{"capabilities":["completion","tools","thinking"]}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			io.Copy(io.Discard, r.Body)
			fe.mu.Lock()
			fe.chats++
			fe.mu.Unlock()
			fmt.Fprintf(w, `{"id":"x","model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, id)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(fe.Close)
	return fe
}

func (fe *fakeEngine) chatCount() int { fe.mu.Lock(); defer fe.mu.Unlock(); return fe.chats }

func chatLAN(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

const imageMsg = `{"model":"%s","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`

// Engines report capabilities: llama.cpp /props (vision), Ollama /api/show
// (tools, thinking, no vision). A shared model sends image requests only to
// the GPU that can see, and refuses them clearly when none can.
func TestCapabilitiesDetectAndRoute(t *testing.T) {
	vision := newFakeEngine(t, "vis-model", "llamacpp")
	textOnly := newFakeEngine(t, "txt-model", "ollama")
	conf := `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"mixed":{"overflow_threshold":0.99,"members":[
	   {"model_id":"vis-model","backend":"` + vision.URL + `"},{"model_id":"txt-model","backend":"` + textOnly.URL + `"}]},
	               "blind":{"overflow_threshold":0.99,"members":[{"model_id":"txt-model","backend":"` + textOnly.URL + `"}]}}}`
	s := testServer(t, conf, nil)
	s.PollOnce() // polls both, inspects their capabilities

	vc := s.engineCaps(vision.URL, "vis-model")
	if vc.Source != "engine" || !slices.Contains(vc.Input, "image") {
		t.Fatalf("llama.cpp caps = %+v", vc)
	}
	tc := s.engineCaps(textOnly.URL, "txt-model")
	if tc.Source != "engine" || slices.Contains(tc.Input, "image") || !slices.Contains(tc.Features, "tools") {
		t.Fatalf("ollama caps = %+v", tc)
	}
	for i := 0; i < 4; i++ {
		if code, body := chatLAN(t, s, fmt.Sprintf(imageMsg, "mixed")); code != 200 {
			t.Fatalf("image via mixed pool: %d %s", code, body)
		}
	}
	if vision.chatCount() != 4 || textOnly.chatCount() != 0 {
		t.Fatalf("image requests went to vision=%d text=%d; want all on vision", vision.chatCount(), textOnly.chatCount())
	}
	code, body := chatLAN(t, s, fmt.Sprintf(imageMsg, "blind"))
	if code != 400 || !strings.Contains(body, "image input") {
		t.Fatalf("image to text-only model: %d %s", code, body)
	}
	if code, _ := chatLAN(t, s, `{"model":"blind","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("plain text to text-only model: %d", code)
	}
}

// /v1/models publishes capabilities and an OpenRouter-style architecture;
// a config override wins over detection.
func TestCapabilitiesPublishedAndOverridden(t *testing.T) {
	vision := newFakeEngine(t, "vis-model", "llamacpp")
	conf := `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"seer":{"members":[{"model_id":"vis-model","backend":"` + vision.URL + `"}]}},
	 "model_capabilities":{"seer":{"features":["tools"]}}}`
	s := testServer(t, conf, nil)
	s.PollOnce()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	var out struct {
		Data []ModelEntry `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	var seer *ModelEntry
	for i := range out.Data {
		if out.Data[i].ID == "seer" {
			seer = &out.Data[i]
		}
	}
	if seer == nil || seer.Capabilities == nil || seer.Architecture == nil {
		t.Fatalf("seer not described: %s", w.Body)
	}
	if seer.Architecture.Modality != "text+image->text" || seer.Capabilities.Source != "config" ||
		!slices.Contains(seer.Capabilities.Features, "tools") {
		t.Fatalf("seer = %+v %+v", seer.Capabilities, seer.Architecture)
	}
}

// NInfer model cards state vision and thinking.
func TestNinferCardCaps(t *testing.T) {
	c := ninferCardCaps(map[string]any{"serving": map[string]any{"modalities": map[string]any{
		"vision": true, "thinking": "on by default"}}})
	if !slices.Contains(c.Input, "image") || !slices.Contains(c.Features, "thinking") || c.Source != "engine" {
		t.Fatalf("card caps = %+v", c)
	}
}

// Engines without load metrics (Ollama, MLX, LM Studio) are polled as up
// via /v1/models, and Ollama's /api/show capabilities are read.
func TestBasicPollAndOllamaCaps(t *testing.T) {
	ol := newFakeEngine(t, "llama3.1:8b", "ollama")
	s := testServer(t, `{"gateway":{"port":0},"metrics":{"default_max_seqs":4}}`, nil)
	s.mu.Lock()
	s.backends[ol.URL] = &BackendInfo{Models: []string{"llama3.1:8b"}, Chat: true}
	s.mu.Unlock()
	s.PollOnce()
	l := s.tracker.Get(ol.URL)
	if l == nil || l.Engine != "openai" || l.Lanes != 4 || s.tracker.IsDown(ol.URL) {
		t.Fatalf("basic poll = %+v down=%v", l, s.tracker.IsDown(ol.URL))
	}
	c := s.engineCaps(ol.URL, "llama3.1:8b")
	if c.Source != "engine" || !slices.Contains(c.Features, "thinking") {
		t.Fatalf("ollama caps = %+v", c)
	}
}

// Agent downloads: only supported platforms; a missing build says which.
func TestAgentDownloadPlatforms(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	for path, want := range map[string]int{
		"/downloads/llm-link-agent-windows-amd64": 404,
		"/downloads/llm-link-agent-..%2fetc":      404,
		"/downloads/llm-link-agent-darwin-arm64":  404, // not built in tests
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s = %d, want %d", path, w.Code, want)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/downloads/llm-link-agent-darwin-arm64", nil))
	if !strings.Contains(w.Body.String(), "darwin-arm64") {
		t.Errorf("missing-build message = %s", w.Body)
	}
}

// A tagged release without a local agent build redirects to its own
// GitHub release asset.
func TestAgentDownloadReleaseFallback(t *testing.T) {
	old := Version
	Version = "v9.9.9"
	defer func() { Version = old }()
	t.Setenv("LLM_GATEWAY_RELEASE_REPO", "owner/repo")
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/downloads/llm-link-agent-darwin-arm64", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://github.com/owner/repo/releases/download/v9.9.9/llm-link-agent-darwin-arm64" {
		t.Fatalf("fallback = %d %s", w.Code, w.Header().Get("Location"))
	}
}

// Text-to-image: /v1/images/generations reaches an image engine; chatting
// with an image model points at the right endpoint; a model whose engine
// says it can't generate images is refused; an unknown name isn't blocked
// by a guess.
func TestImageGeneration(t *testing.T) {
	var hits int
	var mu sync.Mutex
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"flux-schnell"},{"id":"mystery-model"}]}`)
		case "/v1/images/generations":
			mu.Lock()
			hits++
			mu.Unlock()
			fmt.Fprint(w, `{"created":1,"data":[{"b64_json":"iVBORw0KGgo="}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer img.Close()
	ol := newFakeEngine(t, "llama3.1:8b", "ollama") // reports completion/tools/thinking only
	s := testServer(t, `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"]}`, nil)
	s.mu.Lock()
	s.backends[img.URL] = &BackendInfo{Models: []string{"flux-schnell", "mystery-model"}, Chat: true}
	s.backends[ol.URL] = &BackendInfo{Models: []string{"llama3.1:8b"}, Chat: true}
	s.mu.Unlock()
	s.PollOnce()
	post := func(path, body string) (int, string) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.RemoteAddr = "192.168.1.50:5555"
		s.Handler().ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := post("/v1/images/generations", `{"model":"flux-schnell","prompt":"a red fox"}`); code != 200 || !strings.Contains(body, "b64_json") {
		t.Fatalf("image gen: %d %s", code, body)
	}
	if code, body := post("/v1/images/generations", `{"model":"mystery-model","prompt":"a red fox"}`); code != 200 {
		t.Fatalf("unknown-name engine blocked by a guess: %d %s", code, body)
	}
	if code, body := post("/v1/chat/completions", `{"model":"flux-schnell","messages":[{"role":"user","content":"hi"}]}`); code != 400 || !strings.Contains(body, "/v1/images/generations") {
		t.Fatalf("chat to image model: %d %s", code, body)
	}
	if code, body := post("/v1/images/generations", `{"model":"llama3.1:8b","prompt":"a red fox"}`); code != 400 || !strings.Contains(body, "image generation") {
		t.Fatalf("images to a chat-only model: %d %s", code, body)
	}
	if hits != 2 {
		t.Fatalf("image engine hits = %d, want 2", hits)
	}
	if c, _ := s.capsFor("flux-schnell", ""); !slices.Contains(c.Output, "image") {
		t.Fatalf("flux caps = %+v", c)
	}
}
