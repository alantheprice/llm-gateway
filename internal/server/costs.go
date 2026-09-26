// Package server — full cost view (admin): GPU energy (engine NVML) +
// system overhead energy + hardware amortization, per host (1 IP = 1 box).
// Route: GET /usage/costs (JSON) and /admin/costs (page).
//
// All-in $/M answers "what am I actually paying": cloud prices bundle
// capex + whole-machine power + margin; without hosts config the gateway
// only sees the GPUs' share of the bill.
package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"llmgateway/internal/config"
)

// HostCost is the computed cost picture for one configured host.
type HostCost struct {
	Label  string `json:"label"`
	IPs    []string
	Config HostConfig

	// GPU energy — summed from engine /usage payloads on this host.
	GPUKwhToday   float64 `json:"gpu_kwh_today"`
	GPUCostToday  float64 `json:"gpu_cost_usd_today"`
	TokensToday   float64 `json:"tokens_today"`
	GPUCost30dUSD float64 `json:"gpu_cost_usd_30d"`

	// Overhead (whole-system minus GPU): watts × elapsed UTC day × rate.
	// (Day boundary is UTC everywhere — engines bucket energy by UTC.)
	OverheadKwhToday  float64 `json:"overhead_kwh_today"`
	OverheadCostToday float64 `json:"overhead_cost_usd_today"`
	OverheadCost30d   float64 `json:"overhead_cost_usd_30d"`

	// Capital amortization (straight line).
	HardwareUSD    float64 `json:"hardware_cost_usd"`
	Purchased      string  `json:"purchased,omitempty"`
	AmortizeYears  float64 `json:"amortize_years,omitempty"`
	DailyCapital   float64 `json:"capital_usd_per_day"`
	CapitalToday   float64 `json:"capital_usd_today"`
	Capital30d     float64 `json:"capital_usd_30d"`
	CapitalPaid    float64 `json:"capital_accrued_usd"` // capped at HardwareUSD
	MonthsElapsed  float64 `json:"months_elapsed"`
	MonthsLeft     float64 `json:"months_remaining"`
	FullyAmort     bool    `json:"fully_amortized"`
	PctDepreciated float64 `json:"pct_depreciated"`

	// Totals.
	TotalToday   float64 `json:"total_cost_usd_today"`
	Total30d     float64 `json:"total_cost_usd_30d"`
	AllInPerM    any     `json:"all_in_usd_per_m_tokens_today"` // nil when no tokens
	AllIn30dPerM any     `json:"all_in_usd_per_m_tokens_30d"`

	// Forward schedule: monthly rows until fully amortized (cap 48).
	Schedule []ScheduleRow `json:"schedule"`
}

// ScheduleRow is one month of the forward amortization projection.
type ScheduleRow struct {
	Month     string  `json:"month"` // YYYY-MM
	Capital   float64 `json:"capital_usd"`
	EnergyEst float64 `json:"energy_est_usd"` // at today's run-rate
	Total     float64 `json:"total_usd"`
	CumPaid   float64 `json:"capital_cumulative_usd"`
}

// HostConfig mirrors config.HostCfg (kept local to avoid import cycles in
// tests; the server converts before calling).
type HostConfig struct {
	Label         string
	IPs           []string
	OverheadWatts float64
	HardwareUSD   float64
	Purchased     string
	AmortizeYears float64
}

// CostsParams bundles everything the cost math needs (pure — unit-testable).
type CostsParams struct {
	Hosts         []HostConfig
	RateUSDPerKwh float64
	Now           time.Time
	// Per-host engine facts, keyed by host index (already matched).
	GPUKwhToday  []float64
	GPUCostToday []float64
	GPUKwh30d    []float64
	GPUCost30d   []float64
	TokensToday  []float64
}

const amortMonthsCap = 48

// dayCapital: straight-line daily capital charge.
func dayCapital(hardware, years float64) float64 {
	if hardware <= 0 || years <= 0 {
		return 0
	}
	return hardware / (years * 365.25)
}

// capitalState returns accrued capital (capped) and month bookkeeping.
func capitalState(hardware, years float64, purchased string, now time.Time) (paid, daily float64, monthsElapsed, monthsLeft float64, fully bool) {
	daily = dayCapital(hardware, years)
	if daily <= 0 {
		return 0, 0, 0, 0, hardware <= 0
	}
	monthsTotal := years * 12
	if purchased != "" {
		if t, err := time.ParseInLocation("2006-01-02", purchased, time.UTC); err == nil && t.Before(now) {
			days := now.Sub(t).Hours() / 24
			paid = math.Min(daily*days, hardware)
			monthsElapsed = days / 30.4375
		}
	}
	monthsLeft = math.Max(0, monthsTotal-monthsElapsed)
	fully = paid >= hardware-0.005
	return
}

