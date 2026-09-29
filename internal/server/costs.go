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
	"log"
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
	"llmgateway/internal/embeddedpb"
)

// HostCost is the computed cost picture for one configured host.
type HostCost struct {
	Label  string   `json:"label"`
	IPs    []string `json:"host_keys"` // match keys: IPs + "link:<agent>"
	IPList []string `json:"ips"`       // configured IPs (display)
	Links  []string `json:"links"`     // configured link agent names (display)
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
	IPs           []string // match keys (config.HostCfg.HostKeys)
	RawIPs        []string // configured IPs, for display
	Links         []string // configured link agents, for display
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
		h := HostCost{Label: hc.Label, IPs: hc.IPs, IPList: hc.RawIPs, Links: hc.Links, Config: hc}
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
		// Capital accrues by the hour like overhead, so "today" is a true
		// part-day number: $/M is stable through the day and the monthly
		// projection (today / day-fraction × 30) is coherent.
		h.CapitalToday = h.DailyCapital * (hoursElapsed / 24)
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
		// AllIn30dPerM: 30-day cost over 30-day tokens when the engine
		// reports a rolling 30-day token count; else today's $/M is the
		// honest estimate (no per-host token history yet).
		h.AllIn30dPerM = h.AllInPerM
		if h.TokensToday > 0 {
			h.AllInPerM = round4(h.TotalToday / h.TokensToday * 1e6)
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

// backendHostIP extracts the host key of a backend URL: the IP/hostname
// for LAN backends, "link:<agent>" for link engines (which have no IP —
// hosts claim them via their `links` list).
func backendHostIP(backend string) string {
	if isLinkURL(backend) {
		rest := strings.TrimPrefix(backend, "http://link/")
		if i := strings.LastIndex(rest, ":"); i > 0 {
			rest = rest[:i]
		}
		return "link:" + strings.ToLower(rest)
	}
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
			Label: h.Label, IPs: h.HostKeys(), RawIPs: h.IPs, Links: h.Links, OverheadWatts: h.OverheadWatts,
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
	backends = append(backends, s.linkReg.LiveURLs()...) // link engines cost like LAN ones
	port := s.cfg.Gateway.Port
	// Host idle-watt allowances (fixed-cost layer; 0 → 40 default).
	idleByHost := map[string]float64{}
	for _, h := range s.cfg.Hosts {
		w := h.GPUIdleWatts
		if w <= 0 {
			w = 40
		}
		for _, ip := range h.HostKeys() {
			idleByHost[ip] = w
		}
	}
	book := s.cfg.PriceBook
	s.mu.Unlock()

	uptimeShort := make([]bool, len(hosts))
	gpuKwhToday := make([]float64, len(hosts))
	gpuCostToday := make([]float64, len(hosts))
	gpuCost30d := make([]float64, len(hosts))
	tokensToday := make([]float64, len(hosts))
	gpuKwh30d := make([]float64, len(hosts))

	usage := s.gatherUsage(backends)
	groups, reporters, kwhBy := s.countedEnergy(usage)

	for u, payload := range usage {
		ip := backendHostIP(u)
		idx, ok := backendHost[ip]
		if !ok {
			continue // unconfigured host: GPU-only view lives in /usage
		}
		tokensToday[idx] += usageNum(payload, "energy", "today", "tokens")
		if !reporters[u] {
			continue // shares a card with a service whose report counts
		}
		gpuKwhToday[idx] += usageNum(payload, "energy", "today", "kwh")
		gpuCostToday[idx] += usageNum(payload, "energy", "today", "cost_usd")
		// uptime < seconds-since-UTC-midnight ⇒ restarted mid-day
		upS := usageNum(payload, "uptime", "seconds")
		nowUTC := time.Now().UTC()
		utcSec := float64(nowUTC.Hour())*3600 + float64(nowUTC.Minute())*60 + float64(nowUTC.Second())
		if upS > 0 && upS < utcSec {
			uptimeShort[idx] = true
		}
		gpuKwh30d[idx] += usageNum(payload, "energy", "rolling_30d", "kwh")
		gpuCost30d[idx] += usageNum(payload, "energy", "rolling_30d", "cost_usd")
	}

	// Engine-restart guard: NInfer resets energy.today.tokens on restart
	// but keeps the day's kWh — tokens can sit anywhere below the true
	// count, so threshold checks (tokens < 1000) don't catch real resets
	// (observed: 3,767 tokens vs 2.56 kWh after a restart ≈ $740/M).
	// Reliable signal: the engine's uptime. If the engine has been up
	// LESS time than has elapsed in the UTC day, it restarted mid-day and
	// its "today" counters are incomplete → the host's $/M reads nil
	// rather than a wrong number. Recovers on the next UTC day.
	for i := range hosts {
		if uptimeShort[i] {
			tokensToday[i] = 0
		}
	}

	routedBy := map[string]float64{}
	// Tokens: prefer the gateway's own request log (every routed request,
	// per backend, this UTC day). It survives engine restarts, so the
	// restart guard above only matters when analytics is unavailable.
	if ops := s.Ops(); ops != nil {
		var rows []embeddedpb.BackendTokens
		if err := ops.QueryBackendTokensDay(time.Now().UTC().Format("2006-01-02"), &rows); err == nil && len(rows) > 0 {
			gw := make([]float64, len(hosts))
			for _, r := range rows {
				routedBy[r.Backend] += float64(r.Tokens)
				if idx, ok := backendHost[backendHostIP(r.Backend)]; ok {
					gw[idx] += float64(r.Tokens)
				}
			}
			tokensToday = gw
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
		fleetPerM = round4(totalToday / tokens * 1e6)
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

	// Freeze today's row into the cost history — also invoked from the
	// poll loop (recordCostToday) so a day nobody views still gets its row.
	var gpuCost, overhead, capital float64
	for _, h := range hostCosts {
		gpuCost += h.GPUCostToday
		overhead += h.OverheadCostToday
		capital += h.CapitalToday
	}
	s.recordCostToday(gpuCost, overhead, capital, valueToday, int64(pTok+oTok))

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

	out := map[string]any{
		"electricity_rate_usd_per_kwh": rate,
		"hosts":                        hostCosts,
		"unmatched_backends":           unmatched,
		"unmatched_gpus":               s.labelled(unmatched),
		"totals": map[string]any{
			"total_cost_usd_today":      math.Round(totalToday*100) / 100,
			"total_cost_usd_30d":        math.Round(total30d*100) / 100,
			"tokens_today":              tokens,
			"all_in_usd_per_m_tokens":   fleetPerM,
			"projected_monthly_usd":     math.Round(projMonth(totalToday)*100) / 100,
			"gpu_only_usd_per_m_tokens": gpuOnlyPerM(hostCosts, tokens),
		},
		"gpus":             s.gpuEnergyRows(groups, reporters, kwhBy, usage, routedBy),
		"energy_warnings":  energyWarnings(s.hostsSnapshot(), groups, kwhBy),
		"price_book":       book,
		"user_value_today": userValue,
		"value_today":      math.Round(valueToday*100) / 100,
		"cost_history":     s.costHistorySeries(),
		"peaks":            s.peaks.Snapshot(),
		"gateway_port":     port,
	}
	if ops := s.Ops(); ops != nil {
		if rows, err := ops.GPUDaily(time.Now().UTC().Format("2006-01-02")); err == nil {
			// Live update from the poller's captured payload, then express
			// the per-day counters as DELTAS (lifetime minus day-start
			// snapshot) so the card shows today's traffic, not lifetime.
			s.mu.Lock()
			for i := range rows {
				if raw := s.lastMetrics[rows[i].Backend]; raw != nil {
					if h := int64(usageNum(raw, "tokens", "input", "cache_hits")); h > rows[i].CacheHits {
						rows[i].CacheHits = h
					}
					if t := int64(usageNum(raw, "tokens", "input", "total")); t > rows[i].EngineInput {
						rows[i].EngineInput = t
					}
				}
				if d := rows[i].CacheHits - rows[i].DayStartCache; d > 0 {
					rows[i].CacheHits = d
				} else {
					rows[i].CacheHits = 0 // day-start snapshot was taken mid/post-day
				}
				if d := rows[i].EngineInput - rows[i].DayStartInput; d > 0 {
					rows[i].EngineInput = d
				} else {
					rows[i].EngineInput = 0
				}
			}
			hosts := s.cfg.Hosts
			s.mu.Unlock()
			out["gpu_today"] = attributeServiceEnergy(hosts, mergeGPURowsByIdentity(hosts, rows), groups, kwhBy, routedBy)
		}
	}
	return out
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

// projMonth: extrapolate a part-day total to 30 days. Day fraction comes
// from UTC clock — same basis the per-day buckets use.
func projMonth(totalToday float64) float64 {
	now := time.Now().UTC()
	frac := (float64(now.Hour()) + float64(now.Minute())/60 + float64(now.Second())/3600) / 24
	if frac < 1.0/24 {
		frac = 1.0 / 24 // first hour: don't divide by ~0
	}
	return totalToday / frac * 30
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
	return round4(gpu / tokens * 1e6)
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

// recordCostToday: freeze today's cost-history row (energy, overhead,
// capital, routed tokens, value). Called from usageCostsPayload AND the
// poll loop so a day nobody views still accumulates. MAX-merge upsert
// keeps the value converging through the day.
func (s *Server) recordCostToday(gpuCost, overhead, capital, valueToday float64, tokens int64) {
	now := time.Now().UTC()
	s.costHistory.RecordDay(now.Format("2006-01-02"), CostDay{
		EnergyUSD:   math.Round(gpuCost*10000) / 10000,
		OverheadUSD: math.Round(overhead*10000) / 10000,
		CapitalUSD:  math.Round(capital*10000) / 10000,
		Tokens:      tokens,
		ValueUSD:    math.Round(valueToday*10000) / 10000,
	})
	if ops := s.Ops(); ops != nil {
		if err := ops.UpsertCostDay(now.Format("2006-01-02"),
			gpuCost, overhead, capital, valueToday, tokens); err != nil {
			log.Printf("cost_history upsert: %v", err)
		}
	}
}

// mergeGPURowsByIdentity folds per-backend rows that are the same engine
// (e.g. its old direct URL and its link URL after a cutover) into one row.
// Rows for one engine carry the same engine counters, so they merge by
// max, not sum.
func mergeGPURowsByIdentity(hosts []config.HostCfg, rows []embeddedpb.GPUDailyRow) []map[string]any {
	type acc struct {
		id                             gpuIdentity
		backends                       []string
		tokens, cacheHits, engineInput int64
		kwh                            float64
	}
	byKey := map[string]*acc{}
	var order []string
	for _, r := range rows {
		id := identifyGPU(hosts, r.Backend)
		a := byKey[id.Key]
		if a == nil {
			a = &acc{id: id}
			byKey[id.Key] = a
			order = append(order, id.Key)
		}
		if r.Backend != "" && isLinkURL(r.Backend) {
			a.id.Via = "link" // current path wins over a stale direct row
		}
		a.backends = append(a.backends, r.Backend)
		a.tokens = max(a.tokens, r.Tokens)
		a.cacheHits = max(a.cacheHits, r.CacheHits)
		a.engineInput = max(a.engineInput, r.EngineInput)
		a.kwh = max(a.kwh, r.Kwh)
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		out = append(out, map[string]any{
			"gpu_key": a.id.Key, "gpu_label": a.id.Label, "host": a.id.Host, "via": a.id.Via,
			"backend": a.backends[0], "backends": a.backends,
			"tokens": a.tokens, "cache_hits": a.cacheHits, "engine_input": a.engineInput, "kwh": a.kwh,
		})
	}
	return out
}

// labelled: backend URLs with their GPU identity, for display lists.
func (s *Server) labelled(urls []string) []map[string]any {
	out := make([]map[string]any, 0, len(urls))
	for _, u := range urls {
		id := s.gpuIdentity(u)
		out = append(out, map[string]any{"url": u, "gpu_label": id.Label, "via": id.Via})
	}
	return out
}
