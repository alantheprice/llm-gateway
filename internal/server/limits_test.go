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

func TestComputePricingV2MarginalFixed(t *testing.T) {
	// One host: 10 kWh GPU today, 12h elapsed, 40W idle (0.48 kWh idle).
	// Rate 0.125 -> marginal = (10-0.48)*0.125 = $1.19.
	// Fixed today = $10 (capital+overhead+idle energy).
	in := PricingInputs{
		GPUKwhToday: []float64{10}, GPUIdleWatts: []float64{40},
		RateUSDPerKwh: 0.125, DayElapsedHours: 12,
		FixedToday: 10,
		PPtokPerS:  4000, TGtokPerS: 500,
		PromptTokens: 1_000_000, OutputTokens: 1_000_000,
		ExpectedTokensPerDay: 50_000_000,
		CacheDiscountPct:     75,
	}
	p := ComputePricingV2(in)
	if p.MarginalToday < 1.18 || p.MarginalToday > 1.20 {
		t.Fatalf("marginal = %v, want 1.19", p.MarginalToday)
	}
	// GPU-time: pp 250s, tg 2000s -> pp share 1/9.
	// marginal pp = 1.19*(1/9)/1M*1e6 = $0.13/M; tg = 1.19*(8/9) = $1.06/M.
	if p.MarginalPPPerM < 0.12 || p.MarginalPPPerM > 0.14 {
		t.Fatalf("marginal pp = %v, want 0.13", p.MarginalPPPerM)
	}
	if p.MarginalTGPerM < 1.05 || p.MarginalTGPerM > 1.07 {
		t.Fatalf("marginal tg = %v, want 1.06", p.MarginalTGPerM)
	}
	// Usage price = marginal (+ margin). Fixed is NOT in the per-token
	// price — it's a capacity fee, time-based like the cost itself.
	if p.PromptPerM != 0.13 || p.OutputPerM != 1.06 {
		t.Fatalf("usage prices pp=%v tg=%v, want 0.13/1.06 (marginal only)", p.PromptPerM, p.OutputPerM)
	}
	// Fixed reference add-on at basis volume ($10 / 50M = $0.20/M) — a
	// REFERENCE, not part of the price. Hyperbolic: halves if volume
	// doubles.
	if p.FixedPPPerM != 0.2 || p.FixedTGPerM != 0.2 {
		t.Fatalf("fixed reference pp=%v tg=%v, want 0.20/0.20", p.FixedPPPerM, p.FixedTGPerM)
	}
	if p.FixedMonthly != 300 {
		t.Fatalf("fixed monthly = %v, want 300", p.FixedMonthly)
	}
	// Additivity of the REFERENCE all-in: usage price + fixed reference
	// equals the all-in at basis volume (marginal unaffected by volume).
	if math.Abs((p.PromptPerM+p.FixedPPPerM)-(p.MarginalPPPerM+p.FixedPPPerM)) > 0.011 {
		t.Fatal("usage price must equal marginal")
	}
	// Cached = usage prompt price − 75% (0.13 × 0.25 = 0.03).
	if p.CachedPerM < 0.02 || p.CachedPerM > 0.04 {
		t.Fatalf("cached = %v, want 0.03", p.CachedPerM)
	}
	if p.FixedMonthly != 300 {
		t.Fatalf("fixed monthly = %v, want 300", p.FixedMonthly)
	}
}

func TestExpectedVolume(t *testing.T) {
	if ExpectedVolume(0, 42e6) != 42e6 {
		t.Fatal("auto should use 7-day avg")
	}
	if ExpectedVolume(10e6, 42e6) != 10e6 {
		t.Fatal("explicit config must win")
	}
	if ExpectedVolume(0, 0) != 1e6 {
		t.Fatal("empty history should floor at 1M")
	}
	if ExpectedVolume(0, 500e3) != 1e6 {
		t.Fatal("tiny avg should floor at 1M")
	}
}
