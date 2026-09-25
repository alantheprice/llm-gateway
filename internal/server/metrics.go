package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"llmgateway/internal/config"
)

// ---- admin-gated metrics views (Python parity: usage/metrics/backends) ----
//
// Python gates these with _admin_required: an admin session cookie OR a
// Bearer/api_key key whose record is a UI key minted with role=admin.
// Everything else → 403 (had a session/key) or 401 (anonymous). LAN trust
// does NOT apply — these reveal energy/cost/system info.

func (s *Server) adminGate(w http.ResponseWriter, r *http.Request) bool {
	if sess, ok := s.sessionFrom(r); ok {
		if sess.Role == "admin" {
			return true
		}
		errBody(w, http.StatusForbidden, "admin only")
		return false
	}
	// Bearer header or ?api_key= (Prometheus scrapes can't always set
	// headers).
	key := bearerKey(r)
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	if key != "" {
		if _, rec, ok := s.store.LookupKey(key); ok {
			if rec.UI && rec.Role == "admin" {
				return true
			}
			// Valid key, not an admin ui key → same 401 Python returns
			// (anonymous from the admin plane's perspective).
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": "Invalid or missing API key", "type": "unauthorized"}})
	return false
}

// metricBackends: pool members + overflow fallbacks + discovered ninfer
// engines (Python's backends set in usage/metrics handlers).
func (s *Server) metricBackends() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[string]bool{}
	for _, pool := range s.cfg.ModelPools {
		for _, m := range pool.Members {
			if m.Backend != "" {
				set[m.Backend] = true
			}
		}
	}
	for _, pair := range s.cfg.OverflowPairs {
		if pair.FallbackBackend != "" {
			set[pair.FallbackBackend] = true
		}
	}
	for u, l := range s.tracker.Snapshot() {
		if l != nil && l.Engine == "ninfer" {
			set[u] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func usageNum(m map[string]any, path ...string) float64 {
	if len(m) == 0 {
		return 0
	}
	node := m
	for i, k := range path {
		v, ok := node[k]
		if !ok {
			return 0
		}
		if i == len(path)-1 {
			if f, ok := v.(float64); ok {
				return f
			}
			return 0
		}
		if node, ok = v.(map[string]any); !ok {
			return 0
		}
	}
	return 0
}

func usageMap(m map[string]any, path ...string) map[string]any {
	node := m
	for _, k := range path {
		v, ok := node[k]
		if !ok {
			return nil
		}
		if node, ok = v.(map[string]any); !ok {
			return nil
		}
	}
	return node
}

// energyRate mirrors Python _energy_rate: prefer the engine's
// cost_per_m_tokens_usd (schema-2 engines bucket tokens correctly); fall
// back to cost/tokens when internally consistent; else nil.
func energyRate(usage map[string]any) any {
	today := usageMap(usage, "energy", "today")
	if today == nil {
		return nil
	}
	if r, ok := today["cost_per_m_tokens_usd"].(float64); ok {
		return round6(r)
	}
	cost, cok := today["cost_usd"].(float64)
	tokens, tok := today["tokens"].(float64)
	if cok && tok && tokens > 0 {
		return round6(cost / tokens * 1e6)
	}
	return nil
}

func round6(v float64) float64 { return float64(int64(v*1e6+0.5)) / 1e6 }

// gatherUsage fetches /usage from each backend concurrently.
func (s *Server) gatherUsage(urls []string) map[string]map[string]any {
	type res struct {
		url     string
		payload map[string]any
	}
	ch := make(chan res, len(urls))
	for _, u := range urls {
		go func(u string) {
			p, ok := getJSON(s.client, u+"/usage", 4*time.Second)
			if ok {
				ch <- res{u, p}
			} else {
				ch <- res{u, nil}
			}
		}(u)
	}
	out := map[string]map[string]any{}
	for range urls {
		r := <-ch
		if r.payload != nil {
			out[r.url] = r.payload
		}
	}
	return out
}

// handleUsageRich: GET /usage — aggregated engine metrics, admin only.
// Python parity: gateway/totals envelope + per-backend reshape + blended
// pool rate.
func (s *Server) handleUsageRich(w http.ResponseWriter, r *http.Request) {
	if !s.adminGate(w, r) {
		return
	}
	urls := s.metricBackends()
	usageByBackend := s.gatherUsage(urls)

	perBackend := map[string]any{}
	for _, url := range urls {
		usage := usageByBackend[url]
		if usage == nil {
			perBackend[url] = map[string]any{"reachable": false, "engine": s.engineOf(url)}
			continue
		}
		perBackend[url] = map[string]any{
			"reachable":             true,
			"model":                 usage["model"],
			"uptime_seconds":        usageMap(usage, "uptime")["seconds"],
			"tokens":                usage["tokens"],
			"throughput":            usage["throughput"],
			"lanes":                 usage["lanes"],
			"health":                usage["health"],
			"energy":                usage["energy"],
			"rate_usd_per_m_tokens": energyRate(usage),
		}
	}

	// Pool-wide blended rate (Python: sum today's cost / tokens).
	var totalCost, totalTokens float64
	for _, u := range usageByBackend {
		totalCost += usageNum(u, "energy", "today", "cost_usd")
		totalTokens += usageNum(u, "energy", "today", "tokens")
	}
	var poolRate any
	if totalTokens > 0 {
		poolRate = round6(totalCost / totalTokens * 1e6)
	}

	var ti, tc, to, kwhT, costT, kwh30, cost30, dec, pre float64
	for _, u := range usageByBackend {
		ti += usageNum(u, "tokens", "input", "total")
		tc += usageNum(u, "tokens", "input", "cache_hits")
		to += usageNum(u, "tokens", "output", "total")
		kwhT += usageNum(u, "energy", "today", "kwh")
		costT += usageNum(u, "energy", "today", "cost_usd")
		kwh30 += usageNum(u, "energy", "rolling_30d", "kwh")
		cost30 += usageNum(u, "energy", "rolling_30d", "cost_usd")
		dec += usageNum(u, "throughput", "decode_tok_per_s")
		pre += usageNum(u, "throughput", "prefill_tok_per_s")
	}

	s.mu.Lock()
	port := s.cfg.Gateway.Port
	poll := s.cfg.Metrics.PollInterval
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"gateway": map[string]any{"port": port, "poll_interval_seconds": poll},
		"totals": map[string]any{
			"tokens_input":                  ti,
			"tokens_cached":                 tc,
			"tokens_output":                 to,
			"energy_kwh_today":              kwhT,
			"energy_cost_usd_today":         costT,
			"energy_kwh_30d":                kwh30,
			"energy_cost_usd_30d":           cost30,
			"decode_tok_per_s":              dec,
			"prefill_tok_per_s":             pre,
			"energy_cost_per_m_tokens_usd":  poolRate,
			"energy_rate_note":              "blended NVML-measured energy cost across pool GPUs; NVML meters the whole GPU, so this is average cost per token today, not marginal cost",
		},
		"backends": perBackend,
	})
}

func (s *Server) engineOf(url string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.tracker.Get(url); l != nil {
		return l.Engine
	}
	return "unknown"
}

// promEmitter writes HELP/TYPE once per metric name per scrape, then samples.
type promEmitter struct {
	lines   []string
	seenTyp map[string]bool
}

func newPromEmitter() *promEmitter { return &promEmitter{seenTyp: map[string]bool{}} }

func (p *promEmitter) line(name, mtype, help string, value any, labels string) {
	if !p.seenTyp[name] {
		p.seenTyp[name] = true
		p.lines = append(p.lines, fmt.Sprintf("# HELP %s %s", name, help))
		p.lines = append(p.lines, fmt.Sprintf("# TYPE %s %s", name, mtype))
	}
	if labels != "" {
		p.lines = append(p.lines, fmt.Sprintf("%s%s %v", name, labels, value))
	} else {
		p.lines = append(p.lines, fmt.Sprintf("%s %v", name, value))
	}
}

// handleMetricsRich: GET /metrics — curated Prometheus exposition (admin).
func (s *Server) handleMetricsRich(w http.ResponseWriter, r *http.Request) {
	if !s.adminGate(w, r) {
		return
	}
	urls := s.metricBackends()
	usageByBackend := s.gatherUsage(urls)
	ordered := make([]string, 0, len(urls))
	for _, u := range urls {
		if usageByBackend[u] != nil {
			ordered = append(ordered, u)
		}
	}
	single := len(ordered) == 1
	p := newPromEmitter()

	type perSeries map[string]float64

	collect := func(paths ...[]string) []perSeries {
		out := make([]perSeries, len(paths))
		for i := range out {
			out[i] = perSeries{}
		}
		for _, url := range ordered {
			u := usageByBackend[url]
			for i, path := range paths {
				out[i][url] = usageNum(u, path...)
			}
		}
		return out
	}

	series := collect(
		[]string{"tokens", "input", "total"},
		[]string{"tokens", "input", "cache_hits"},
		[]string{"tokens", "output", "total"},
		[]string{"throughput", "decode_tok_per_s"},
		[]string{"throughput", "prefill_tok_per_s"},
		[]string{"energy", "today", "kwh"},
		[]string{"energy", "today", "cost_usd"},
		[]string{"lanes", "capacity"},
		[]string{"lanes", "processing"},
		[]string{"lanes", "waiting"},
		[]string{"health", "kv_pressure_spills"},
		[]string{"health", "sessions_evicted"},
	)
	tokIn, tokCached, tokOut, dec, pre, kwh, cost := series[0], series[1], series[2], series[3], series[4], series[5], series[6]
	cap_, run, wait, spills, evict := series[7], series[8], series[9], series[10], series[11]

	tot := func(ps perSeries) float64 {
		var t float64
		for _, v := range ps {
			t += v
		}
		return t
	}
	emit := func(name, mtype, help string, ps perSeries) {
		for _, url := range ordered {
			p.line(name, mtype, help, formatProm(ps[url]), fmt.Sprintf("{ninfer_backend=%q}", url))
		}
		if single {
			p.line(name, mtype, help, formatProm(ps[ordered[0]]), "")
		}
		p.line("gateway:"+strings.SplitN(name, ":", 2)[1], mtype, help, formatProm(tot(ps)), "")
	}

	emit("ninfer:prompt_tokens_total", "counter", "Prompt tokens (cache hits excluded) per NInfer backend.", tokIn)
	emit("ninfer:prompt_cache_tokens_total", "counter", "Prompt tokens served from cache reuse per NInfer backend.", tokCached)
	emit("ninfer:tokens_predicted_total", "counter", "Output tokens committed per NInfer backend.", tokOut)
	emit("ninfer:decode_tokens_per_second", "gauge", "Recent-window decode throughput per NInfer backend (0 when idle).", dec)
	emit("ninfer:prefill_tokens_per_second", "gauge", "Recent-window prefill throughput per NInfer backend (0 when idle).", pre)
	emit("ninfer:energy_kwh_today", "gauge", "Energy consumed today (kWh) per NInfer backend.", kwh)
	emit("ninfer:energy_cost_usd_today", "gauge", "Energy cost today (USD) per NInfer backend.", cost)
	emit("ninfer:lane_capacity", "gauge", "Decoding lane capacity per NInfer backend.", cap_)
	emit("ninfer:lanes_processing", "gauge", "Requests currently processing per NInfer backend.", run)
	emit("ninfer:lanes_waiting", "gauge", "Requests waiting in the admission queue per NInfer backend.", wait)
	emit("ninfer:kv_pressure_spills_total", "counter", "KV pages spilled under pressure per NInfer backend.", spills)
	emit("ninfer:sessions_evicted_total", "counter", "Sessions evicted under KV pressure per NInfer backend.", evict)

	// Gateway routing observability.
	s.mu.Lock()
	for _, poolName := range sortedStrKeys(s.cfg.ModelPools) {
		pool := s.cfg.ModelPools[poolName]
		th := pool.OverflowThreshold
		if th == 0 {
			th = 0.20
		}
		p.line("gateway:pool_overflow_threshold", "gauge",
			"Load score at which pool routing skips a member.",
			formatProm(th), fmt.Sprintf("{pool=%q}", poolName))
		p.line("gateway:pool_members", "gauge",
			"Configured members per balancing pool.",
			len(pool.Members), fmt.Sprintf("{pool=%q}", poolName))
	}
	loadURLs := make([]string, 0, len(s.backends))
	for u := range s.backends {
		loadURLs = append(loadURLs, u)
	}
	sort.Strings(loadURLs)
	scores := map[string]float64{}
	for _, u := range loadURLs {
		scores[u] = s.tracker.Score(u)
	}
	s.mu.Unlock()
	for _, u := range loadURLs {
		p.line("gateway:backend_load_score", "gauge",
			"Current routing load score per backend.",
			formatProm(round4(scores[u])), fmt.Sprintf("{ninfer_backend=%q}", u))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(strings.Join(p.lines, "\n") + "\n"))
}

func sortedStrKeys(m map[string]config.PoolCfg) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// formatProm renders floats without Go's float64fmt artifacts (1e+06 etc.)
// and ints plainly. NaN/Inf → 0 (Prometheus-safe).
func formatProm(v float64) string {
	if v != v || v > 1e308 || v < -1e308 {
		return "0"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

func round4(v float64) float64 { return float64(int64(v*1e4+0.5)) / 1e4 }

// handleBackendsRich: GET /backends — live routing state per backend (admin).
// Python parity: models per backend, score, engine, staleness, lane/queue
// fields, throughput, KV health, energy, cache hit.
func (s *Server) handleBackendsRich(w http.ResponseWriter, r *http.Request) {
	if !s.adminGate(w, r) {
		return
	}
	s.PollOnce()
	type entry struct {
		URL    string
		Models []string
	}
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	infoByURL := map[string]*BackendInfo{}
	for u, info := range s.backends {
		urls = append(urls, u)
		infoByURL[u] = info
	}
	sort.Strings(urls)
	stale := s.cfg.Metrics.StaleThreshold
	poll := s.cfg.Metrics.PollInterval
	overflow := s.cfg.OverflowPairs
	now := time.Now()
	out := []map[string]any{}
	for _, u := range urls {
		info := infoByURL[u]
		e := map[string]any{"url": u, "models": info.Models}
		l := s.tracker.Get(u)
		if l == nil {
			e["engine"] = "unknown"
			e["metrics_stale"] = true
			out = append(out, e)
			continue
		}
		age := now.Sub(l.LastUpdated).Seconds()
		e["load_score"] = round3(s.tracker.Score(u))
		e["engine"] = l.Engine
		e["metrics_age_seconds"] = round3(age)
		e["metrics_stale"] = age > float64(stale)
		e["running"] = l.Running
		e["waiting"] = l.Waiting
		e["lanes"] = l.Lanes
		e["max_seqs"] = l.MaxSeqs
		e["decode_tps"] = round3(l.DecodeTPS)
		e["prefill_tps"] = round3(l.PrefillTPS)
		e["spills_total"] = l.SpillsTotal
		e["evictions_total"] = l.EvictionsTotal
		if l.KVUsage > 0 {
			e["kv_usage"] = l.KVUsage
		}
		e["energy_daily_kwh"] = round3(l.EnergyKWH)
		e["cache_hit_rate_pct"] = round3(l.CacheHitPct)
		// Extras from the last raw /usage payload (uptime, lane breakdown,
		// KV pages, window freshness).
		if raw := s.lastMetrics[u]; raw != nil {
			if up, ok := raw["uptime"].(map[string]any); ok {
				if secs, ok := up["seconds"].(float64); ok {
					e["uptime_seconds"] = round3(secs)
				}
			}
			if lanes := usageMap(raw, "lanes"); lanes != nil {
				for _, f := range []string{"prefilling", "decoding", "restoring_checkpoints", "kv_pages_occupied", "capacity"} {
					if v, ok := lanes[f].(float64); ok {
						e[f] = int(v)
					}
				}
			}
			if health := usageMap(raw, "health"); health != nil {
				if v, ok := health["kv_pages_occupied"].(float64); ok {
					e["kv_pages_occupied"] = int(v)
				}
			}
			if thr := usageMap(raw, "throughput"); thr != nil {
				if v, ok := thr["window_active"].(bool); ok {
					e["window_fresh"] = v
				}
			}
		}
		out = append(out, e)
	}
	overflowOut := map[string]any{}
	for k, v := range overflow {
		overflowOut[k] = v
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"backends":                 out,
		"poll_interval_seconds":    poll,
		"stale_threshold_seconds":  stale,
		"overflow_pairs":           overflowOut,
	})
}
