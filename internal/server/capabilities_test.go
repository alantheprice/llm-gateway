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
