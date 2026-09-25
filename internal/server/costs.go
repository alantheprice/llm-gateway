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
	"sort"
	"strings"
	"time"
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
		if t, err := time.ParseInLocation("2006-01-02", purchased, time.Local); err == nil && t.Before(now) {
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
	hoursElapsed := float64(now.Hour()) + float64(now.Minute())/60 + float64(now.Second())/3600
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
			start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
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
	margin := s.cfg.PricingMarginPct
	expCfg := s.cfg.PricingExpectedTokensPerDay
	cacheDisc := s.cfg.PricingCacheDiscountPct
	if cacheDisc <= 0 {
		cacheDisc = 75
	}
	s.mu.Unlock()
	_ = margin

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

	// Recommended pricing: two-layer model. Marginal = GPU energy above
	// idle (per-host idle allowance), split pp/tg by GPU-time. Fixed =
	// capex + overhead + idle energy, spread over expected daily volume
	// (config knob, else trailing 7-day average, floor 1M).
	todays := s.usage.TodaySnapshot()
	var pTok, oTok, cTok float64
	for _, t := range todays {
		pTok += float64(t.PromptTokens)
		oTok += float64(t.OutputTokens)
		cTok += float64(t.CachedTokens)
	}
	s.mu.Lock()
	var ppTPS, tgTPS float64
	for _, u := range backends {
		if payload, ok := s.lastMetrics[u]; ok {
			ppTPS += usageNum(payload, "throughput", "prefill_tok_per_s")
			tgTPS += usageNum(payload, "throughput", "decode_tok_per_s")
		}
	}
	s.mu.Unlock()
	// Throughput windows are 5s snapshots — idle windows read ~0 and near-
	// idle reads a few tok/s, either of which wrecks the GPU-time split.
	// Use peak capacity instead: per-request high-water × lanes = the
	// fleet's measured ceiling (decayed), the same basis the expected-
	// volume calculation uses below. (peakCapacity takes s.mu itself —
	// must run unlocked.)
	ppPeak, tgPeak := s.peakCapacity()
	if tgPeak > 0 {
		ppTPS, tgTPS = ppPeak, tgPeak
	} else {
		ppTPS, tgTPS = s.lastGoodPP, s.lastGoodTG
	}

	// Fixed = total all-in today minus marginal GPU energy (capital +
	// overhead + idle energy). Idle-kWh already moves to fixed inside
	// ComputePricingV2, so pass totals and let it do the split.
	var gpuCost, overhead, capital float64
	for _, h := range hostCosts {
		gpuCost += h.GPUCostToday
		overhead += h.OverheadCostToday
		capital += h.CapitalToday
	}
	now := time.Now()
	hoursElapsed := float64(now.Hour()) + float64(now.Minute())/60 + float64(now.Second())/3600

	// Trailing 7-day average daily tokens for the auto expected-volume.
	avg7 := s.usage.AvgDailyTokens(7)
	// Capacity basis: 1/3 of measured peak capacity sustained 24/7 —
	// per-request peak decode × lanes × 86400 ÷ 3.
	ppPeak, tgPeak = s.peakCapacity()
	peakDaily := tgPeak * 86400 / 3
	// The explicit config knob still wins; otherwise capacity basis,
	// floored at 25% of the 7-day actual so a quiet week doesn't over-amortize.
	auto := math.Max(peakDaily, avg7*0.25)
	if auto < 1e6 {
		auto = 1e6
	}
	expected := ExpectedVolume(expCfg, auto)

	pricing := ComputePricingV2(PricingInputs{
		GPUKwhToday: gpuKwh, GPUIdleWatts: idleW, RateUSDPerKwh: rate,
		DayElapsedHours: hoursElapsed,
		FixedToday:      gpuCost + overhead + capital,
		PPtokPerS:       ppTPS, TGtokPerS: tgTPS,
		PromptTokens:         pTok - cTok, // computed prompt only (cached used no prefill compute)
		CachedTokens:         cTok,
		OutputTokens:         oTok,
		ExpectedTokensPerDay: expected,
		CacheDiscountPct:     cacheDisc,
		MarginPct:            margin,
	})

	// Value attribution at floor prices (cached at the cached rate).
	userValue := map[string]float64{}
	if pricing.PromptPerM > 0 || pricing.OutputPerM > 0 {
		pp := pricing.PromptPerM / (1 + margin/100)
		tg := pricing.OutputPerM / (1 + margin/100)
		cc := pricing.CachedPerM / (1 + margin/100)
		for name, t := range todays {
			v := pp*float64(t.PromptTokens-t.CachedTokens)/1e6 + cc*float64(t.CachedTokens)/1e6 + tg*float64(t.OutputTokens)/1e6
			if v > 0 {
				userValue[name] = math.Round(v*1000) / 1000
			}
		}
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
		"recommended_pricing": pricing,
		"user_value_today":    userValue,
		"capacity_basis": map[string]any{
			"peak_fleet_tg_tok_per_s": math.Round(tgPeak*10) / 10,
			"peak_daily_tokens":       math.Round(peakDaily),
			"assumed_utilization_pct": 33.3,
			"avg_7d_daily_tokens":     math.Round(avg7),
			"expected_tokens_per_day": math.Round(expected),
		},
		"peaks":        s.peaks.Snapshot(),
		"gateway_port": port,
	}
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

// PricingRecommendation: what to charge per 1M prompt (pp), generated (tg),
// and cached tokens so revenue covers cost. Two-layer model:
//
//   - Marginal: GPU energy above idle — the true incremental cost of the
//     tokens actually processed today — split pp/tg by measured GPU-time.
//   - Fixed: capex amortization + host overhead + idle GPU power. These
//     accrue whether or not tokens flow, so they're spread over an
//     EXPECTED daily volume (config pricing_expected_tokens_per_day; 0 =
//     trailing 7-day average, floor 1M) — not today's actuals, which made
//     the price swing wildly with light days.
//
// Cached tokens bypass prefill compute (≈no marginal energy) but occupy KV
// memory — they're priced as prompt-minus-discount
// (pricing_cache_discount_pct, default 75 → cached costs 25% of prompt).
type PricingRecommendation struct {
	// Recommended (what to charge).
	PromptPerM float64 `json:"prompt_per_m_usd"`
	OutputPerM float64 `json:"output_per_m_usd"`
	CachedPerM float64 `json:"cached_per_m_usd"`
	// Marginal layer (energy above idle, GPU-time split).
	MarginalPPPerM float64 `json:"marginal_prompt_per_m_usd"`
	MarginalTGPerM float64 `json:"marginal_output_per_m_usd"`
	MarginalToday  float64 `json:"marginal_cost_usd_today"`
	// Fixed layer.
	FixedToday     float64 `json:"fixed_cost_usd_today"`
	FixedMonthly   float64 `json:"fixed_cost_usd_month"`
	ExpectedTokDay float64 `json:"expected_tokens_per_day"`
	FixedPPPerM    float64 `json:"fixed_prompt_per_m_usd"`
	FixedTGPerM    float64 `json:"fixed_output_per_m_usd"`
	// Basis.
	PPtokPerS     float64 `json:"fleet_pp_tok_per_s"`
	TGtokPerS     float64 `json:"fleet_tg_tok_per_s"`
	PromptTokens  float64 `json:"prompt_tokens_today"`
	CachedTokens  float64 `json:"cached_tokens_today"`
	OutputTokens  float64 `json:"output_tokens_today"`
	CacheDiscount float64 `json:"cache_discount_pct"`
	MarginPct     float64 `json:"margin_pct"`
	Note          string  `json:"note"`
}

// PricingInputs bundles the facts ComputePricing needs (pure, testable).
type PricingInputs struct {
	// GPU energy + rate for the marginal split.
	GPUKwhToday     []float64 // per host
	GPUIdleWatts    []float64 // per host
	RateUSDPerKwh   float64
	DayElapsedHours float64
	// Fixed costs (from ComputeCosts totals).
	FixedToday float64 // capital + overhead + idle energy
	// Throughput + volumes.
	PPtokPerS, TGtokPerS float64
	PromptTokens         float64 // computed (non-cached) prompt tokens today
	CachedTokens         float64
	OutputTokens         float64
	ExpectedTokensPerDay float64 // 0 = caller resolved the auto value
	CacheDiscountPct     float64 // share OFF the prompt price for cached
	MarginPct            float64
}

// ExpectedVolume resolves the expected-daily-volume knob: explicit config,
// else the trailing 7-day average (floor 1M so fixed cost doesn't explode
// on an empty week).
func ExpectedVolume(configured, avg7d float64) float64 {
	if configured > 0 {
		return configured
	}
	if avg7d > 1e6 {
		return avg7d
	}
	return 1e6
}

func ComputePricingV2(in PricingInputs) PricingRecommendation {
	p := PricingRecommendation{
		PPtokPerS: in.PPtokPerS, TGtokPerS: in.TGtokPerS,
		PromptTokens: in.PromptTokens, CachedTokens: in.CachedTokens,
		OutputTokens:  in.OutputTokens,
		CacheDiscount: in.CacheDiscountPct, MarginPct: in.MarginPct,
		ExpectedTokDay: in.ExpectedTokensPerDay,
		Note: "Marginal = GPU energy above idle (what each extra token truly costs), split pp/tg by measured GPU-time. " +
			"Fixed = capex + host overhead + idle power, spread over the expected daily volume — not today's actuals. " +
			"Cached tokens skip prefill compute but hold KV memory, so they price at prompt minus the cache discount.",
	}

	// --- marginal layer ---
	var marginal float64
	for i, kwh := range in.GPUKwhToday {
		idle := 40.0
		if i < len(in.GPUIdleWatts) {
			idle = in.GPUIdleWatts[i]
		}
		idleKwh := idle / 1000 * in.DayElapsedHours
		if over := kwh - idleKwh; over > 0 {
			marginal += over * in.RateUSDPerKwh
		}
	}
	p.MarginalToday = round2(marginal)

	// GPU-time split of marginal between prefill and decode.
	ppSecs, tgSecs := 0.0, 0.0
	if in.PPtokPerS > 0 {
		ppSecs = in.PromptTokens / in.PPtokPerS
	}
	if in.TGtokPerS > 0 {
		tgSecs = in.OutputTokens / in.TGtokPerS
	}
	mult := 1 + in.MarginPct/100
	if ppSecs+tgSecs > 0 {
		ppShare := ppSecs / (ppSecs + tgSecs)
		if in.PromptTokens > 0 {
			p.MarginalPPPerM = round2(marginal * ppShare / in.PromptTokens * 1e6 * mult)
		}
		if in.OutputTokens > 0 {
			p.MarginalTGPerM = round2(marginal * (1 - ppShare) / in.OutputTokens * 1e6 * mult)
		}
	}

	// --- fixed layer over expected volume ---
	p.FixedToday = round2(in.FixedToday)
	p.FixedMonthly = round2(in.FixedToday * 30)
	if in.ExpectedTokensPerDay > 0 {
		// Fixed cost is a capacity load: uniform per 1M tokens of EVERY
		// class. (Splitting it by today's class mix made the dominant
		// class carry the highest price — charging prompt more than
		// generated, backwards.) Collect fixedPerM per 1M of each class;
		// at expected volume the fleet collects exactly fixed_today.
		fixedPerM := in.FixedToday / in.ExpectedTokensPerDay * 1e6 * mult
		p.FixedPPPerM = round2(fixedPerM)
		p.FixedTGPerM = round2(fixedPerM)
		p.PromptPerM = round2(p.MarginalPPPerM + p.FixedPPPerM)
		p.OutputPerM = round2(p.MarginalTGPerM + p.FixedTGPerM)
	}
	// Cached: prompt price minus the cache discount (default 75% off).
	p.CachedPerM = round2(p.PromptPerM * (1 - in.CacheDiscountPct/100))
	return p
}

func round2(v float64) float64 { return math.Round(v*100) / 100 } // peakCapacity: fleet throughput ceiling = per-request peak decode/prefill
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
