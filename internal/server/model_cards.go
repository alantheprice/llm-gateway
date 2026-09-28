package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Model cards through the gateway (NInfer serving standard, schema
// "ninfer-model-card/1"). Engines serve GET /v1/models/<id> with a
// "model_card" object, /v1/models/<id>/card (card only) and
// /v1/models/<id>/details (card + live host/GPU/load facts). The gateway:
//
//   - answers /v1/models/<pool> with the pool plus each member's card, labelled
//     by GPU identity and path (direct | link) — what clients call is the pool;
//   - proxies /v1/models/<model>[/card|/details] for standalone models, and for
//     pool members to admins (members are hidden from the catalog);
//   - redacts deployment internals (paths, endpoints, accounting) for
//     non-admins; /details (host RAM, kernel, live VRAM) is admin-only.
//
// Cards are cached by the poller (refreshMaxContext), so these views never
// wait on an engine except for /details, which is live by definition.

// cardRedactKeys: card fields describing where and how a deployment runs on
// its host, not what it serves. Hidden from non-admins.
var cardRedactKeys = map[string]bool{
	"artifact_path": true, "endpoint": true, "accounting": true,
	"request_log": true, "metrics_state": true, "device": true,
}

// redactCard returns a copy of v without host-internal fields; any string
// that looks like an absolute host path is replaced.
func redactCard(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if cardRedactKeys[k] {
				continue
			}
			out[k] = redactCard(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactCard(val)
		}
		return out
	case string:
		if strings.Contains(t, "/home/") || strings.HasPrefix(t, "/models/") || strings.HasPrefix(t, "/state/") {
			return "[redacted]"
		}
		return t
	default:
		return v
	}
}

// cardSummary: the handful of facts a person scans first — for tables,
// tooltips and the stats-for-nerds panel. Nil when there is no card.
func cardSummary(card map[string]any) map[string]any {
	if card == nil {
		return nil
	}
	get := func(path ...string) any {
		var cur any = card
		for _, p := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[p]
		}
		return cur
	}
	out := map[string]any{}
	set := func(k string, v any) {
		if v != nil && v != "" {
			out[k] = v
		}
	}
	set("name", get("name"))
	set("source_model", get("source_model", "repository"))
	set("weights", get("quantization", "weights", "format"))
	set("kv_cache", get("quantization", "kv_cache", "dtype"))
	set("speculative", get("serving", "speculative_decoding", "backend"))
	set("draft_tokens", get("serving", "speculative_decoding", "draft_tokens"))
	set("context", get("context", "max_context_len"))
	set("gpu", get("hardware", "gpu"))
	set("vram_mib", get("hardware", "vram_total_mib"))
	set("engine", get("serving", "engine"))
	set("schema", get("schema"))
	return out
}

// engineInfo: the cached card, context window and served id for a backend.
func (s *Server) engineInfo(url string) ctxInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxCtx[url]
}

// handleModelInfo: GET /v1/models/<id>[/card|/details].
func (s *Server) handleModelInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errBody(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.cfg.Gateway.ModelsRequireAuth {
		if _, _, ok := s.checkAuth(w, r); !ok {
			return
		}
	}
	_, _, isAdmin := s.adminIdentity(w, r)

	rest := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	view := ""
	for _, suffix := range []string{"/details", "/card"} {
		if strings.HasSuffix(rest, suffix) {
			view, rest = strings.TrimPrefix(suffix, "/"), strings.TrimSuffix(rest, suffix)
			break
		}
	}
	id := strings.TrimSuffix(rest, "/")
	if id == "" {
		s.handleModels(w, r)
		return
	}
	s.mu.Lock()
	pool, isPool := s.cfg.ModelPools[id]
	s.mu.Unlock()
	if !isPool {
		if backend, _, _ := s.modelBackend(id); backend == "" {
			// A private link: its owner sees it as an admin would (full
			// card, live details); users it is shared with see the
			// redacted card.
			if pm, ok := s.resolvePrivate(s.requestUser(r), id); ok {
				full := isAdmin || !pm.Shared
				if view == "details" && !full {
					errBody(w, http.StatusForbidden, "model details are for the GPU's owner")
					return
				}
				s.writeEngineInfo(w, pm.URL, pm.ModelID, view, full)
				return
			}
		}
	}
	if view == "details" && !isAdmin {
		errBody(w, http.StatusForbidden, "model details are admin-only")
		return
	}
	if isPool {
		s.writePoolInfo(w, id, &pool, view, isAdmin)
		return
	}

	backend, engineID, isMember := s.modelBackend(id)
	if backend == "" || (isMember && !isAdmin) {
		errBody(w, http.StatusNotFound, "model not found")
		return
	}
	s.writeEngineInfo(w, backend, engineID, view, isAdmin)
}

