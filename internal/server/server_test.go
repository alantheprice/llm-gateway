package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/config"
)

func testServer(t *testing.T, confJSON string, users *auth.Store) *Server {
	t.Helper()
	dir := t.TempDir()
	confPath := filepath.Join(dir, "conf.json")
	if err := os.WriteFile(confPath, []byte(confJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(confPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.UsersFile = filepath.Join(dir, "users.json")
	cfg.Gateway.UsageFile = filepath.Join(dir, "usage.json")
	if users == nil {
		users, _ = auth.Open(cfg.Gateway.UsersFile)
	}
	s := New(cfg, users)
	s.store.LegacyKeysFile = cfg.Gateway.APIKeysFile
	s.ParseNetworks()
	s.Discover()
	return s
}

const twoBackendConf = `{
  "gateway": {"port": 0, "trust_local_networks": true},
  "local_networks": ["192.168.1.0/24"],
  "model_pools": {"qwen": {"members": [
    {"model_id": "m-a", "backend": "%BACKEND_A%"},
    {"model_id": "m-b", "backend": "%BACKEND_B%", "large_context": true, "capacity_weight": 4}
  ], "overflow_threshold": 0.2, "sticky_bias": 0.05, "large_prompt_tokens": 1000}},
  "public_models": ["qwen"]
}`

func TestHealthNoAuth(t *testing.T) {
	s := testServer(t, `{}`, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 200 || w.Body.String() != "OK" {
		t.Fatalf("health = %d %q", w.Code, w.Body.String())
	}
}

func TestAuthRequiredFromLoopback(t *testing.T) {
	// 127.0.0.1 is tunnel traffic: never trusted (SPEC §2).
	s := testServer(t, `{"gateway":{"trust_local_networks":true},"local_networks":["192.168.1.0/24"]}`, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != 401 {
		t.Fatalf("loopback without key = %d, want 401", w.Code)
	}
	// LAN IP is trusted when trust_local_networks
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.RemoteAddr = "192.168.1.50:44444"
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("LAN request = %d, want 200", w.Code)
	}
}

func TestBearerKeyAuth(t *testing.T) {
	s := testServer(t, `{"gateway":{"trust_local_networks":true}}`, nil)
	plain, _, err := s.store.CreateKey("bob", "main", "user", false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+plain)
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("valid key = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	// Legacy key works too
	legacyDir := t.TempDir()
	legacyFile := legacyDir + "/api-keys.list"
	os.WriteFile(legacyFile, []byte("sk-legacy-op\n"), 0o600)
	s.store.LegacyKeysFile = legacyFile
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer sk-legacy-op")
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("legacy key = %d, want 200", w.Code)
	}
}

func TestPoolFailoverOn503(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/slots") {
			// not ninfer; vllm-less backend => score 0
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "hi"}}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	defer alive.Close()

	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", dead.URL)
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", alive.URL)
	s := testServer(t, conf, nil)

	w := httptest.NewRecorder()
	body := `{"model":"qwen","messages":[{"role":"user","content":"hello"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("failover response = %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp["choices"] == nil {
		t.Fatalf("body not the live backend's: %s", w.Body.String())
	}
	// usage recorded once, attributed to 'local' (LAN-trusted unkeyed request),
	// exact tokens from the live backend's response
	s.usage.Flush()
	data, _ := os.ReadFile(s.usage.path)
	var uf UsageFile
	json.Unmarshal(data, &uf)
	u := uf.Users["local"]
	if u == nil || u.Requests != 1 || u.PromptTokens != 5 || u.OutputTokens != 2 {
		t.Fatalf("usage = %+v", uf.Users)
	}
}

func TestSessionPinningSameMember(t *testing.T) {
	var hitsA, hitsB int
	var mu = make(chan int, 1)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if strings.HasSuffix("/slots", "/slots") {
		}
		mu <- 1
		hitsA++
		<-mu
		w.WriteHeader(404)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu <- 1
		hitsB++
		<-mu
		w.WriteHeader(404)
	}))
	defer b.Close()
	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", a.URL)
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", b.URL)
	s := testServer(t, conf, nil)

	sendWithSession := func(sess string) {
		w := httptest.NewRecorder()
		body := `{"model":"qwen","messages":[{"role":"user","content":"x"}]}`
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("X-Session-Id", sess)
		r.RemoteAddr = "192.168.1.50:5556"
		s.Handler().ServeHTTP(w, r)
	}
	sendWithSession("conv-42")
	sendWithSession("conv-42")
	sendWithSession("conv-42")
	// Both requests must hit the SAME backend (whichever the pin chose).
	if !(hitsA == 3 && hitsB == 0) && !(hitsA == 0 && hitsB == 3) {
		t.Fatalf("pinning failed: hitsA=%d hitsB=%d", hitsA, hitsB)
	}
}

func TestGlobalRateLimit(t *testing.T) {
	s := testServer(t, `{}`, nil)
	// /v1/models from loopback (tunnel) with bad keys: 300/min cap.
	blocked := false
	for i := 0; i < 305; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.RemoteAddr = "127.0.0.1:1000"             // tunnel path (SPEC: peer 127.0.0.1)
		r.Header.Set("CF-Connecting-IP", "9.9.9.9") // same tunnel client
		s.Handler().ServeHTTP(w, r)
		if w.Code == 429 {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("expected 429 after 300 requests from one tunnel IP")
	}
	// Different CF IP still allowed
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1000"
	r.Header.Set("CF-Connecting-IP", "8.8.8.8")
	s.Handler().ServeHTTP(w, r)
	if w.Code == 429 {
		t.Fatal("different tunnel IP should not be blocked")
	}
}

func TestUnknownV1ProbeThrottled(t *testing.T) {
	s := testServer(t, `{"gateway":{"trust_local_networks":true}}`, nil)
	var code int
	for i := 0; i < 61; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/frobnicate", nil)
		r.Header.Set("CF-Connecting-IP", "7.7.7.7")
		s.Handler().ServeHTTP(w, r)
		code = w.Code
	}
	if code != 429 {
		t.Fatalf("probe #61 = %d, want 429", code)
	}
}

func TestModelsCatalogPoolsCollapseAndFilter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "qwen"}, {"id": "qwen-large"}, {"id": "Qwen3-Embedding-0.6B"},
		}})
	}))
	defer up.Close()
	// No model_pools: only the member-hiding rule can't apply; everything
	// discovered is advertised (public_models is accepted but not applied).
	conf := `{"gateway":{"trust_local_networks":true},"local_networks":["192.168.1.0/24"],"discovery":{"local_ports":[` +
		strings.TrimPrefix(up.URL, "http://127.0.0.1:") + `]},"public_models":["qwen"]}`
	s := testServer(t, conf, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.RemoteAddr = "192.168.1.50:5557"
	s.Handler().ServeHTTP(w, r)
	var resp struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	got := map[string]bool{}
	for _, m := range resp.Data {
		got[m.ID] = true
	}
	for _, want := range []string{"qwen", "qwen-large", "Qwen3-Embedding-0.6B"} {
		if !got[want] {
			t.Fatalf("catalog missing %q: %+v", want, resp.Data)
		}
	}
	if len(resp.Data) != 3 {
		t.Fatalf("catalog = %+v, want exactly the 3 discovered models", resp.Data)
	}
}

func TestModelsCatalogHidesPoolMembersSynthesizesVirtual(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "qwen3.8-27b-5090"}, {"id": "Qwen3-Embedding-0.6B"}, {"id": "qwen3.5-9b-fim"},
		}})
	}))
	defer up.Close()
	conf := `{"gateway":{"trust_local_networks":true},"local_networks":["192.168.1.0/24"],"discovery":{"local_ports":[` +
		strings.TrimPrefix(up.URL, "http://127.0.0.1:") + `]},"model_pools":{"qwen3.8-27b":{"members":[{"model_id":"qwen3.8-27b-5090","backend":"http://127.0.0.1:1"}]}}}`
	s := testServer(t, conf, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.RemoteAddr = "192.168.1.50:5557"
	s.Handler().ServeHTTP(w, r)
	var resp struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	got := map[string]bool{}
	for _, m := range resp.Data {
		got[m.ID] = true
	}
	if got["qwen3.8-27b-5090"] {
		t.Fatalf("pool member id leaked into catalog: %+v", resp.Data)
	}
	for _, want := range []string{"qwen3.8-27b", "Qwen3-Embedding-0.6B", "qwen3.5-9b-fim"} {
		if !got[want] {
			t.Fatalf("catalog missing %q: %+v", want, resp.Data)
		}
	}
}

func TestChatConfigModelsAreNames(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "qwen3.8-27b-5090"}, {"id": "Qwen3-Embedding-0.6B"},
		}})
	}))
	defer up.Close()
	conf := `{"gateway":{"trust_local_networks":true},"local_networks":["192.168.1.0/24"],"discovery":{"local_ports":[` +
		strings.TrimPrefix(up.URL, "http://127.0.0.1:") + `]}}`
	s := testServer(t, conf, nil)
	tok := s.store.SignSession(auth.Claims{U: "bob", Role: "user"}, time.Hour)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/chat/config", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	s.Handler().ServeHTTP(w, r)
	var out struct {
		Models []string `json:"models"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || len(out.Models) != 2 || out.Models[0] != "Qwen3-Embedding-0.6B" {
		t.Fatalf("chat/config models = %v (code %d, body %s)", out.Models, w.Code, w.Body.String())
	}
}

func TestUsageKindSplit(t *testing.T) {
	dir := t.TempDir()
	u := NewUsageStore(filepath.Join(dir, "usage.json"))
	u.Record("bob", "main", "qwen3.8-27b", 10, 5)
	u.Record("bob", "main", "text-embed", 3, 0)
	u.Record("bob", "", "fim-model", 7, 9)
	u.Flush()
	data, _ := os.ReadFile(filepath.Join(dir, "usage.json"))
	var uf UsageFile
	json.Unmarshal(data, &uf)
	bob := uf.Users["bob"]
	if bob.Kinds["chat"].PromptTokens != 10 || bob.Kinds["embeddings"].PromptTokens != 3 ||
		bob.Kinds["fim"].OutputTokens != 9 {
		t.Fatalf("kinds = %+v", bob.Kinds)
	}
	if bob.Keys["main"] == nil || bob.Keys["main"].Requests != 2 {
		t.Fatalf("keys = %+v", bob.Keys)
	}
}

var _ = time.Now
