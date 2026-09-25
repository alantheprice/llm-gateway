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
	HardwareUSD   float64  `json:"hardware_cost_usd"`
	Purchased     string   `json:"purchased,omitempty"`
	AmortizeYears float64  `json:"amortize_years,omitempty"`
	DailyCapital  float64  `json:"capital_usd_per_day"`
	CapitalToday  float64  `json:"capital_usd_today"`
	Capital30d    float64  `json:"capital_usd_30d"`
	CapitalPaid   float64  `json:"capital_accrued_usd"` // capped at HardwareUSD
	MonthsElapsed float64  `json:"months_elapsed"`
	MonthsLeft    float64  `json:"months_remaining"`
	FullyAmort    bool     `json:"fully_amortized"`
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
	Hosts           []HostConfig
	RateUSDPerKwh   float64
	Now             time.Time
	// Per-host engine facts, keyed by host index (already matched).
	GPUKwhToday     []float64
	GPUCostToday    []float64
	GPUKwh30d       []float64
	GPUCost30d      []float64
	TokensToday     []float64
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
			"total_cost_usd_today":        math.Round(totalToday*100) / 100,
			"total_cost_usd_30d":          math.Round(total30d*100) / 100,
			"tokens_today":                tokens,
			"all_in_usd_per_m_tokens":     fleetPerM,
			"projected_monthly_usd":       math.Round(totalToday*30*100) / 100,
			"gpu_only_usd_per_m_tokens":   gpuOnlyPerM(hostCosts, tokens),
		},
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
