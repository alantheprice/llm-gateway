package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"llmgateway/internal/config"
)

// handleCostsBackfill: POST /admin/costs/backfill
//
// Rebuilds past cost_history days from the engines' own energy.daily
// buckets (31-day retention) — repairing rows written by the old
// page-view-only recording (which froze early, missed days, and
// MAX-merged wrong values forever). Overhead uses each host's configured
// idle watts; capital uses the host's daily amortization. Past days are
// SET (not MAX-merged) — the engines are the source of truth.
func (s *Server) handleCostsBackfill(w http.ResponseWriter, r *http.Request) {
	if !s.adminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		errBody(w, 405, "POST only")
		return
	}
	ops := s.Ops()
	if ops == nil {
		errBody(w, 503, "ops store not attached")
		return
	}

	rate := s.cfg.ElectricityRate
	if rate <= 0 {
		rate = 0.125
	}

	// Gather energy.daily per backend from the poller's captured payloads.
	type dayAgg struct {
		kwh    float64
		energy float64
	}
	perDay := map[string]*dayAgg{}

	s.mu.Lock()
	for backendURL, raw := range s.lastMetrics {
		if _, ok := s.hostByIP(backendHostIP(backendURL)); !ok {
			continue
		}
		energy, ok := raw["energy"].(map[string]any)
		if !ok {
			continue
		}
		daysRaw, ok := energy["daily"].(map[string]any)
		if !ok {
			continue
		}
		for day, v := range daysRaw {
			dm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			kwh := usageNum(dm, "kwh")
			cost := usageNum(dm, "cost_usd")
			agg := perDay[day]
			if agg == nil {
				agg = &dayAgg{}
				perDay[day] = agg
			}
			agg.kwh += kwh
			agg.energy += cost
		}
	}
	s.mu.Unlock()

	// Host daily capital for overhead/capital rows.
	today := time.Now().UTC().Format("2006-01-02")
	repaired := 0
	for day, agg := range perDay {
		if day == today || day == "" {
			continue // today is still live; never rewrite it here
		}
		var overhead, capital float64
		for _, h := range s.cfg.Hosts {
			overhead += h.OverheadWatts / 1000 * 24 * rate
			capital += dayCapital(h.HardwareCostUSD, h.AmortizeYears)
		}
		// Historical routed-token counts aren't recoverable per day.
		if err := replaceCostDay(ops, day, agg.energy, overhead, capital, 0, 0); err != nil {
			continue
		}
		repaired++
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"repaired": repaired,
		"note":     "energy from engine energy.daily; overhead+capital from host config; today not touched; historical token counts unrecoverable (0)",
	})
}

// replaceCostDay: SET (not MAX) — engines are the source of truth for
// historical days and a stale wrong maximum must not win.
func replaceCostDay(ops OpsStore, day string, energy, overhead, capital, value float64, tokens int64) error {
	return ops.ReplaceCostDay(day, energy, overhead, capital, value, tokens)
}

// hostExisted: whether the host was already purchased by this day
// (charges nothing before the hardware existed).
func hostExisted(h config.HostCfg, day string) bool {
	if h.Purchased == "" {
		return true // no purchase date recorded: assume it existed
	}
	purchased, err := time.Parse("2006-01-02", h.Purchased)
	if err != nil {
		return true
	}
	dayT, err := time.Parse("2006-01-02", day)
	if err != nil {
		return true
	}
	return !dayT.Before(purchased)
}

// tokensValueForDay: rebuild a past day's routed tokens and book-price
// value from the ops usage_daily table (per-user chat/embeddings/fim
// rows; key:<id> breakdowns and the synthetic 'lifetime' row excluded).
func (s *Server) tokensValueForDay(day string) (int64, float64) {
	ops := s.Ops()
	if ops == nil {
		return 0, 0
	}
	book := s.cfg.PriceBook
	var tokens int64
	var value float64
	type row struct {
		User         string `db:"user"`
		Kind         string `db:"kind"`
		Requests     int64  `db:"requests"`
		PromptTokens int64  `db:"prompt_tokens"`
		CachedTokens int64  `db:"cached_tokens"`
		OutputTokens int64  `db:"output_tokens"`
	}
	var rows []row
	if err := ops.QueryUsageDay(day, &rows); err != nil {
		return 0, 0
	}
	for _, r := range rows {
		if r.User == "lifetime" || strings.HasPrefix(r.Kind, "key:") {
			continue
		}
		tokens += r.PromptTokens + r.OutputTokens
		value += applyBookPrices(book, int(r.PromptTokens), int(r.CachedTokens), int(r.OutputTokens))
	}
	return tokens, math.Round(value*10000) / 10000
}
