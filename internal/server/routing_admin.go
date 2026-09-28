package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"llmgateway/internal/embeddedpb"
	"llmgateway/internal/link"
	"llmgateway/internal/routing"
)

// Routing admin: one page that shows every name a client can call and what
// the gateway does with it, in the order handleChat resolves it — shared
// pool, overflow pair, a single engine — plus users' private links, the
// engines an admin can route to, and problems worth fixing. The page edits
// pools and overflow through POST /admin/config (same validation and save
// path as the config page); the dry run uses the live pickers without
// side effects.

// engineView: one engine (direct backend or live link) as the page shows it.
type engineView struct {
	URL        string   `json:"url"`
	Models     []string `json:"models"`
	GPULabel   string   `json:"gpu_label"`
	Via        string   `json:"via"` // direct | link
	Engine     string   `json:"engine,omitempty"`
	Known      bool     `json:"known"` // discovered or a live link
	Up         bool     `json:"up"`
	Polled     bool     `json:"polled"`
	Running    int      `json:"running"`
	Waiting    int      `json:"waiting"`
	Lanes      int      `json:"lanes"`
	Score      float64  `json:"score"`
	QueueMs    float64  `json:"queue_ms"`
	MaxContext int      `json:"max_context"`
	Requests   int64    `json:"requests_today"`
	Owner      string   `json:"owner,omitempty"` // link owner
}

type routeMember struct {
	engineView
	ModelID        string  `json:"model_id"`
	CapacityWeight int     `json:"capacity_weight"`
	MaxContextCfg  int     `json:"max_context_override"`
	Consent        string  `json:"consent"` // n/a | admin-owned | consented | not-consented | paused
	Serving        bool    `json:"serving"` // eligible for this pool right now
	SharePct       float64 `json:"share_pct"`
}

type routeEntry struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"` // pool | overflow | direct | gpu-direct
	ModelKind string         `json:"model_kind"`
	Summary   string         `json:"summary"`
	Pool      map[string]any `json:"pool,omitempty"`
	Members   []routeMember  `json:"members,omitempty"`
	Primary   *engineView    `json:"primary,omitempty"`
	Fallback  *routeMember   `json:"fallback,omitempty"`
	Threshold float64        `json:"threshold,omitempty"`
	Engine    *engineView    `json:"engine,omitempty"`
	Requests  int64          `json:"requests_today"`
}

type routeProblem struct {
	Severity string `json:"severity"` // error | warning | info
	Name     string `json:"name,omitempty"`
	Text     string `json:"text"`
}

func (s *Server) handleRoutingPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok || sess.Role != "admin" {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	s.renderPage(w, r, "routing.html", "admin_routing", "Routing")
}

// requestsToday: successful requests per backend this UTC day.
func (s *Server) requestsToday() map[string]int64 {
	out := map[string]int64{}
	ops := s.Ops()
	if ops == nil {
		return out
	}
	var rows []embeddedpb.BackendTokens
	if ops.QueryBackendTokensDay(time.Now().UTC().Format("2006-01-02"), &rows) == nil {
		for _, r := range rows {
			out[r.Backend] = r.Requests
		}
	}
	return out
}

// engineInfoView: live view of one backend URL.
func (s *Server) engineInfoView(url string, reqs map[string]int64) engineView {
	id := s.gpuIdentity(url)
	ev := engineView{URL: url, GPULabel: id.Label, Via: id.Via, Up: !s.tracker.IsDown(url),
		Score: round3(s.tracker.Score(url)), QueueMs: math.Round(s.tracker.QueueWait(url)),
		MaxContext: s.maxContext(url), Requests: reqs[url]}
	if ev.Via == "" {
		ev.Via = "direct"
	}
	if isLinkURL(url) {
		if c := s.linkReg.Lookup(url); c != nil {
			ev.Known, ev.Owner = true, c.Owner
			for _, e := range c.Engines() {
				if link.VirtualURL(c.Agent, e.Port) == url {
					ev.Models = []string{e.ModelID}
				}
			}
		}
	} else {
		s.mu.Lock()
		if info, ok := s.backends[url]; ok {
			ev.Known, ev.Models, ev.Engine = true, append([]string(nil), info.Models...), info.Engine
		}
		s.mu.Unlock()
	}
	if l := s.tracker.Get(url); l != nil {
		ev.Polled, ev.Running, ev.Waiting, ev.Lanes = true, l.Running, l.Waiting, l.Lanes
		if ev.Engine == "" {
			ev.Engine = l.Engine
		}
	}
	if !ev.Known {
		ev.Up = false
	}
	return ev
}

