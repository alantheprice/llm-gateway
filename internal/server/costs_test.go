package server

import (
	"math"
	"testing"
	"time"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDayCapital(t *testing.T) {
	if !almost(dayCapital(4380, 3), 4380/(3*365.25)) {
		t.Fatal("straight-line daily capital wrong")
	}
	if dayCapital(0, 3) != 0 || dayCapital(1000, 0) != 0 {
		t.Fatal("zero hardware/years must give zero capital")
	}
}

func TestCapitalState(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	// Purchased exactly 365 days before `now` — same wall-clock time, so
	// no timezone/parse skew. 2025-09-25→2026-09-25 spans 365 days
	// (no Feb 29 in between).
	purchased := now.AddDate(-1, 0, 0).Format("2006-01-02")
	paid, daily, monthsElapsed, monthsLeft, fully := capitalState(4380, 3, purchased, now)
	if !almost(daily, 4380/(3*365.25)) {
		t.Fatalf("daily = %v", daily)
	}
	// paid = daily × days-since-purchase, ± sub-cent skew (purchased parses
	// to midnight; DST transitions shift wall-clock hours).
	wantPaid := daily * (365.0 + 0.5) // 365 days + 12h to noon
	if math.Abs(paid-wantPaid) > 0.01 {
		t.Fatalf("paid = %v, want ~%v", paid, wantPaid)
	}
	if fully {
		t.Fatal("not yet fully amortized")
	}
	if monthsElapsed <= 11.9 || monthsElapsed >= 12.1 {
		t.Fatalf("monthsElapsed = %v", monthsElapsed)
	}
	if monthsLeft >= 24.1 || monthsLeft <= 23.9 {
		t.Fatalf("monthsLeft = %v", monthsLeft)
	}
	// Purchased 5 years ago with 3y term → capped, fully amortized.
	old := now.AddDate(-5, 0, 0).Format("2006-01-02")
	paid, _, _, _, fully = capitalState(4380, 3, old, now)
	if !almost(paid, 4380) || !fully {
		t.Fatalf("capped accrued = %v fully=%v", paid, fully)
	}
}

func TestComputeCostsAllIn(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) // 12h elapsed (UTC day)
	hosts := []HostConfig{{
		Label: "box1", IPs: []string{"10.0.0.1"},
		OverheadWatts: 100, HardwareUSD: 4380, AmortizeYears: 3,
	}}
	p := CostsParams{
		Hosts: hosts, RateUSDPerKwh: 0.125, Now: now,
		GPUKwhToday:  []float64{6.0},
		GPUCostToday: []float64{0.75},
		GPUCost30d:   []float64{22.5},
		TokensToday:  []float64{200_000_000},
	}
	out := ComputeCosts(p)
	h := out[0]
	// overhead: 100W × 12h = 1.2 kWh = $0.15
	if !almost(h.OverheadKwhToday, 1.2) || !almost(h.OverheadCostToday, 0.15) {
		t.Fatalf("overhead kwh=%v cost=%v", h.OverheadKwhToday, h.OverheadCostToday)
	}
	// capital daily = 4380/(3*365.25) = 3.9973...
	wantDaily := 4380 / (3 * 365.25)
	if !almost(h.CapitalToday, wantDaily) {
		t.Fatalf("capital today = %v want %v", h.CapitalToday, wantDaily)
	}
	if !almost(h.TotalToday, 0.75+0.15+wantDaily) {
		t.Fatalf("total = %v", h.TotalToday)
	}
	// all-in $/M = total / 200M × 1e6 = total/200
	wantPerM := math.Round((0.75+0.15+wantDaily)/200*100) / 100
	got, ok := h.AllInPerM.(float64)
	if !ok || math.Abs(got-wantPerM) > 1e-9 {
		t.Fatalf("all-in per M = %v want %v", h.AllInPerM, wantPerM)
	}
	// 30d: gpu 22.5 + overhead 100W*24h*30*0.125=9 + capital 30*daily
	if !almost(h.Total30d, 22.5+9+wantDaily*30) {
		t.Fatalf("30d total = %v", h.Total30d)
	}
}

func TestComputeCostsSchedule(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	hosts := []HostConfig{{
		Label: "box1", IPs: []string{"10.0.0.1"},
		OverheadWatts: 100, HardwareUSD: 1200, AmortizeYears: 2,
	}}
	p := CostsParams{
		Hosts: hosts, RateUSDPerKwh: 0.125, Now: now,
		GPUKwhToday: []float64{1}, GPUCostToday: []float64{0.125},
		TokensToday: []float64{1_000_000},
	}
	h := ComputeCosts(p)[0]
	// 2y term → 24 rows cap is 48; expect ceil(months left)=24 rows.
	if len(h.Schedule) != 24 {
		t.Fatalf("schedule rows = %d, want 24", len(h.Schedule))
	}
	// Cumulative paid reaches hardware cost at the end.
	last := h.Schedule[len(h.Schedule)-1]
	if math.Abs(last.CumPaid-1200) > 1.0 { // monthly tranches of daily*30.4375 overshoot slightly, clamped
		t.Fatalf("final cumulative = %v, want ~1200", last.CumPaid)
	}
	if last.CumPaid > 1200+0.005 {
		t.Fatalf("cumulative overshoots hardware: %v", last.CumPaid)
	}
	// Energy estimate = today's (gpu+overhead) × 30
	wantEnergy := (0.125 + 0.125*0.0 + (100.0/1000)*12*0.125) * 30
	if math.Abs(h.Schedule[0].EnergyEst-wantEnergy) > 0.01 {
		t.Fatalf("energy est = %v want %v", h.Schedule[0].EnergyEst, wantEnergy)
	}
}

func TestBackendHostIP(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8000":    "127.0.0.1",
		"http://192.168.1.20:8006": "192.168.1.20",
		"192.168.1.20:8006":        "192.168.1.20",
	}
	for in, want := range cases {
		if got := backendHostIP(in); got != want {
			t.Fatalf("backendHostIP(%q) = %q, want %q", in, got, want)
		}
	}
}