// ComputeCosts is the pure cost engine (1 IP = 1 box).
func ComputeCosts(p CostsParams) []HostCost {
	now := p.Now
	utcNow := now.UTC()
	hoursElapsed := float64(utcNow.Hour()) + float64(utcNow.Minute())/60 + float64(utcNow.Second())/3600
	rate := p.RateUSDPerKwh
	if rate <= 0 {
		rate = 0.125
	}
	out := make([]HostCost, 0, len(p.Hosts))
	for i, hc := range p.Hosts {
		h := HostCost{Label: hc.Label, IPs: hc.IPs, Config: hc}
		if i < len(p.GPUKwhToday) {
			h.GPUKwhToday = p.GPUKwhToday[i]
		}
		if i < len(p.GPUCostToday) {
			h.GPUCostToday = p.GPUCostToday[i]
		}
		if i < len(p.GPUCost30d) {
			h.GPUCost30dUSD = p.GPUCost30d[i]
		}
		if i < len(p.TokensToday) {
			h.TokensToday = p.TokensToday[i]
		}
		h.OverheadKwhToday = hc.OverheadWatts / 1000 * hoursElapsed
		h.OverheadCostToday = h.OverheadKwhToday * rate
		h.OverheadCost30d = hc.OverheadWatts / 1000 * 24 * 30 * rate

		h.HardwareUSD = hc.HardwareUSD
		h.Purchased = hc.Purchased
		h.AmortizeYears = hc.AmortizeYears
		h.DailyCapital = dayCapital(hc.HardwareUSD, hc.AmortizeYears)
		h.CapitalToday = h.DailyCapital
		h.Capital30d = h.DailyCapital * 30
		paid, _, me, ml, fully := capitalState(hc.HardwareUSD, hc.AmortizeYears, hc.Purchased, now)
		h.CapitalPaid = paid
		h.MonthsElapsed = me
		h.MonthsLeft = ml
		h.FullyAmort = fully
		if hc.HardwareUSD > 0 {
			h.PctDepreciated = math.Round(paid/hc.HardwareUSD*1000) / 10
		}

		h.TotalToday = h.GPUCostToday + h.OverheadCostToday + h.CapitalToday
		h.Total30d = h.GPUCost30dUSD + h.OverheadCost30d + h.Capital30d
		if h.TokensToday > 0 {
			h.AllInPerM = math.Round(h.TotalToday/h.TokensToday*1e6*100) / 100
			// 30d projection uses today's token run-rate (no per-day history
			// per host yet): conservative = today's $/M.
			h.AllIn30dPerM = h.AllInPerM
		}

		// Forward schedule, monthly, at today's energy run-rate.
		if h.DailyCapital > 0 {
			months := int(math.Ceil(h.MonthsLeft))
			if months > amortMonthsCap {
				months = amortMonthsCap
			}
			energyMonth := (h.GPUCostToday + h.OverheadCostToday) * 30
			cum := h.CapitalPaid
			start := time.Date(utcNow.Year(), utcNow.Month(), 1, 0, 0, 0, 0, time.UTC)
			for m := 0; m < months; m++ {
				mo := start.AddDate(0, m+1, 0)
				monthCap := math.Min(h.DailyCapital*30.4375, math.Max(0, hc.HardwareUSD-cum))
				if monthCap < 0.005 {
					break
				}
				cum += monthCap
				h.Schedule = append(h.Schedule, ScheduleRow{
					Month:     mo.Format("2006-01"),
					Capital:   math.Round(monthCap*100) / 100,
					EnergyEst: math.Round(energyMonth*100) / 100,
					Total:     math.Round((monthCap+energyMonth)*100) / 100,
					CumPaid:   math.Round(cum*100) / 100,
				})
			}
		}
		out = append(out, h)
	}
	return out
}

// backendHostIP extracts the host part of a backend URL.
func backendHostIP(backend string) string {
	u, err := url.Parse(backend)
	if err != nil || u.Host == "" {
		// bare "host:port"
		if i := strings.LastIndex(backend, ":"); i > 0 {
			return backend[:i]
		}
		return backend
	}
	return u.Hostname()
}