// linkConsent: how a pool may use a link member (see linkServesPool).
func (s *Server) linkConsent(url, pool string) string {
	if !isLinkURL(url) {
		return "n/a"
	}
	c := s.linkReg.Lookup(url)
	if c == nil || c.Owner == "" {
		return "n/a"
	}
	ls := s.store.LinkSettingsOf(c.Owner, c.KeyID)
	switch {
	case ls.Paused:
		return "paused"
	case s.store.RoleOf(c.Owner) == "admin":
		return "admin-owned"
	case slices.Contains(ls.Pools, pool):
		return "consented"
	}
	return "not-consented"
}

// allEngines: every backend the gateway can route to — discovered engines
// and live links — for the editor's picker.
func (s *Server) allEngines(reqs map[string]int64) []engineView {
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	for u := range s.backends {
		urls = append(urls, u)
	}
	s.mu.Unlock()
	urls = append(urls, s.linkReg.LiveURLs()...)
	urls = dedupeStrings(urls)
	sort.Strings(urls)
	out := make([]engineView, 0, len(urls))
	for _, u := range urls {
		out = append(out, s.engineInfoView(u, reqs))
	}
	return out
}

// routeMap builds the page's model: entries in resolution order, private
// links, engines, problems, scoring knobs.
func (s *Server) routeMap() map[string]any {
	reqs := s.requestsToday()
	s.mu.Lock()
	pools := map[string]poolCfgT{}
	for k, v := range s.cfg.ModelPools {
		pools[k] = v
	}
	pairs := s.cfg.OverflowPairs
	metrics := s.cfg.Metrics
	publicModels := append([]string(nil), s.cfg.PublicModels...)
	discovered := map[string][]string{} // model id -> backend URLs
	for u, info := range s.backends {
		for _, m := range info.Models {
			discovered[m] = append(discovered[m], u)
		}
	}
	s.mu.Unlock()

	var entries []routeEntry
	var problems []routeProblem
	memberIDs := map[string]string{} // member model id -> pool
	covered := map[string]bool{}

	// 1. Shared pools.
	poolNames := make([]string, 0, len(pools))
	for n := range pools {
		poolNames = append(poolNames, n)
	}
	sort.Strings(poolNames)
	for _, name := range poolNames {
		p := pools[name]
		covered[name] = true
		e := routeEntry{Name: name, Kind: "pool", ModelKind: kindFor(name),
			Pool: map[string]any{
				"cache_affinity": p.CacheAffinity, "max_pool_share": p.MaxPoolShare,
				"overflow_threshold": p.OverflowThreshold, "sticky_bias": p.StickyBias, "capacity_bias": p.CapacityBias,
			}}
		var total int64
		for _, m := range p.Members {
			total += reqs[m.Backend]
		}
		serving := 0
		for _, m := range p.Members {
			memberIDs[m.ModelID] = name
			ev := s.engineInfoView(m.Backend, reqs)
			rm := routeMember{engineView: ev, ModelID: m.ModelID, CapacityWeight: m.CapacityWeight,
				MaxContextCfg: m.MaxContext, Consent: s.linkConsent(m.Backend, name)}
			rm.Serving = ev.Known && ev.Up && s.linkServesPool(m.Backend, name)
			if total > 0 {
				rm.SharePct = math.Round(float64(reqs[m.Backend])/float64(total)*1000) / 10
			}
			if rm.Serving {
				serving++
			}
			e.Members = append(e.Members, rm)
			e.Requests += reqs[m.Backend]
			label := ev.GPULabel
			switch {
			case !ev.Known && isLinkURL(m.Backend):
				problems = append(problems, routeProblem{"warning", name, label + " is listed but its link agent isn't connected."})
			case !ev.Known:
				problems = append(problems, routeProblem{"warning", name, label + " (" + m.Backend + ") is listed but the gateway hasn't found an engine there."})
			case !ev.Up:
				problems = append(problems, routeProblem{"warning", name, label + " is down."})
			case len(ev.Models) > 0 && !slices.Contains(ev.Models, m.ModelID):
				problems = append(problems, routeProblem{"error", name, fmt.Sprintf("%s is listed with model %q, but that engine serves %s. Requests routed there will fail.",
					label, m.ModelID, strings.Join(ev.Models, ", "))})
			}
			if rm.Consent == "not-consented" {
				problems = append(problems, routeProblem{"info", name, label + " is listed, but its owner " + ev.Owner + " hasn't agreed to serve this model, so it gets no traffic."})
			}
			if rm.Consent == "paused" {
				problems = append(problems, routeProblem{"info", name, label + " is paused by its owner " + ev.Owner + "."})
			}
		}
		e.Summary = fmt.Sprintf("%d of %d GPUs serving", serving, len(p.Members))
		if serving == 0 {
			problems = append(problems, routeProblem{"error", name, "No GPU can serve " + name + " right now; requests get a 503."})
		}
		if p.LargePromptTokens > 0 {
			problems = append(problems, routeProblem{"info", name, "large_prompt_tokens is set but no longer does anything."})
		}
		entries = append(entries, e)
	}

	// 2. Overflow pairs (checked after pools, before direct engines).
	pairNames := make([]string, 0, len(pairs))
	for n := range pairs {
		pairNames = append(pairNames, n)
	}
	sort.Strings(pairNames)
	for _, name := range pairNames {
		pr := pairs[name]
		if covered[name] {
			problems = append(problems, routeProblem{"warning", name, "Overflow for " + name + " never applies: a pool with the same name takes these requests first."})
			continue
		}
		covered[name] = true
		e := routeEntry{Name: name, Kind: "overflow", ModelKind: kindFor(name), Threshold: pr.Threshold}
		if urls := discovered[name]; len(urls) > 0 {
			pv := s.engineInfoView(urls[0], reqs)
			e.Primary = &pv
			e.Requests += reqs[urls[0]]
		} else {
			problems = append(problems, routeProblem{"error", name, "No engine serves " + name + " directly, so overflow has nothing to spill from; requests get a 404."})
		}
		fv := s.engineInfoView(pr.FallbackBackend, reqs)
		fb := routeMember{engineView: fv, ModelID: pr.FallbackModelID, Consent: s.linkConsent(pr.FallbackBackend, name)}
		fb.Serving = fv.Known && fv.Up && s.linkServesPool(pr.FallbackBackend, name)
		e.Fallback = &fb
		if !fb.Serving {
			problems = append(problems, routeProblem{"warning", name, "Overflow target " + fv.GPULabel + " can't take traffic right now."})
		}
		e.Summary = fmt.Sprintf("spills to %s when %s is at least %.0f%% busy", fv.GPULabel,
			func() string {
				if e.Primary != nil {
					return e.Primary.GPULabel
				}
				return "the primary"
			}(), pr.Threshold*100)
		entries = append(entries, e)
	}

	// 3. Single engines callable by their own model id.
	ids := make([]string, 0, len(discovered))
	for id := range discovered {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if covered[id] {
			continue
		}
		ev := s.engineInfoView(discovered[id][0], reqs)
		e := routeEntry{Name: id, Kind: "direct", ModelKind: kindFor(id), Engine: &ev, Requests: reqs[ev.URL]}
		e.Summary = "one engine, " + ev.GPULabel
		if pool, ok := memberIDs[id]; ok {
			e.Kind = "gpu-direct"
			e.Summary = "one GPU of " + pool + ", called directly: skips the pool's load balancing"
		}
		entries = append(entries, e)
	}

	if len(publicModels) > 0 {
		problems = append(problems, routeProblem{"info", "", "public_models is set but ignored; what clients see comes from pools and engines."})
	}

	// 4. Private links (owners' GPUs).
	var private []map[string]any
	for _, c := range s.linkReg.Conns() {
		if c.Owner == "" {
			continue
		}
		ls := s.store.LinkSettingsOf(c.Owner, c.KeyID)
		sharing := "only the owner"
		switch {
		case ls.SharedAll:
			sharing = "everyone"
		case len(ls.SharedWith) > 0:
			sharing = fmt.Sprintf("%d people", len(ls.SharedWith))
		}
		for _, en := range c.Engines() {
			u := link.VirtualURL(c.Agent, en.Port)
			private = append(private, map[string]any{
				"name": privateModelName(c.Agent, en.ModelID), "owner": c.Owner, "sharing": sharing,
				"paused": ls.Paused, "pools": ls.Pools, "engine": s.engineInfoView(u, reqs),
			})
		}
	}
	sort.Slice(private, func(i, j int) bool { return private[i]["name"].(string) < private[j]["name"].(string) })

	sevRank := map[string]int{"error": 0, "warning": 1, "info": 2}
	sort.SliceStable(problems, func(i, j int) bool { return sevRank[problems[i].Severity] < sevRank[problems[j].Severity] })
	if problems == nil {
		problems = []routeProblem{}
	}
	return map[string]any{
		"entries":  entries,
		"private":  private,
		"engines":  s.allEngines(reqs),
		"problems": problems,
		"scoring": map[string]any{
			"ninfer_lane_weight": metrics.NinferLaneWeight, "ninfer_queue_weight": metrics.NinferQueueWeight,
			"ninfer_pressure_weight": metrics.NinferPressureWt, "stale_threshold": metrics.StaleThreshold,
			"poll_interval": metrics.PollInterval, "default_max_seqs": metrics.DefaultMaxSeqs,
		},
		"generated": time.Now().UTC(),
	}
}