// modelBackend finds the backend serving a model id: pool members first
// (flagging them — hidden from non-admins), then discovered backends.
func (s *Server) modelBackend(id string) (backend, engineID string, isMember bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pool := range s.cfg.ModelPools {
		for _, m := range pool.Members {
			if m.ModelID == id && m.Backend != "" {
				return m.Backend, m.ModelID, true
			}
		}
	}
	for u, info := range s.backends {
		for _, m := range info.Models {
			if m == id {
				return u, id, false
			}
		}
	}
	return "", "", false
}

// writeEngineInfo proxies one engine's model endpoint (over its link when
// linked), redacting for non-admins.
func (s *Server) writeEngineInfo(w http.ResponseWriter, backend, engineID, view string, isAdmin bool) {
	path := "/v1/models/" + engineID
	if view != "" {
		path += "/" + view
	}
	status, body, err := s.backendGet(backend, path, 5*time.Second)
	if err != nil {
		errBody(w, http.StatusBadGateway, "engine unreachable")
		return
	}
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		errBody(w, http.StatusBadGateway, "engine returned invalid JSON")
		return
	}
	if !isAdmin {
		payload = redactCard(payload)
	}
	if m, ok := payload.(map[string]any); ok && view == "" {
		id := s.gpuIdentity(backend)
		m["via"] = id.Via
		if id.Host != "" || isAdmin {
			m["gpu_label"] = id.Label
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}

// writePoolInfo: a pool as clients see it — the virtual model plus what
// actually serves it, member by member.
func (s *Server) writePoolInfo(w http.ResponseWriter, name string, pool *poolCfgT, view string, isAdmin bool) {
	members := make([]map[string]any, 0, len(pool.Members))
	maxCtx := 0
	for i, m := range pool.Members {
		info := s.engineInfo(m.Backend)
		id := s.gpuIdentity(m.Backend)
		entry := map[string]any{
			"gpu_label": id.Label, "gpu_key": id.Key, "via": id.Via,
			"up": !s.tracker.IsDown(m.Backend),
		}
		if id.Host == "" && !isAdmin {
			// Unclaimed GPUs are identified by their backend address; don't
			// expose internal addresses to non-admins.
			entry["gpu_label"] = fmt.Sprintf("member %d", i+1)
			delete(entry, "gpu_key")
		}
		if info.tokens > 0 {
			entry["max_model_len"] = info.tokens
			maxCtx = max(maxCtx, info.tokens)
		}
		switch view {
		case "details": // admin-only (checked by the caller): live per member
			if d, ok := s.backendJSON(m.Backend, "/v1/models/"+m.ModelID+"/details", 5*time.Second); ok {
				entry["details"] = d
			}
		default:
			var card any
			if info.card != nil {
				card = info.card
				if !isAdmin {
					card = redactCard(info.card)
				}
			}
			entry["model_card"] = card
			entry["card_summary"] = cardSummary(info.card)
		}
		if isAdmin {
			entry["model_id"], entry["backend"] = m.ModelID, m.Backend
		}
		members = append(members, entry)
	}
	out := map[string]any{"id": name, "object": "model", "owned_by": "llm-gateway", "pool": true, "members": members}
	if maxCtx > 0 {
		out["max_model_len"] = maxCtx
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
