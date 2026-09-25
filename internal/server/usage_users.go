package server

import (
	"encoding/json"
	"net/http"
)

// handleUsageUsers: GET /usage/users — per-user token accounting (admin only).
// Deliberately NO per-user energy: NVML meters whole-GPU power, so energy is
// a full-system metric (Python parity).
func (s *Server) handleUsageUsers(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.adminIdentity(w, r); !ok {
		return // adminIdentity already wrote 403
	}
	s.usage.Flush()
	allTime, today := s.usage.UsersSnapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"note": "tokens are attributable per user; energy/cost is a full-system " +
			"metric — see /usage. kinds breaks usage down by model type " +
			"(chat / embeddings / fim).",
		"all_time": withTotal(allTime),
		"today":    withTotal(today),
	})
}

func withTotal(in map[string]*UserUsage) map[string]map[string]any {
	out := map[string]map[string]any{}
	for user, u := range in {
		if u == nil {
			continue
		}
		out[user] = map[string]any{
			"requests":      u.Requests,
			"prompt_tokens": u.PromptTokens,
			"output_tokens": u.OutputTokens,
			"total_tokens":  u.PromptTokens + u.OutputTokens,
			"keys":          u.Keys,
			"kinds":         u.Kinds,
		}
	}
	return out
}

// ---- /config + /config/reload (admin; Python parity) ----

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.adminIdentity(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.cfg)
}

func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.adminIdentity(w, r); !ok {
		return
	}
	if nc, changed := s.cfg.PollWatch(); changed {
		s.SetConfig(nc)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "reloaded"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "unchanged"})
}

// handleFavicon: browsers request /favicon.ico unprompted; redirect to the
// embedded SVG.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/static/favicon.svg", http.StatusFound)
}
