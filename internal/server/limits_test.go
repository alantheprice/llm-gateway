package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// limitTestServer: server + a bob key, no backends (resolve 404s after the
// quota gate — the gate must fire first).
func limitTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := testServer(t, `{"gateway":{"trust_local_networks":true}}`, nil)
	plain, _, err := s.store.CreateKey("bob", "main", "user", false)
	if err != nil {
		t.Fatal(err)
	}
	return s, plain
}

func TestDailyLimit429(t *testing.T) {
	s, key := limitTestServer(t)
	if err := s.store.SetDailyLimit("bob", 100); err != nil {
		t.Fatal(err)
	}
	// Simulate 90 tokens used today.
	s.usage.Record("bob", "main", "qwen3.8-27b", 60, 30)

	body := `{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound { // under limit: passes the gate, then model resolution 404s
		t.Fatalf("under limit: got %d, want 404 (model not found)", w.Code)
	}

	// Push over the limit (180 > 100).
	s.usage.Record("bob", "main", "qwen3.8-27b", 60, 30)
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+key)
	s.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("over limit: got %d, want 429 (%s)", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	var resp struct {
		Error struct {
			Limit int `json:"limit"`
			Used  int `json:"used"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Limit != 100 || resp.Error.Used != 180 {
		t.Fatalf("error body limit/used = %d/%d, want 100/180", resp.Error.Limit, resp.Error.Used)
	}
}

func TestDailyLimitExemptions(t *testing.T) {
	s, key := limitTestServer(t)
	if err := s.store.SetDailyLimit("bob", 1); err != nil {
		t.Fatal(err)
	}
	s.usage.Record("bob", "main", "qwen3.8-27b", 5000, 5000) // way over

	// Admin ui key is exempt.
	adminKey, _, err := s.store.CreateUIKey("admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	s.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatal("admin must be exempt from daily limits")
	}

	// Legacy operator key is exempt.
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-op-legacy")
	s.store.LegacyKeysFile = filepath.Join(t.TempDir(), "api-keys.list")
	if err := os.WriteFile(s.store.LegacyKeysFile, []byte("sk-op-legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatal("operator must be exempt from daily limits")
	}

	// Unlimiting (0) clears the quota.
	if err := s.store.SetDailyLimit("bob", 0); err != nil {
		t.Fatal(err)
	}
	if got := s.store.DailyLimit("bob"); got != 0 {
		t.Fatalf("daily limit = %d, want 0", got)
	}
	_ = key
}

func TestComputePricingSplitsByGPUTime(t *testing.T) {
	// Equal token counts: prefill 1M/4000 = 250s of GPU time, decode
	// 1M/500 = 2000s → tg carries 2000/2250 = 88.9% of cost.
	p := ComputePricing(100, 4000, 500, 1_000_000, 1_000_000, 0)
	if p.CostPPShare+p.CostTGShare < 99.99 || p.CostPPShare+p.CostTGShare > 100.01 {
		t.Fatalf("shares must sum to total: %v + %v", p.CostPPShare, p.CostTGShare)
	}
	if p.CostTGShare < 88.8 || p.CostTGShare > 89.0 {
		t.Fatalf("tg share = %v, want 88.9", p.CostTGShare)
	}
	// pp: 11.11$/1M tok, tg: 88.89$/1M tok (rounded to cents).
	if p.PromptPerM != 11.11 || p.OutputPerM != 88.89 {
		t.Fatalf("pricing pp=%v tg=%v, want 11.11/88.89", p.PromptPerM, p.OutputPerM)
	}
	// Margin multiplies both (x1.5). Half-cent rounding drift is expected.
	m := ComputePricing(100, 4000, 500, 1_000_000, 1_000_000, 50)
	if m.PromptPerM < 16.66 || m.PromptPerM > 16.68 || m.OutputPerM < 133.32 || m.OutputPerM > 133.35 {
		t.Fatalf("margin pricing pp=%v tg=%v, want ~16.67/~133.33", m.PromptPerM, m.OutputPerM)
	}
	// Zero token guard.
	z := ComputePricing(100, 4000, 500, 0, 0, 0)
	if z.PromptPerM != 0 || z.OutputPerM != 0 {
		t.Fatal("no tokens → no price")
	}
}
