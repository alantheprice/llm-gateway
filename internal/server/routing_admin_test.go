package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/routing"
)

const routingConf = `{"gateway":{"port":0},
 "model_pools":{"shared":{"overflow_threshold":0.9,"members":[
   {"model_id":"a-model","backend":"http://10.9.0.1:8000"},
   {"model_id":"wrong-id","backend":"http://10.9.0.2:8000"}]}}}`

func routingServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t, routingConf, nil)
	s.mu.Lock()
	s.backends["http://10.9.0.1:8000"] = &BackendInfo{Models: []string{"a-model"}, Chat: true}
	s.backends["http://10.9.0.2:8000"] = &BackendInfo{Models: []string{"b-model"}, Chat: true}
	s.mu.Unlock()
	now := time.Now()
	s.tracker.Set("http://10.9.0.1:8000", &routing.Load{Engine: "ninfer", Running: 5, Lanes: 6, LastUpdated: now})
	s.tracker.Set("http://10.9.0.2:8000", &routing.Load{Engine: "ninfer", Running: 0, Lanes: 6, LastUpdated: now})
	return s
}

func adminDo(t *testing.T, s *Server, role, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	u := "root"
	if role != "admin" {
		u = "bob"
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: u, Role: role}, time.Hour)})
	s.Handler().ServeHTTP(w, r)
	return w
}

// The route map lists names with the right kind (a pool member's own id is
// "one GPU, called directly"), and flags a GPU listed with a model id its
// engine doesn't serve.
func TestRouteMap(t *testing.T) {
	s := routingServer(t)
	w := adminDo(t, s, "admin", "GET", "/api/routing", "")
	if w.Code != 200 {
		t.Fatalf("GET /api/routing: %d %s", w.Code, w.Body)
	}
	var m struct {
		Entries  []routeEntry   `json:"entries"`
		Problems []routeProblem `json:"problems"`
		Engines  []engineView   `json:"engines"`
	}
	json.Unmarshal(w.Body.Bytes(), &m)
	kinds := map[string]string{}
	for _, e := range m.Entries {
		kinds[e.Name] = e.Kind
	}
	if kinds["shared"] != "pool" || kinds["a-model"] != "gpu-direct" || kinds["b-model"] != "direct" {
		t.Fatalf("kinds = %v", kinds)
	}
	found := false
	for _, p := range m.Problems {
		if p.Severity == "error" && strings.Contains(p.Text, `"wrong-id"`) && strings.Contains(p.Text, "b-model") {
			found = true
		}
	}
	if !found {
		t.Fatalf("wrong model id not flagged: %+v", m.Problems)
	}
	if len(m.Engines) != 2 {
		t.Fatalf("engines = %+v", m.Engines)
	}
	if w := adminDo(t, s, "user", "GET", "/api/routing", ""); w.Code != 403 {
		t.Fatalf("non-admin got %d", w.Code)
	}
}

// The dry run follows handleChat's order: pool pick by load, a single
// engine by its own id, 404 for unknown names.
func TestRouteDryRun(t *testing.T) {
	s := routingServer(t)
	run := func(body string) map[string]any {
		w := adminDo(t, s, "admin", "POST", "/api/routing/test", body)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 {
			t.Fatalf("test %s: %d %s", body, w.Code, w.Body)
		}
		return out
	}
	if out := run(`{"model":"shared","prompt_tokens":1000}`); out["result"] != "routed" || out["pick_backend"] != "http://10.9.0.2:8000" {
		t.Fatalf("pool pick = %v (want the idle GPU)", out)
	}
	if out := run(`{"model":"a-model"}`); out["pick_backend"] != "http://10.9.0.1:8000" {
		t.Fatalf("direct = %v", out)
	}
	if out := run(`{"model":"nope"}`); out["result"] != "404" {
		t.Fatalf("unknown = %v", out)
	}
	if w := adminDo(t, s, "user", "POST", "/api/routing/test", `{"model":"shared"}`); w.Code != 403 {
		t.Fatalf("non-admin dry run got %d", w.Code)
	}
}

// Saving config keeps live load state (the tracker is not rebuilt).
func TestConfigSaveKeepsLoadState(t *testing.T) {
	s := routingServer(t)
	s.SetConfig(s.cfg)
	if l := s.tracker.Get("http://10.9.0.1:8000"); l == nil || l.Running != 5 {
		t.Fatalf("load lost across SetConfig: %+v", l)
	}
}

