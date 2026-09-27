package server

import (
	"encoding/json"
	"net/http"
	"time"

	"llmgateway/internal/embeddedpb"
)

// handleAnalytics: GET /api/analytics?days=N
//
// Drill-down series over the per-request telemetry (raw rows, 14-day
// retention). Returns everything the Analytics page charts in one call:
// traffic, latency percentiles, per-GPU stats, cache reuse paths, and
// the per-user table. Admin session required.
func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok || sess.Role != "admin" {
		errBody(w, 401, "admin only")
		return
	}
	ops := s.Ops()
	if ops == nil {
		errBody(w, 503, "analytics store not attached")
		return
	}
	days := 1
	switch r.URL.Query().Get("days") {
	case "7":
		days = 7
	case "14":
		days = 14
	default:
		days = 1
	}

	// One row per hour: requests, tokens, errors, avg + p95 TTFT, tok/s.
	var hourly []embeddedpb.HourlyRow
	if err := ops.QueryAnalyticsHourly(days, &hourly); err != nil {
		errBody(w, 500, err.Error())
		return
	}

	var perGPU []embeddedpb.GPURow
	if err := ops.QueryAnalyticsPerGPU(days, &perGPU); err != nil {
		errBody(w, 500, err.Error())
		return
	}

	var perUser []embeddedpb.UserRow
	if err := ops.QueryAnalyticsPerUser(days, &perUser); err != nil {
		errBody(w, 500, err.Error())
		return
	}

	var reuse []embeddedpb.ReuseRow
	if err := ops.QueryAnalyticsReuse(days, &reuse); err != nil {
		errBody(w, 500, err.Error())
		return
	}

	out := map[string]any{
		"days":      days,
		"hourly":    hourly,
		"per_gpu":   perGPU,
		"per_user":  perUser,
		"reuse":     reuse,
		"generated": time.Now().UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
