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
	// ?from=&to= (UTC days, inclusive), or the older ?days=1|7|14.
	dr := embeddedpb.LastDays(1)
	switch r.URL.Query().Get("days") {
	case "7":
		dr = embeddedpb.LastDays(7)
	case "14":
		dr = embeddedpb.LastDays(14)
	}
	if f, t := r.URL.Query().Get("from"), r.URL.Query().Get("to"); f != "" || t != "" {
		for _, d := range []string{f, t} {
			if d != "" {
				if _, err := time.Parse("2006-01-02", d); err != nil {
					errBody(w, 400, "dates are YYYY-MM-DD")
					return
				}
			}
		}
		if t == "" {
			t = time.Now().UTC().Format("2006-01-02")
		}
		if f > t {
			f, t = t, f
		}
		dr = embeddedpb.DayRange{From: f, To: t}
	}
	days := dr

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
		"from": dr.From,
		"to":   dr.To,
		// Per-request rows are kept this long; earlier days have no detail.
		"retention_days": AnalyticsRetentionDays,
		"hourly":         hourly,
		"per_gpu":        s.withTTFTPercentiles(ops, days, s.mergePerGPU(perGPU)),
		"per_user":       perUser,
		"reuse":          reuse,
		"generated":      time.Now().UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// mergePerGPU: per-backend analytics rows folded by GPU identity, so one
// engine reached by different URLs over the window (direct, then link)
// is one row. Request rows are distinct, so counts sum; rates are
// recomputed from the sums; speed is request-weighted; worst TTFT is max.
func (s *Server) mergePerGPU(rows []embeddedpb.GPURow) []map[string]any {
	s.mu.Lock()
	hosts := s.cfg.Hosts
	s.mu.Unlock()
	type acc struct {
		id                                      gpuIdentity
		backends                                []string
		requests, input, output, cached, dn, da int64
		tpsWeighted, ttftMax                    float64
		ttftSum, queueSum                       float64
		ttftN, queueN                           int64
		latestLink                              bool
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
		if isLinkURL(r.Backend) {
			a.latestLink = true
		}
		a.backends = append(a.backends, r.Backend)
		a.requests += r.Requests
		a.input += r.Input
		a.output += r.Output
		a.cached += r.Cached
		a.dn += r.DraftN
		a.da += r.DraftAccepted
		a.tpsWeighted += r.TokPerSec * float64(r.Requests)
		a.ttftMax = max(a.ttftMax, r.TTFTMax)
		a.ttftSum += r.TTFTAvg * float64(r.TTFTN)
		a.ttftN += r.TTFTN
		a.queueSum += r.QueueAvg * float64(r.QueueN)
		a.queueN += r.QueueN
	}
	pct := func(n, d int64) float64 {
		if d <= 0 {
			return 0
		}
		return float64(int64(1000*float64(n)/float64(d)+0.5)) / 10
	}
	var total int64
	for _, k := range order {
		total += byKey[k].requests
	}
	avg := func(sum float64, n int64) float64 {
		if n == 0 {
			return 0
		}
		return float64(int64(sum/float64(n) + 0.5))
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		via := a.id.Via
		if a.latestLink {
			via = "link"
		}
		tps := 0.0
		if a.requests > 0 {
			tps = float64(int64(a.tpsWeighted/float64(a.requests) + 0.5))
		}
		out = append(out, map[string]any{
			"gpu_key": a.id.Key, "gpu_label": a.id.Label, "host": a.id.Host, "via": via,
			"backend": a.backends[0], "backends": a.backends,
			"requests": a.requests, "input": a.input, "output": a.output, "cached": a.cached,
			"cache_hit_pct": pct(a.cached, a.input), "tok_per_s": tps, "ttft_max": a.ttftMax,
			"ttft_avg": avg(a.ttftSum, a.ttftN), "queue_avg": avg(a.queueSum, a.queueN),
			"share_pct": pct(a.requests, total),
			"draft_n":   a.dn, "draft_accepted": a.da, "draft_accept_pct": pct(a.da, a.dn),
		})
	}
	return out
}

// withTTFTPercentiles adds a real p95 time-to-first-token per merged GPU
// (over all of its backend URLs).
func (s *Server) withTTFTPercentiles(ops OpsStore, days embeddedpb.DayRange, rows []map[string]any) []map[string]any {
	for _, r := range rows {
		backends, _ := r["backends"].([]string)
		if p, err := ops.QueryTTFTPercentile(days, backends, 0.95); err == nil && p > 0 {
			r["ttft_p95"] = float64(int64(p + 0.5))
		}
	}
	return rows
}

// AnalyticsRetentionDays: how long per-request analytics rows are kept.
const AnalyticsRetentionDays = 14