// The page renders for admins and redirects everyone else.
func TestRoutingPage(t *testing.T) {
	if err := InitUI(); err != nil {
		t.Fatal(err)
	}
	s := routingServer(t)
	if w := adminDo(t, s, "admin", "GET", "/admin/routing", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `id="routes"`) {
		t.Fatalf("admin page: %d", w.Code)
	}
	if w := adminDo(t, s, "user", "GET", "/admin/routing", ""); w.Code != http.StatusFound {
		t.Fatalf("non-admin page: %d", w.Code)
	}
}

// engineEcho answers chat completions (non-streamed) with its model id,
// optionally holding each request until release is closed.
func engineEcho(t *testing.T, id string, hold chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, id)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if hold != nil {
				select {
				case <-hold:
				case <-time.After(5 * time.Second):
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"x","model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, id)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An alias routes exactly like its pool, and the response names the alias.
func TestPoolAlias(t *testing.T) {
	a, b := engineEcho(t, "m-a", nil), engineEcho(t, "m-b", nil)
	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", a.URL)
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", b.URL)
	conf = strings.Replace(conf, `"overflow_threshold": 0.2,`, `"overflow_threshold": 0.2, "aliases": ["old-name"],`, 1)
	s := testServer(t, conf, nil)
	if name, _, ok := s.poolFor("old-name"); !ok || name != "qwen" {
		t.Fatalf("poolFor(alias) = %q %v", name, ok)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"old-name","messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out["model"] != "old-name" {
		t.Fatalf("alias chat: %d %v", w.Code, out)
	}
	if d := s.dryRun("old-name", 100, 10); d["result"] != "routed" {
		t.Fatalf("alias dry run = %v", d)
	}
	// An alias can't claim another pool's name.
	s2, _ := adminConfServer(t, `{"gateway":{"port":8033}}`)
	if w := adminSessionRequest(s2, "POST", "/admin/config", `{"model_pools":{
	  "p1":{"members":[{"model_id":"m","backend":"http://127.0.0.1:9001"}]},
	  "p2":{"aliases":["p1"],"members":[{"model_id":"m","backend":"http://127.0.0.1:9002"}]}}}`); w.Code != 400 {
		t.Fatalf("conflicting alias accepted: %d %s", w.Code, w.Body)
	}
}

// Requests count as in flight from the moment they're sent — including
// non-streamed ones and ones that don't go through a pool.
func TestInFlightCountedFromSend(t *testing.T) {
	hold := make(chan struct{})
	eng := engineEcho(t, "solo", hold)
	s := testServer(t, `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"]}`, nil)
	s.mu.Lock()
	s.backends[eng.URL] = &BackendInfo{Models: []string{"solo"}, Chat: true}
	s.mu.Unlock()
	done := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"solo","messages":[{"role":"user","content":"hi"}]}`))
		r.RemoteAddr = "192.168.1.50:5555"
		s.Handler().ServeHTTP(w, r)
		done <- w.Code
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.tracker.InFlight(eng.URL) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := s.tracker.InFlight(eng.URL); n != 1 {
		t.Fatalf("in flight while the engine works = %d, want 1", n)
	}
	close(hold)
	if code := <-done; code != 200 {
		t.Fatalf("request: %d", code)
	}
	if n := s.tracker.InFlight(eng.URL); n != 0 {
		t.Fatalf("in flight after completion = %d, want 0", n)
	}
}

// The docs-only operations still publish their response schemas.
func TestOpenAPIDocsOnlySchemas(t *testing.T) {
	s := routingServer(t)
	w := adminDo(t, s, "admin", "GET", "/openapi.json", "")
	if w.Code != 200 {
		t.Fatalf("openapi: %d", w.Code)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]any `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	json.Unmarshal(w.Body.Bytes(), &spec)
	for _, p := range []string{"/v1/models", "/chat/config", "/keys", "/admin/users"} {
		get, ok := spec.Paths[p]["get"]
		if !ok || len(get.Responses["200"].Content) == 0 {
			t.Errorf("%s: missing GET or 200 schema", p)
		}
	}
}

// Embeddings and completions reach engines through a shared model too.
func TestPoolServesEmbeddingsAndCompletions(t *testing.T) {
	hits := map[string]int{}
	var mu sync.Mutex
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/embeddings":
			fmt.Fprint(w, `{"object":"list","model":"emb-engine","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":2}}`)
		case "/v1/completions":
			fmt.Fprint(w, `{"model":"fim-engine","choices":[{"text":"x"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer eng.Close()
	conf := `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"embed-shared":{"members":[{"model_id":"emb-engine","backend":"` + eng.URL + `"}]},
	                "fim-shared":{"members":[{"model_id":"fim-engine","backend":"` + eng.URL + `"}]}}}`
	s := testServer(t, conf, nil)
	for path, model := range map[string]string{"/v1/embeddings": "embed-shared", "/v1/completions": "fim-shared"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"`+model+`","input":"hi","prompt":"hi"}`))
		r.RemoteAddr = "192.168.1.50:5555"
		s.Handler().ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 || out["model"] != model {
			t.Fatalf("%s via pool: %d %s", path, w.Code, w.Body)
		}
	}
	if hits["/v1/embeddings"] != 1 || hits["/v1/completions"] != 1 {
		t.Fatalf("engine hits = %v", hits)
	}
}

