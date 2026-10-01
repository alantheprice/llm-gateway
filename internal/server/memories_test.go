package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

// ---------- pure unit tests ----------

func TestCleanMemoryText(t *testing.T) {
	got, err := cleanMemoryText("  I prefer dark mode. \n")
	if err != nil || got != "I prefer dark mode." {
		t.Fatalf("clean = %q, %v", got, err)
	}
	if _, err := cleanMemoryText("   \n  "); err == nil {
		t.Fatalf("empty memory should error")
	}
	if _, err := cleanMemoryText(strings.Repeat("x", maxMemoryChars+1)); err == nil {
		t.Fatalf("over-long memory should error")
	}
	// The cap is in RUNES, not bytes: 2000 CJK characters (6000 bytes)
	// must pass, 2001 must not.
	cjk := strings.Repeat("日", maxMemoryChars)
	if _, err := cleanMemoryText(cjk); err != nil {
		t.Fatalf("%d CJK runes should pass (got %v)", maxMemoryChars, err)
	}
	if _, err := cleanMemoryText(cjk + "日"); err == nil {
		t.Fatalf("2001 CJK runes should error")
	}
	if !memoryIDRE.MatchString("m" + strings.Repeat("a", 16)) {
		t.Fatalf("generated id shape rejected by id regex")
	}
}

// ---------- integration: save, search, per-user isolation ----------

