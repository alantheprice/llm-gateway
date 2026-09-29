package server

import (
	"encoding/json"
	"net/http"
	"time"
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
			"cached_tokens": u.CachedTokens,
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

// handleUsageRange: GET /api/usage/range?from=YYYY-MM-DD&to=YYYY-MM-DD —
// usage per user over a range of UTC days (both empty = all time), with a
// per-day series for charts. Admins see every user; others only themselves.
func (s *Server) handleUsageRange(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	var only map[string]bool
	if sess.Role != "admin" {
		only = map[string]bool{sess.U: true}
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	for _, d := range []string{from, to} {
		if d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				errBody(w, http.StatusBadRequest, "dates are YYYY-MM-DD")
				return
			}
		}
	}
	if from != "" && to != "" && from > to {
		from, to = to, from
	}
	s.usage.Flush()
	users, days, series := s.usage.Range(from, to, only)
	out := map[string]any{"from": from, "to": to, "days": days, "series": series, "users": users,
		"today": time.Now().UTC().Format("2006-01-02")}
	if from == "" && to == "" {
		// All time: lifetime totals (they include usage from before daily
		// records began); the series covers the recorded days.
		out["users"] = s.usage.LifetimeRange(only)
	}
	// Value at today's price book: what this usage would cost at the
	// operator's list prices. Informational — nobody is charged.
	s.mu.Lock()
	book := s.cfg.PriceBook
	s.mu.Unlock()
	priced := book.PromptUSDPerM > 0 || book.CachedUSDPerM > 0 || book.OutputUSDPerM > 0
	if list, ok := out["users"].([]UserRange); ok && priced {
		for i := range list {
			list[i].ValueUSD = round4(applyBookPrices(book, list[i].Prompt, list[i].Cached, list[i].Output))
		}
	}
	if priced {
		out["price_book"] = book
	}
	var first string
	if all := s.usage.Days(); len(all) > 0 {
		first = all[0]
	}
	out["first_day"] = first
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
