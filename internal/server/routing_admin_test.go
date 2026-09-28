package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/routing"
)

const routingConf = `{"gateway":{"port":0},
 "model_pools":{"shared":{"overflow_threshold":0.9,"members":[
   {"model_id":"a-model","backend":"http://10.9.0.1:8000"},
   {"model_id":"wrong-id","backend":"http://10.9.0.2:8000"}]}},
 "overflow_pairs":{"a-model":{"fallback_model_id":"b-model","fallback_backend":"http://10.9.0.2:8000","overflow_threshold":0.5}}}`

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

// The route map lists names in resolution order with the right kind, and
// flags a GPU listed with a model id its engine doesn't serve.
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
	if kinds["shared"] != "pool" || kinds["a-model"] != "overflow" || kinds["b-model"] != "direct" {
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

// The dry run follows handleChat's order: pool pick by load, overflow
// spill when the primary is busy, 404 for unknown names.
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
	if out := run(`{"model":"a-model"}`); out["pick_backend"] != "http://10.9.0.2:8000" {
		t.Fatalf("overflow = %v (primary is 5/6 busy, should spill)", out)
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