func memSave(t *testing.T, h http.Handler, key, text string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/memories/save", strings.NewReader(fmt.Sprintf(`{"text":%q}`, text)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func memSearch(t *testing.T, h http.Handler, key, query string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/memories/search", strings.NewReader(fmt.Sprintf(`{"query":%q,"top_k":5}`, query)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func memAPI(t *testing.T, s *Server, h http.Handler, user, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: user, Role: "user"}, time.Hour)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func TestMemorySaveSearchAndIsolation(t *testing.T) {
	app, _, _ := testPB(t)
	vocab := []string{"coffee", "espresso", "dark", "mode", "deploy", "kubernetes", "cluster", "rollout"}
	emb := newFakeEmbedding(t, vocab)

	s := testServer(t, ragConf(emb.URL), nil)
	s.backends[emb.URL] = &BackendInfo{Models: []string{"test-embed"}, Embeds: true, Chat: false}
	s.SetOps(app)
	h := s.Handler()

	danaKey, _, _ := s.store.CreateKey("dana", "dkey", "user", false)
	erinKey, _, _ := s.store.CreateKey("erin", "ekey", "user", false)

	// Both users save memories.
	if c, m := memSave(t, h, danaKey, "The user drinks coffee; prefers a dark roast."); c != 200 {
		t.Fatalf("dana save = %d %v", c, m)
	}
	if c, m := memSave(t, h, danaKey, "Deployment runs on a kubernetes cluster."); c != 200 {
		t.Fatalf("dana save 2 = %d %v", c, m)
	}
	if c, m := memSave(t, h, erinKey, "The user works the espresso machine at the shop."); c != 200 {
		t.Fatalf("erin save = %d %v", c, m)
	}

	// Dana's search ranks her own memories and never returns erin's.
	c, m := memSearch(t, h, danaKey, "coffee dark roast")
	if c != 200 {
		t.Fatalf("dana search = %d %v", c, m)
	}
	results := m["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("dana: no results: %v", m)
	}
	top := results[0].(map[string]any)
	if !strings.Contains(strings.ToLower(top["text"].(string)), "coffee") {
		t.Fatalf("top result not dana's coffee memory: %v", top)
	}
	for _, rr := range results {
		if strings.Contains(strings.ToLower(rr.(map[string]any)["text"].(string)), "espresso") {
			t.Fatalf("dana's search leaked erin's memory: %v", rr)
		}
	}

	// Erin's search finds her own.
	c, m = memSearch(t, h, erinKey, "espresso machine")
	if c != 200 {
		t.Fatalf("erin search = %d %v", c, m)
	}
	results = m["results"].([]any)
	if len(results) == 0 || !strings.Contains(strings.ToLower(results[0].(map[string]any)["text"].(string)), "espresso") {
		t.Fatalf("erin: top not her memory: %v", m)
	}

	// Session CRUD: dana lists her two, deletes one, it stops matching.
	c, m = memAPI(t, s, h, "dana", "GET", "/api/memories", "")
	if c != 200 {
		t.Fatalf("list = %d %v", c, m)
	}
	mems := m["memories"].([]any)
	if len(mems) != 2 {
		t.Fatalf("dana list = %v, want 2", m["memories"])
	}
	// Delete the coffee one by content (updated timestamps can tie).
	var id string
	for _, mm := range mems {
		if strings.Contains(strings.ToLower(mm.(map[string]any)["text"].(string)), "coffee") {
			id = mm.(map[string]any)["id"].(string)
		}
	}
	if id == "" {
		t.Fatalf("no coffee memory in list: %v", mems)
	}
	c, m = memAPI(t, s, h, "dana", "DELETE", "/api/memories/"+id, "")
	if c != 200 {
		t.Fatalf("delete = %d %v", c, m)
	}
	c, m = memSearch(t, h, danaKey, "coffee dark roast")
	if c != 200 {
		t.Fatalf("search after delete = %d %v", c, m)
	}
	for _, rr := range m["results"].([]any) {
		if strings.Contains(strings.ToLower(rr.(map[string]any)["text"].(string)), "coffee") {
			t.Fatalf("deleted memory still searchable: %v", rr)
		}
	}

	// Empty body / empty query are rejected.
	if c, _ := memSave(t, h, danaKey, "   "); c != 400 {
		t.Fatalf("blank save = %d, want 400", c)
	}
	if c, _ := memSearch(t, h, danaKey, "  "); c != 400 {
		t.Fatalf("blank search = %d, want 400", c)
	}
	// No auth: the key-auth endpoints refuse.
	r := httptest.NewRequest("POST", "/v1/memories/search", strings.NewReader(`{"query":"coffee"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauthenticated search = %d, want 401", w.Code)
	}
}

// TestMemoryCap: the per-user 500 cap is enforced on the write path (413).
func TestMemoryCap(t *testing.T) {
	app, _, _ := testPB(t)
	emb := newFakeEmbedding(t, []string{"filler"})

	s := testServer(t, ragConf(emb.URL), nil)
	s.backends[emb.URL] = &BackendInfo{Models: []string{"test-embed"}, Embeds: true, Chat: false}
	s.SetOps(app)
	h := s.Handler()

	// Seed to the cap directly (cheap: no embed round-trips per row). The
	// shared test DB may already hold rows for "dana" (other tests use the
	// same user), so top up rather than assume a clean slate.
	base, err := s.memories.MemoryCount("dana")
	if err != nil {
		t.Fatalf("count before seed: %v", err)
	}
	emb4 := []byte{0x00, 0x00, 0x80, 0x3f} // a valid 1-dim float32 blob
	for i := 0; i < maxMemoriesPerUser-base; i++ {
		if err := s.memories.AddMemory("dana", fmt.Sprintf("mcap%03d", i), fmt.Sprintf("fact %d", i), emb4, 1, 0, 0); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	if n, err := s.memories.MemoryCount("dana"); err != nil || n != maxMemoriesPerUser {
		t.Fatalf("seeded count = %d, %v; want %d", n, err, maxMemoriesPerUser)
	}
	// At the cap, both write paths refuse with 413.
	if c, m := memAPI(t, s, h, "dana", "POST", "/api/memories", `{"text":"one more"}`); c != 413 {
		t.Fatalf("session save at cap = %d %v, want 413", c, m)
	}
	key, _, _ := s.store.CreateKey("dana", "dk", "user", false)
	if c, m := memSave(t, h, key, "one more"); c != 413 {
		t.Fatalf("key save at cap = %d %v, want 413", c, m)
	}
}

// TestMemoryCrossUserByID: one user can't read or delete another's memory by
// id (the queries are user-scoped; verify it, not just search isolation).
func TestMemoryCrossUserByID(t *testing.T) {
	app, _, _ := testPB(t)
	emb := newFakeEmbedding(t, []string{"secret"})

	s := testServer(t, ragConf(emb.URL), nil)
	s.backends[emb.URL] = &BackendInfo{Models: []string{"test-embed"}, Embeds: true, Chat: false}
	s.SetOps(app)
	h := s.Handler()

	erinKey, _, _ := s.store.CreateKey("erin", "ekey", "user", false)
	c, m := memSave(t, h, erinKey, "erin's secret recipe for espresso.")
	if c != 200 {
		t.Fatalf("erin save = %d %v", c, m)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("save returned no id: %v", m)
	}
	// Dana attacks that id via her own session (shared test DB: erin may
	// have other memories from earlier tests — only THIS id is in scope).
	if c, m := memAPI(t, s, h, "dana", "GET", "/api/memories/"+id, ""); c != 404 {
		t.Fatalf("dana GET erin's memory = %d %v, want 404", c, m)
	}
	if c, m := memAPI(t, s, h, "dana", "DELETE", "/api/memories/"+id, ""); c != 200 {
		t.Fatalf("dana DELETE erin's memory = %d %v, want no-op 200", c, m)
	}
	if m2, err := s.memories.GetMemory("erin", id); err != nil || m2 == nil {
		t.Fatalf("erin's memory gone after cross-user delete attempt: %v (got %+v)", err, m2)
	}
}

// withMemoryTool mirrors the docs-tool gate: only a key-auth caller with the
// pref on gets the tools; an anonymous caller never does.
func TestWithMemoryToolInjection(t *testing.T) {
	s := testServer(t, `{}`, nil)
	s.store.CreateKey("carol", "k", "user", false)
	_ = s.store.SetPrefs("carol", auth.UserPrefs{MemoryEnabled: true})

	got := s.withMemoryTool([]byte(`{"messages":[],"memory_tool":false}`), "carol", "k1")
	var m map[string]any
	json.Unmarshal(got, &m)
	if m["memory_tool"] != true {
		t.Fatalf("memory_tool = %v, want true", m["memory_tool"])
	}
	got2 := s.withMemoryTool([]byte(`{"messages":[],"memory_tool":true}`), "carol", "")
	var m2 map[string]any
	json.Unmarshal(got2, &m2)
	if m2["memory_tool"] != false {
		t.Fatalf("anonymous memory_tool = %v, want false", m2["memory_tool"])
	}
}
