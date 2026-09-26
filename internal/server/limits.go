// Per-user daily token quotas (SPEC §7 identity plane). Admin sets a limit
// per user; inference surfaces return 429 once today's prompt+output tokens
// cross it. Admins, the legacy operator key, and LAN-trusted unkeyed
// requests ("local") are exempt — you don't want to lock the operator out
// of their own gateway.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// enforceDailyLimit returns false (and writes 429) when the user is over
// their configured daily token limit. Exempt: admins, operator, local.
func (s *Server) enforceDailyLimit(w http.ResponseWriter, user string) bool {
	if user == "" || user == "local" || user == "operator" {
		return true
	}
	if s.store.RoleOf(user) == "admin" {
		return true
	}
	limit := s.store.DailyLimit(user)
	if limit <= 0 {
		return true // unlimited
	}
	used := s.usage.TodayTokens(user)
	if used < limit {
		return true
	}
	// Over quota: retry after the UTC-day rollover (+1s of slack). Usage is
	// bucketed by UTC date, so the quota window is the UTC day.
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 24, 0, 0, 1, time.UTC)
	retry := int(time.Until(midnight).Seconds()) + 1
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", fmt.Sprint(retry))
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			// resets_at is a full RFC3339 instant (always UTC midnight) so the
			// UI can render the reset in the viewer's local time.
			"message":   fmt.Sprintf("Daily token limit reached (%d of %d). Resets at UTC midnight.", used, limit),
			"type":      "rate_limit_error",
			"limit":     limit,
			"used":      used,
			"resets_at": midnight.UTC().Format(time.RFC3339),
		}})
	return false
}

// userDailyStatus: limit + today's tokens for one user (admin views).
func (s *Server) userDailyStatus(username string) map[string]any {
	limit := s.store.DailyLimit(username)
	used := s.usage.TodayTokens(username)
	out := map[string]any{
		"daily_token_limit": limit, // 0 = unlimited
		"tokens_today":      used,
	}
	if limit > 0 {
		out["pct_of_limit"] = int(float64(used) / float64(limit) * 100)
	}
	return out
}
