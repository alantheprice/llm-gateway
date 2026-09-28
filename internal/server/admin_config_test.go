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

// adminConfServer builds a server whose conf lives in a temp file so the
// admin config save path can be exercised end-to-end (persist + reload).
func adminConfServer(t *testing.T, confJSON string) (*Server, string) {
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
	usersPath := filepath.Join(dir, "users.json")
	users, _ := auth.Open(usersPath)
	s := New(cfg, users)
	t.Cleanup(s.CloseAnalytics)
	s.store.LegacyKeysFile = cfg.Gateway.APIKeysFile
	s.ParseNetworks()
	return s, confPath
}

func adminSessionRequest(s *Server, method, path string, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	tok := s.store.SignSession(adminClaims(), time.Hour)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	s.Handler().ServeHTTP(w, r)
	return w
}

func adminClaims() (c auth.Claims) { c.U = "root"; c.Role = "admin"; return }

func TestAdminConfigGetRoundTrip(t *testing.T) {
	s, _ := adminConfServer(t, `{"gateway":{"port":8033,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],"cache":{"ttl":60}}`)
	w := adminSessionRequest(s, "GET", "/admin/config", "")
	if w.Code != 200 {
		t.Fatalf("GET /admin/config = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Config   map[string]any `json:"config"`
		ConfPath string         `json:"confPath"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.ConfPath == "" || out.Config["cache"].(map[string]any)["ttl"].(float64) != 60 {
		t.Fatalf("unexpected payload: %s", w.Body.String())
	}
}

func TestAdminConfigPostSavesPersistsAndApplies(t *testing.T) {
	s, confPath := adminConfServer(t, `{"gateway":{"port":8033},"cache":{"ttl":60},"metrics":{"stale_threshold":30}}`)
	body := `{"gateway":{"port":8033,"models_require_auth":true},"cache":{"ttl":120},"metrics":{"stale_threshold":45},"local_networks":["10.0.0.0/8"]}`
	w := adminSessionRequest(s, "POST", "/admin/config", body)
	if w.Code != 200 {
		t.Fatalf("POST /admin/config = %d: %s", w.Code, w.Body.String())
	}
	// Applied live.
	if !s.cfg.Gateway.ModelsRequireAuth || s.cfg.Cache.TTL != 120 || s.cfg.Metrics.StaleThreshold != 45 {
		t.Fatalf("config not applied live: %+v", s.cfg)
	}
	// Persisted to disk.
	disk, err := config.Load(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Cache.TTL != 120 || !disk.Gateway.ModelsRequireAuth || disk.Metrics.StaleThreshold != 45 {
		t.Fatalf("config not persisted: %+v", disk)
	}
	// Overlay semantics: fields not in the POST keep their values (port came
	// through, discovery defaults were not wiped).
	if s.cfg.Gateway.Port != 8033 {
		t.Fatalf("overlay clobbered unposted fields: port=%d", s.cfg.Gateway.Port)
	}
}

func TestAdminConfigPostValidation(t *testing.T) {
	s, _ := adminConfServer(t, `{"gateway":{"port":8033}}`)
	// Bad CIDR rejected.
	w := adminSessionRequest(s, "POST", "/admin/config", `{"local_networks":["not-a-cidr"]}`)
	if w.Code != 400 {
		t.Fatalf("bad CIDR = %d, want 400: %s", w.Code, w.Body.String())
	}
	// Bad pool member URL rejected.
	w = adminSessionRequest(s, "POST", "/admin/config", `{"model_pools":{"x":{"members":[{"model_id":"m","backend":"ftp://nope"}]}}}`)
	if w.Code != 400 {
		t.Fatalf("bad backend URL = %d, want 400: %s", w.Code, w.Body.String())
	}
	// Identity-plane paths protected: a POST cannot repoint users_file.
	w = adminSessionRequest(s, "POST", "/admin/config", `{"gateway":{"port":8033,"users_file":"/tmp/evil.json"}}`)
	if w.Code != 200 {
		t.Fatalf("users_file POST = %d: %s", w.Code, w.Body.String())
	}
	if s.cfg.Gateway.UsersFile == "/tmp/evil.json" {
		t.Fatal("users_file was overwritten via admin config POST")
	}
	// Non-admin rejected.
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/admin/config", strings.NewReader(`{"cache":{"ttl":1}}`))
	tok := s.store.SignSession(auth.Claims{U: "bob", Role: "user"}, time.Hour)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	s.Handler().ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("user session could save admin config")
	}
}

func TestAdminConfigPageRedirectsNonAdmin(t *testing.T) {
	s, _ := adminConfServer(t, `{}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/admin/config/page", nil)
	tok := s.store.SignSession(auth.Claims{U: "bob", Role: "user"}, time.Hour)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("non-admin page = %d, want redirect", w.Code)
	}
}

// A host reached only by link is valid, keeps its links through
// validation, and may not claim a link another host already claims.
func TestValidateConfigLinkOnlyHost(t *testing.T) {
	c := &config.Config{Gateway: config.GatewayCfg{Port: 8033}, Hosts: []config.HostCfg{
		{Label: "offsite", Links: []string{"gpu-b"}, OverheadWatts: 100},
	}}
	if err := validateConfig(c); err != nil {
		t.Fatalf("link-only host rejected: %v", err)
	}
	if len(c.Hosts) != 1 || len(c.Hosts[0].Links) != 1 {
		t.Fatalf("links lost in validation: %+v", c.Hosts)
	}
	dup := &config.Config{Gateway: config.GatewayCfg{Port: 8033}, Hosts: []config.HostCfg{
		{Label: "a", Links: []string{"gpu-b"}},
		{Label: "b", Links: []string{"GPU-B"}},
	}}
	if err := validateConfig(dup); err == nil {
		t.Fatal("two hosts claiming the same link should be rejected")
	}
}

// Removing a pool or overflow pair in the form removes it: posted sections
// replace the maps instead of merging into them.
func TestAdminConfigPostRemovesPoolsAndPairs(t *testing.T) {
	s, _ := adminConfServer(t, `{"gateway":{"port":8033},
	 "model_pools":{"a":{"members":[{"model_id":"m","backend":"http://127.0.0.1:9001"}]},
	                "b":{"members":[{"model_id":"m","backend":"http://127.0.0.1:9002"}]}},
	 "overflow_pairs":{"x":{"fallback_model_id":"m","fallback_backend":"http://127.0.0.1:9003","overflow_threshold":0.5}}}`)
	w := adminSessionRequest(s, "POST", "/admin/config",
		`{"model_pools":{"a":{"members":[{"model_id":"m","backend":"http://127.0.0.1:9001"}]}},"overflow_pairs":{}}`)
	if w.Code != 200 {
		t.Fatalf("POST = %d: %s", w.Code, w.Body)
	}
	if _, ok := s.cfg.ModelPools["b"]; ok || len(s.cfg.ModelPools) != 1 || len(s.cfg.OverflowPairs) != 0 {
		t.Fatalf("pools=%v pairs=%v; want only pool a, no pairs", s.cfg.ModelPools, s.cfg.OverflowPairs)
	}
	// A post without those sections leaves them alone.
	adminSessionRequest(s, "POST", "/admin/config", `{"cache":{"ttl":90}}`)
	if len(s.cfg.ModelPools) != 1 {
		t.Fatalf("unposted pools changed: %v", s.cfg.ModelPools)
	}
}
