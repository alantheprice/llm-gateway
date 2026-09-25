package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/config"
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

func TestApplyBookPrices(t *testing.T) {
	pb := config.PriceBook{PromptUSDPerM: 0.25, CachedUSDPerM: 0.05, OutputUSDPerM: 2.0}
	// 1M computed prompt + 1M cached + 1M output
	v := applyBookPrices(pb, 2_000_000, 1_000_000, 1_000_000)
	want := 0.25*1 + 0.05*1 + 2.0*1 // cached carved out of prompt at the cached rate
	if math.Abs(v-want) > 1e-9 {
		t.Fatalf("value = %v, want %v", v, want)
	}
	// Cached > prompt would be odd but must not go negative.
	v = applyBookPrices(pb, 500_000, 800_000, 0) // computed = -300k clamps? no — operator's problem, but value must not be NaN
	if math.IsNaN(v) || math.IsInf(v, 0) {
		t.Fatal("book prices must not produce NaN/Inf")
	}
}

func TestCostHistoryFreezesTodayOnly(t *testing.T) {
	dir := t.TempDir()
	ch := NewCostHistory(filepath.Join(dir, "cost_history.json"))
	today := time.Now().Format("2006-01-02")
	ch.RecordDay(today, CostDay{EnergyUSD: 1.0, Tokens: 100, ValueUSD: 2.0})
	ch.RecordDay("2020-01-01", CostDay{EnergyUSD: 99}) // past day: rejected
	if got := ch.Days[today]; got.EnergyUSD != 1.0 {
		t.Fatalf("today not recorded: %+v", got)
	}
	if _, ok := ch.Days["2020-01-01"]; ok {
		t.Fatal("past day must be frozen out")
	}
	// Reload from disk.
	ch2 := NewCostHistory(filepath.Join(dir, "cost_history.json"))
	if got := ch2.Days[today]; got.ValueUSD != 2.0 {
		t.Fatalf("persisted row lost: %+v", got)
	}
	// Series is sorted and capped.
	days, m := ch.Series()
	if len(days) != 1 || days[0] != today || m[today].Tokens != 100 {
		t.Fatalf("series = %v", days)
	}
}
