package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

// backfillOps: the two OpsStore calls the backfill makes; anything else
// panics (nil embedded interface).
type backfillOps struct {
	OpsStore
	usage map[string][]map[string]any
	rows  map[string][4]float64 // day -> energy, overhead, capital, value
	toks  map[string]int64
}

func (o *backfillOps) QueryUsageDay(day string, dest any) error {
	b, _ := json.Marshal(o.usage[day])
	return json.Unmarshal(b, dest)
}

func (o *backfillOps) ReplaceCostDay(day string, energy, overhead, capital, value float64, tokens int64) error {
	o.rows[day] = [4]float64{energy, overhead, capital, value}
	o.toks[day] = tokens
	return nil
}

// Backfill: one GPU seen at two URLs counts once; a host bought later is
// not charged; tokens/value come from usage rows; today is left alone.
func TestCostsBackfill(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0},"electricity_rate_usd_per_kwh":0.1,
	 "price_book":{"prompt_usd_per_m":1,"cached_usd_per_m":0,"output_usd_per_m":2},
	 "hosts":[
	  {"label":"a","ips":["127.0.0.1"],"overhead_watts":100,"hardware_cost_usd":3650,"purchased":"2026-01-01","amortize_years":1},
	  {"label":"b","ips":["192.168.1.200"],"links":["gpu-b"],"overhead_watts":100,"hardware_cost_usd":3650,"purchased":"2026-09-25","amortize_years":1}]}`, nil)
	today := time.Now().UTC().Format("2006-01-02")
	daily := func(kwh float64) map[string]any {
		return map[string]any{"energy": map[string]any{"daily": map[string]any{
			"2026-09-24": map[string]any{"kwh": kwh, "cost_usd": kwh / 10},
			today:        map[string]any{"kwh": 1.0, "cost_usd": 0.1},
		}}}
	}
	s.mu.Lock()
	s.lastMetrics = map[string]map[string]any{
		"http://127.0.0.1:8000":     daily(2),
		"http://192.168.1.200:8006": daily(3), // host b, old direct URL
		"http://link/gpu-b:8006":    daily(3), // host b, same engine via link
	}
	s.mu.Unlock()
	ops := &backfillOps{rows: map[string][4]float64{}, toks: map[string]int64{},
		usage: map[string][]map[string]any{"2026-09-24": {
			{"User": "alice", "Kind": "chat", "PromptTokens": 1000000, "CachedTokens": 0, "OutputTokens": 500000},
			{"User": "alice", "Kind": "key:laptop", "PromptTokens": 1000000, "OutputTokens": 500000}, // breakdown: skip
			{"User": "lifetime", "Kind": "chat", "PromptTokens": 9e9},
		}}}
	s.SetOps(ops)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/admin/costs/backfill", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "root", Role: "admin"}, time.Hour)})
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("backfill: %d %s", w.Code, w.Body)
	}
	if _, ok := ops.rows[today]; ok {
		t.Fatal("today was rewritten")
	}
	got := ops.rows["2026-09-24"]
	if e := got[0]; e < 0.499 || e > 0.501 { // 0.2 (a) + 0.3 (b once), not 0.8
		t.Fatalf("energy = %v, want 0.5 (duplicate GPU URL counted twice?)", e)
	}
	if o := got[1]; o < 0.239 || o > 0.241 { // only host a: 100 W * 24 h * $0.1
		t.Fatalf("overhead = %v, want 0.24 (host b not bought yet)", o)
	}
	if c := got[2]; c < 9.99 || c > 10.01 { // host a: 3650 / 365
		t.Fatalf("capital = %v, want 10", c)
	}
	if ops.toks["2026-09-24"] != 1500000 || got[3] < 1.999 || got[3] > 2.001 {
		t.Fatalf("tokens/value = %d / %v, want 1500000 / 2", ops.toks["2026-09-24"], got[3])
	}
}