func (s *Server) handleRoutingAPI(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.adminIdentity(w, r); !ok {
		errBody(w, http.StatusForbidden, "admin only")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.routeMap())
}

// dryRun: where a request would go right now, and why — the same steps
// handleChat takes, with no dispatch and no state changes. New conversation
// (no cache-affinity lookup), no session pin.
func (s *Server) dryRun(model string, promptTokens, outputTokens int) map[string]any {
	out := map[string]any{"model": model, "prompt_tokens": promptTokens, "output_tokens": outputTokens}
	steps := []string{}
	need := promptTokens + outputTokens

	s.mu.Lock()
	pool, isPool := s.cfg.ModelPools[model]
	pair, hasPair := s.cfg.OverflowPairs[model]
	leader := s.leader[model]
	s.mu.Unlock()

	if isPool {
		steps = append(steps, model+" is a shared pool of "+fmt.Sprint(len(pool.Members))+" GPUs.")
		all := []map[string]any{}
		eligible := s.poolMembers(model, &pool, need)
		elig := map[string]bool{}
		for _, m := range eligible {
			elig[m.URL] = true
		}
		for _, m := range pool.Members {
			reason := "eligible"
			switch {
			case !s.linkServesPool(m.Backend, model):
				reason = "owner hasn't agreed to serve this model"
			case s.tracker.IsDown(m.Backend):
				reason = "down"
			case !elig[m.Backend]:
				reason = fmt.Sprintf("context window too small for %d tokens", need)
			}
			all = append(all, map[string]any{"gpu_label": s.gpuIdentity(m.Backend).Label, "backend": m.Backend,
				"score": round3(s.tracker.Score(m.Backend)), "status": reason})
		}
		out["candidates"] = all
		if len(eligible) == 0 {
			steps = append(steps, "No GPU is eligible, so the request gets a 503.")
			out["result"] = "503"
			out["steps"] = steps
			return out
		}
		if pool.CacheAffinity {
			steps = append(steps, "Cache affinity is on: a follow-up message returns to the GPU holding its conversation. This test is a new conversation.")
		}
		if pool.MaxPoolShare > 0 {
			scores := map[string]float64{}
			for _, m := range eligible {
				scores[m.URL] = s.tracker.Score(m.URL)
			}
			if u := routing.PickNewConversation(s.dayShare, pool.MaxPoolShare, eligible, scores); u != "" {
				steps = append(steps, fmt.Sprintf("Fairness cap (%.0f%% per GPU) picks the least-loaded GPU under its share.", pool.MaxPoolShare*100))
				out["pick"] = s.gpuIdentity(u).Label
				out["pick_backend"] = u
				out["result"] = "routed"
				out["steps"] = steps
				return out
			}
		}
		pick := routing.PickPool(model, pool.OverflowThreshold, pool.StickyBias, pool.CapacityBias,
			eligible, s.tracker, "", leader, pool.CacheAffinity)
		steps = append(steps, "Scored by load (lower is freer); ties go to the GPU with more capacity.")
		out["pick"] = s.gpuIdentity(pick.URL).Label
		out["pick_backend"] = pick.URL
		out["pick_model_id"] = pick.ModelID
		out["result"] = "routed"
		out["steps"] = steps
		return out
	}
	if hasPair {
		url, _ := s.resolve(model)
		score := s.tracker.Score(url)
		if url != "" && score < pair.Threshold {
			steps = append(steps, fmt.Sprintf("%s has headroom (load %.0f%%, spills at %.0f%%), so it serves directly.",
				s.gpuIdentity(url).Label, score*100, pair.Threshold*100))
			out["pick"], out["pick_backend"], out["result"] = s.gpuIdentity(url).Label, url, "routed"
		} else if !s.linkServesPool(pair.FallbackBackend, model) {
			steps = append(steps, "The primary is busy, but the overflow GPU's owner hasn't agreed to serve this model; it stays on the primary.")
			out["pick"], out["pick_backend"], out["result"] = s.gpuIdentity(url).Label, url, "routed"
		} else {
			steps = append(steps, fmt.Sprintf("The primary is busy (load %.0f%%, spills at %.0f%%), so it spills to %s.",
				score*100, pair.Threshold*100, s.gpuIdentity(pair.FallbackBackend).Label))
			out["pick"], out["pick_backend"], out["result"] = s.gpuIdentity(pair.FallbackBackend).Label, pair.FallbackBackend, "routed"
		}
		out["steps"] = steps
		return out
	}
	if url, _ := s.resolve(model); url != "" {
		steps = append(steps, model+" is served by one engine.")
		out["pick"], out["pick_backend"], out["result"] = s.gpuIdentity(url).Label, url, "routed"
		out["steps"] = steps
		return out
	}
	if strings.Contains(model, "/") {
		steps = append(steps, "Not a platform model. If it's someone's private GPU, only its owner and the people it's shared with can call it.")
	} else {
		steps = append(steps, "No pool, overflow pair or engine serves this name.")
	}
	out["result"] = "404"
	out["steps"] = steps
	return out
}

func (s *Server) handleRoutingTest(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.adminIdentity(w, r); !ok {
		errBody(w, http.StatusForbidden, "admin only")
		return
	}
	if r.Method != http.MethodPost {
		errBody(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		Model        string `json:"model"`
		PromptTokens int    `json:"prompt_tokens"`
		OutputTokens int    `json:"output_tokens"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || strings.TrimSpace(body.Model) == "" {
		errBody(w, http.StatusBadRequest, "model required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.dryRun(strings.TrimSpace(body.Model), max(body.PromptTokens, 0), max(body.OutputTokens, 0)))
}