// usageCostsPayload gathers engines and computes the full cost picture.
func (s *Server) usageCostsPayload() map[string]any {
	s.mu.Lock()
	hosts := make([]HostConfig, len(s.cfg.Hosts))
	for i, h := range s.cfg.Hosts {
		hosts[i] = HostConfig{
			Label: h.Label, IPs: h.IPs, OverheadWatts: h.OverheadWatts,
			HardwareUSD: h.HardwareCostUSD, Purchased: h.Purchased,
			AmortizeYears: h.AmortizeYears,
		}
	}
	rate := s.cfg.ElectricityRate
	// map backend URL -> host index
	backendHost := map[string]int{}
	for i, h := range hosts {
		for _, ip := range h.IPs {
			backendHost[ip] = i
		}
	}
	backends := make([]string, 0, len(s.backends))
	for u := range s.backends {
		backends = append(backends, u)
	}
	port := s.cfg.Gateway.Port
	// Host idle-watt allowances (fixed-cost layer; 0 → 40 default).
	idleByHost := map[string]float64{}
	for _, h := range s.cfg.Hosts {
		w := h.GPUIdleWatts
		if w <= 0 {
			w = 40
		}
		for _, ip := range h.IPs {
			idleByHost[ip] = w
		}
	}
	book := s.cfg.PriceBook
	s.mu.Unlock()

	gpuKwhToday := make([]float64, len(hosts))
	gpuCostToday := make([]float64, len(hosts))
	gpuCost30d := make([]float64, len(hosts))
	tokensToday := make([]float64, len(hosts))
	gpuKwh30d := make([]float64, len(hosts))

	usage := s.gatherUsage(backends)
	for u, payload := range usage {
		ip := backendHostIP(u)
		idx, ok := backendHost[ip]
		if !ok {
			continue // unconfigured host: GPU-only view lives in /usage
		}
		gpuKwhToday[idx] += usageNum(payload, "energy", "today", "kwh")
		gpuCostToday[idx] += usageNum(payload, "energy", "today", "cost_usd")
		gpuKwh30d[idx] += usageNum(payload, "energy", "rolling_30d", "kwh")
		gpuCost30d[idx] += usageNum(payload, "energy", "rolling_30d", "cost_usd")
		tokensToday[idx] += usageNum(payload, "energy", "today", "tokens")
	}

	// Per-host GPU kwh + idle watts aligned to the hosts slice (marginal/
	// fixed split below).
	gpuKwh := make([]float64, len(hosts))
	idleW := make([]float64, len(hosts))
	for u, payload := range usage {
		ip := backendHostIP(u)
		for i, h := range hosts {
			for _, hip := range h.IPs {
				if hip == ip {
					gpuKwh[i] += usageNum(payload, "energy", "today", "kwh")
					idleW[i] = idleByHost[ip]
				}
			}
		}
	}

	// Per-host GPU kwh + idle watts aligned to the hosts slice (for the
	// marginal/fixed split below).
	for u, payload := range usage {
		ip := backendHostIP(u)
		for i, h := range hosts {
			for _, hip := range h.IPs {
				if hip == ip {
					gpuKwh[i] += usageNum(payload, "energy", "today", "kwh")
					idleW[i] = idleByHost[ip]
				}
			}
		}
	}

	hostCosts := ComputeCosts(CostsParams{
		Hosts: hosts, RateUSDPerKwh: rate, Now: time.Now(),
		GPUKwhToday: gpuKwhToday, GPUCostToday: gpuCostToday,
		GPUKwh30d: gpuKwh30d, GPUCost30d: gpuCost30d, TokensToday: tokensToday,
	})

	// Fleet totals.
	var totalToday, total30d, tokens float64
	for _, h := range hostCosts {
		totalToday += h.TotalToday
		total30d += h.Total30d
		tokens += h.TokensToday
	}
	var fleetPerM any
	if tokens > 0 {
		fleetPerM = math.Round(totalToday/tokens*1e6*100) / 100
	}

	// Value at the operator's book prices vs actual cost today.
	todays := s.usage.TodaySnapshot()
	var pTok, oTok, cTok int
	var valueToday float64
	userValue := map[string]float64{}
	for name, t := range todays {
		pTok += t.PromptTokens
		oTok += t.OutputTokens
		cTok += t.CachedTokens
		v := applyBookPrices(book, t.PromptTokens, t.CachedTokens, t.OutputTokens)
		valueToday += v
		if v > 0 {
			userValue[name] = math.Round(v*1000) / 1000
		}
	}

	// Freeze today's row into the cost history (chart accumulates daily).
	var gpuCost, overhead, capital float64
	for _, h := range hostCosts {
		gpuCost += h.GPUCostToday
		overhead += h.OverheadCostToday
		capital += h.CapitalToday
	}
	now := time.Now()
	s.costHistory.RecordDay(now.UTC().Format("2006-01-02"), CostDay{
		EnergyUSD:   math.Round(gpuCost*10000) / 10000,
		OverheadUSD: math.Round(overhead*10000) / 10000,
		CapitalUSD:  math.Round(capital*10000) / 10000,
		Tokens:      int64(pTok + oTok),
		ValueUSD:    math.Round(valueToday*10000) / 10000,
	})
	if ops := s.Ops(); ops != nil {
		_ = ops.UpsertCostDay(now.UTC().Format("2006-01-02"),
			gpuCost, overhead, capital, valueToday, int64(pTok+oTok))
	}

	// Unmatched backends (visible so admins notice uncounted GPU hosts).
	configured := map[string]bool{}
	for _, h := range hosts {
		for _, ip := range h.IPs {
			configured[ip] = true
		}
	}
	unmatched := []string{}
	for _, u := range backends {
		if !configured[backendHostIP(u)] {
			unmatched = append(unmatched, u)
		}
	}
	sort.Strings(unmatched)

	return map[string]any{
		"electricity_rate_usd_per_kwh": rate,
		"hosts":                        hostCosts,
		"unmatched_backends":           unmatched,
		"totals": map[string]any{
			"total_cost_usd_today":      math.Round(totalToday*100) / 100,
			"total_cost_usd_30d":        math.Round(total30d*100) / 100,
			"tokens_today":              tokens,
			"all_in_usd_per_m_tokens":   fleetPerM,
			"projected_monthly_usd":     math.Round(totalToday*30*100) / 100,
			"gpu_only_usd_per_m_tokens": gpuOnlyPerM(hostCosts, tokens),
		},
		"price_book":       book,
		"user_value_today": userValue,
		"value_today":      math.Round(valueToday*100) / 100,
		"cost_history":     s.costHistorySeries(),
		"peaks":            s.peaks.Snapshot(),
		"gateway_port":     port,
	}
}

