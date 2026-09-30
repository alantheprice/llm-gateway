package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeNinfer: a NInfer-shaped engine with a fixed context window and a
// settable lane load; counts chat requests served.
type fakeNinfer struct {
	srv     *httptest.Server
	maxCtx  int
	running atomic.Int64
	served  atomic.Int64
}

func newFakeNinfer(t *testing.T, maxCtx int) *fakeNinfer {
	f := &fakeNinfer{maxCtx: maxCtx}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/slots":
			fmt.Fprintf(w, `{"max_concurrency":8,"requests_processing":%d,"requests_waiting":0}`, f.running.Load())
		case "/usage":
			fmt.Fprint(w, `{}`)
		case "/v1/models":
			fmt.Fprintf(w, `{"data":[{"id":"m","max_model_len":%d}]}`, f.maxCtx)
		case "/v1/chat/completions":
			f.served.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
				"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 1},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func fitConf(a, b string) string {
	return `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"qwen":{"members":[{"model_id":"m","backend":"` + a + `"},{"model_id":"m","backend":"` + b + `"}],
	 "overflow_threshold":0.2,"cache_affinity":true}},"public_models":["qwen"]}`
}

// fakeOpenaiModel: an OpenAI-API engine that reports one model's context
// window; the basic poller counts it as up.
func fakeOpenaiModel(t *testing.T, id string, maxModelLen int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"data":[{"id":%q,"max_model_len":%d}]}`, id, maxModelLen)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			fmt.Fprintf(w, `{"id":"x","model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, id)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chat(t *testing.T, s *Server, messages string) int {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":`+messages+`}`))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	return w.Code
}

// A request larger than a member's engine-reported context window skips
// that member, even when it is idle and first in the pool.
func TestContextFitSkipsSmallWindow(t *testing.T) {
	small, big := newFakeNinfer(t, 32000), newFakeNinfer(t, 262144)
	s := testServer(t, fitConf(small.srv.URL, big.srv.URL), nil)
	s.PollOnce() // learns both windows from /v1/models
	if s.maxContext(small.srv.URL) != 32000 || s.maxContext(big.srv.URL) != 262144 {
		t.Fatalf("windows not learned: %d / %d", s.maxContext(small.srv.URL), s.maxContext(big.srv.URL))
	}
	huge := strings.Repeat("x", 200000) // ~50K tokens by the chars/4 estimate
	if code := chat(t, s, `[{"role":"user","content":"`+huge+`"}]`); code != 200 {
		t.Fatalf("status %d", code)
	}
	if small.served.Load() != 0 || big.served.Load() != 1 {
		t.Fatalf("served small=%d big=%d; the 50K prompt must skip the 32K engine",
			small.served.Load(), big.served.Load())
	}
	// A small prompt may use either (the idle first member wins the tie).
	if code := chat(t, s, `[{"role":"user","content":"hi"}]`); code != 200 {
		t.Fatalf("status %d", code)
	}
	if small.served.Load() != 1 {
		t.Fatalf("small prompt should use the idle first member")
	}
}

// Cache affinity holds for prompts of any size: a long conversation's
// next turn returns to the GPU that served it, even when another member
// is now idle and would win on score.
func TestAffinityAppliesToLargePrompts(t *testing.T) {
	a, b := newFakeNinfer(t, 262144), newFakeNinfer(t, 262144)
	s := testServer(t, fitConf(a.srv.URL, b.srv.URL), nil)
	a.running.Store(8) // first member saturated: turn 1 lands on b
	s.PollOnce()
	big := strings.Repeat("y", 240000) // ~60K tokens, above the old 48K cutoff
	turn1 := `[{"role":"system","content":"` + big + `"},{"role":"user","content":"q1"}]`
	if code := chat(t, s, turn1); code != 200 || b.served.Load() != 1 {
		t.Fatalf("turn 1: status %d, b served %d", code, b.served.Load())
	}
	a.running.Store(0) // a is idle again and first in order
	s.PollOnce()
	turn2 := `[{"role":"system","content":"` + big + `"},{"role":"user","content":"q1"},` +
		`{"role":"assistant","content":"ok"},{"role":"user","content":"q2"}]`
	if code := chat(t, s, turn2); code != 200 {
		t.Fatalf("turn 2 status %d", code)
	}
	if b.served.Load() != 2 || a.served.Load() != 0 {
		t.Fatalf("turn 2 left its cache: a=%d b=%d", a.served.Load(), b.served.Load())
	}
}

// GET /v1/models publishes each model's context window: a shared model's is
// its members' largest (a config max_context override beats the engine's
// report for that member), a standalone model's is its engine's report.
func TestModelsPublishContextLength(t *testing.T) {
	small, big := newFakeNinfer(t, 32000), newFakeNinfer(t, 262144)
	solo := fakeOpenaiModel(t, "solo-model", 131072)
	conf := fmt.Sprintf(`{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"qwen":{"members":[
	    {"model_id":"m","backend":"%s"},
	    {"model_id":"m","backend":"%s","max_context":8192}]}},
	 "public_models":["qwen"]}`, small.srv.URL, big.srv.URL)
	s := testServer(t, conf, nil)
	s.mu.Lock()
	s.backends[solo.URL] = &BackendInfo{Models: []string{"solo-model"}, Chat: true}
	s.mu.Unlock()
	s.PollOnce() // learns each engine's context window from /v1/models

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	var out struct {
		Data []ModelEntry `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for _, e := range out.Data {
		byID[e.ID] = e.ContextLength
	}
	// qwen: max member window — the 8192 override caps the 262144 member, so
	// the unoverridden 32000 member dominates.
	if got := byID["qwen"]; got != 32000 {
		t.Errorf("qwen context_length = %d, want 32000", got)
	}
	// solo-model: its engine-reported window.
	if got := byID["solo-model"]; got != 131072 {
		t.Errorf("solo-model context_length = %d, want 131072", got)
	}
}
