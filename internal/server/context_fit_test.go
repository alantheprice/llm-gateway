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