// costHistorySeries: ordered days with cost + value for the chart.
// Prefers SQLite (source of truth once imported); JSON store is fallback.
func (s *Server) costHistorySeries() map[string]any {
	if ops := s.Ops(); ops != nil {
		if rows, err := ops.CostSeries(90); err == nil && len(rows) > 0 {
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, map[string]any{
					"day":          r.Day,
					"energy_usd":   r.EnergyUSD,
					"overhead_usd": r.OverheadUSD,
					"capital_usd":  r.CapitalUSD,
					"total_usd":    r.Total(),
					"tokens":       r.Tokens,
					"value_usd":    r.ValueUSD,
				})
			}
			return map[string]any{"days": out, "source": "sqlite"}
		}
	}
	days, m := s.costHistory.Series()
	rows := make([]map[string]any, 0, len(days))
	for _, d := range days {
		c := m[d]
		rows = append(rows, map[string]any{
			"day":          d,
			"energy_usd":   c.EnergyUSD,
			"overhead_usd": c.OverheadUSD,
			"capital_usd":  c.CapitalUSD,
			"total_usd":    c.EnergyUSD + c.OverheadUSD + c.CapitalUSD,
			"tokens":       c.Tokens,
			"value_usd":    c.ValueUSD,
		})
	}
	return map[string]any{"days": rows, "source": "json"}
}