// Discovery picks up new engines, keeps one through a couple of failed
// probes, drops it after discoveryMaxMisses, and drops unscanned ones at once.
func TestDiscoveryRefresh(t *testing.T) {
	up := true
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			w.WriteHeader(500)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
	}))
	defer eng.Close()
	port := strings.TrimPrefix(eng.URL, "http://127.0.0.1:")
	s := testServer(t, `{"gateway":{"port":0},"discovery":{"local_ports":[`+port+`]}}`, nil)
	s.Discover()
	if u, _ := s.resolve("m1"); u != eng.URL {
		t.Fatalf("not discovered: %q", u)
	}
	up = false
	for i := 1; i < discoveryMaxMisses; i++ {
		s.Discover()
		if u, _ := s.resolve("m1"); u == "" {
			t.Fatalf("dropped after %d missed probe(s)", i)
		}
	}
	s.Discover()
	if u, _ := s.resolve("m1"); u != "" {
		t.Fatal("still listed after repeated failures")
	}
	// Back up, then removed from the scan list: gone on the next pass.
	up = true
	s.Discover()
	s.mu.Lock()
	s.cfg.Discovery.LocalPorts = nil
	s.mu.Unlock()
	s.Discover()
	if u, _ := s.resolve("m1"); u != "" {
		t.Fatal("unscanned engine still listed")
	}
}

// llama.cpp-style engines (array /slots + llamacpp: metrics) are polled and
// scored by their real lanes; vLLM reports its configured concurrency.
func TestPollLlamaCppAndVLLMLanes(t *testing.T) {
	llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slots":
			fmt.Fprint(w, `[{"id":0,"is_processing":true},{"id":1,"is_processing":false},{"id":2,"is_processing":false}]`)
		case "/metrics":
			fmt.Fprint(w, "llamacpp:requests_processing 1\nllamacpp:requests_deferred 2\n")
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"fim"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer llama.Close()
	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprint(w, "vllm:num_requests_running 1\nvllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0.1\n")
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"emb"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer vllm.Close()
	s := testServer(t, `{"gateway":{"port":0},"metrics":{"default_max_seqs":4}}`, nil)
	s.mu.Lock()
	s.backends[llama.URL] = &BackendInfo{Models: []string{"fim"}, Chat: true}
	s.backends[vllm.URL] = &BackendInfo{Models: []string{"emb"}, Embeds: true}
	s.mu.Unlock()
	s.PollOnce()
	l := s.tracker.Get(llama.URL)
	if l == nil || l.Engine != "llamacpp" || l.Lanes != 3 || l.Running != 1 || l.Waiting != 2 {
		t.Fatalf("llama.cpp load = %+v", l)
	}
	if sc := s.tracker.Score(llama.URL); sc < 0.75 {
		t.Fatalf("queued llama.cpp engine scored %.2f, want saturated", sc)
	}
	if v := s.tracker.Get(vllm.URL); v == nil || v.Lanes != 4 {
		t.Fatalf("vLLM lanes = %+v, want 4 from default_max_seqs", v)
	}
}