func gpuOnlyPerM(hosts []HostCost, tokens float64) any {
	if tokens <= 0 {
		return nil
	}
	var gpu float64
	for _, h := range hosts {
		gpu += h.GPUCostToday
	}
	if gpu == 0 {
		return nil
	}
	return math.Round(gpu/tokens*1e6*100) / 100
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ---- value-vs-cost: operator's price book ----
//
// The gateway never derives prices. The operator sets prompt/cached/output
// prices ($/1M) in the config; "value" = what traffic would have cost at
// those prices, charted against actual daily cost (energy + overhead +
// capex). The judgement of what the service is worth stays with the
// operator; the gateway just does the accounting.

// applyBookPrices prices one day's token tallies at book rates.
func applyBookPrices(pb config.PriceBook, prompt, cached, output int) float64 {
	return pb.PromptUSDPerM*float64(prompt-cached)/1e6 +
		pb.CachedUSDPerM*float64(cached)/1e6 +
		pb.OutputUSDPerM*float64(output)/1e6
}

// CostHistory: per-day actual cost + value, persisted. Engines keep only
// 31 days of energy buckets and nothing for overhead/capex, so the gateway
// freezes each day's numbers once the day completes — that's what makes
// the value-vs-cost chart cumulative over time.
type CostHistory struct {
	mu       sync.Mutex
	path     string
	Days     map[string]CostDay `json:"days"`
	lastSave time.Time
}

// CostDay is one frozen day of fleet actuals.
type CostDay struct {
	EnergyUSD   float64 `json:"energy_usd"`   // GPU energy (engine NVML)
	OverheadUSD float64 `json:"overhead_usd"` // host watts x rate
	CapitalUSD  float64 `json:"capital_usd"`  // capex accrual
	Tokens      int64   `json:"tokens"`
	ValueUSD    float64 `json:"value_usd"` // at the book prices in force that day
}

func NewCostHistory(path string) *CostHistory {
	c := &CostHistory{path: path, Days: map[string]CostDay{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, c)
		if c.Days == nil {
			c.Days = map[string]CostDay{}
		}
	}
	return c
}

func (c *CostHistory) saveLocked() {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, c.path)
	}
}

// RecordDay upserts a day's row. Past days are frozen (only today writes).
func (c *CostHistory) RecordDay(day string, d CostDay) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Cost day keys are UTC (callers pass now.UTC()); the "only today writes"
	// guard must compare against the same UTC day or every write is dropped.
	if day != time.Now().UTC().Format("2006-01-02") {
		return
	}
	c.Days[day] = d
	if time.Since(c.lastSave) > 30*time.Second {
		c.saveLocked()
		c.lastSave = time.Now()
	}
}

// Series returns the last 90 days in order.
func (c *CostHistory) Series() ([]string, map[string]CostDay) {
	c.mu.Lock()
	defer c.mu.Unlock()
	days := make([]string, 0, len(c.Days))
	for d := range c.Days {
		days = append(days, d)
	}
	sort.Strings(days)
	if len(days) > 90 {
		days = days[len(days)-90:]
	}
	out := make(map[string]CostDay, len(days))
	for _, d := range days {
		out[d] = c.Days[d]
	}
	return days, out
}

func CostHistoryPath(usagePath string) string {
	return filepath.Join(filepath.Dir(usagePath), "cost_history.json")
} // peakCapacity: fleet throughput ceiling = per-request peak decode/prefill
// × the backend's lane count, summed over backends. A 6-lane GPU decoding
// ~160 tok/s per stream delivers ~960 tok/s; that's the number a
// utilization assumption applies to.
func (s *Server) peakCapacity() (pp, tg float64) {
	s.mu.Lock()
	lanes := map[string]int{}
	for url, l := range s.tracker.Snapshot() {
		n := l.Lanes
		if n <= 0 {
			n = l.MaxSeqs
		}
		if n <= 0 {
			n = s.cfg.MaxSeqsFor(url)
		}
		lanes[url] = n
	}
	s.mu.Unlock()
	for url, raw := range s.peaks.Snapshot() {
		bp, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ppPeak, _ := bp["pp_tok_per_s_peak"].(float64)
		tgPeak, _ := bp["tg_tok_per_s_peak"].(float64)
		n := lanes[url]
		if n <= 0 {
			n = 1
		}
		pp += ppPeak * float64(n)
		tg += tgPeak * float64(n)
	}
	return pp, tg
}

// handleUsageCosts already returns the envelope below — pricing block and
// per-user value attribution are added to it.

// handleUsageCosts: GET /usage/costs — full cost picture (admin only).

func (s *Server) handleUsageCosts(w http.ResponseWriter, r *http.Request) {
	if !s.adminGate(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.usageCostsPayload())
}

// handleAdminCostsPage serves the costs UI page.
func (s *Server) handleAdminCostsPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok || sess.Role != "admin" {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	s.renderPage(w, r, "admin_costs.html", "admin_costs", "Costs")
}
