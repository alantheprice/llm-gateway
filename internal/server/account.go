package server

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"llmgateway/internal/auth"
)

// ---- /me, /account/update, /chat/password, /api/usage/me, /api/usage/history ----

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// handleMe: GET /me — the logged-in user's PB profile (name, email).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, 401, "login required")
		return
	}
	rec, err := s.pb.FindUser(sess.U)
	if err != nil {
		errBody(w, 503, "PocketBase unreachable")
		return
	}
	if rec == nil {
		errBody(w, 404, "no such user")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"username": rec.Username, "email": rec.Email, "name": rec.Name,
	})
}

// handleAccountUpdate: POST /account/update {name, email, password} —
// PB profile edit, self-authorized with the current password (Python parity).
func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, 401, "login required")
		return
	}
	var body struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errBody(w, 400, "bad json")
		return
	}
	if body.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "password required to confirm changes", "code": "password_required"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email != "" && !emailRE.MatchString(email) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid email address", "code": "bad_email"})
		return
	}
	// Self-authorize: verify current password via PB auth.
	rec, err := s.pb.Authenticate(sess.U, body.Password)
	if err != nil || rec == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "current password incorrect", "code": "wrong_current_password"})
		return
	}
	fields := map[string]any{}
	if body.Name != "" {
		fields["name"] = strings.TrimSpace(body.Name)
	}
	if email != "" {
		fields["email"] = email
	}
	if len(fields) > 0 {
		if err := s.pb.PatchUser(rec.ID, fields); err != nil {
			errBody(w, 400, pbErrMsg(err))
			return
		}
	}
	log.Printf("Account updated: %s", sess.U)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleChatPassword: POST /chat/password {old_password, new_password} —
// PB password change; clears the mcp fence and reissues the session.
func (s *Server) handleChatPassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, 401, "login required")
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errBody(w, 400, "bad json")
		return
	}
	if len(body.NewPassword) < 8 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "new password must be at least 8 characters", "code": "weak_password"})
		return
	}
	rec, err := s.pb.Authenticate(sess.U, body.OldPassword)
	if err != nil || rec == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "current password incorrect", "code": "wrong_current_password"})
		return
	}
	// PB requires oldPassword when a user changes their own password.
	if err := s.pb.PatchUser(rec.ID, map[string]any{
		"oldPassword":     body.OldPassword,
		"password":        body.NewPassword,
		"passwordConfirm": body.NewPassword,
	}); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "password update failed"})
		return
	}
	// Clear the fence + reissue the session (fresh epoch-carrying token).
	s.store.SetMustChangePW(sess.U, false)
	s.setSession(w, auth.Claims{U: sess.U, Role: sess.Role, Ep: s.store.Epoch(sess.U)})
	log.Printf("Password changed via PB: %s", sess.U)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleAPIUsageMe: GET /api/usage/me — the caller's own consumption.
func (s *Server) handleAPIUsageMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, 401, "login required")
		return
	}
	s.usage.Flush()
	u := s.usage.User(sess.U)
	tot := map[string]any{
		"requests":      uint0(u),
		"prompt_tokens": pint(u),
		"output_tokens": pout(u),
		"total_tokens":  pint(u) + pout(u),
		"kinds":         map[string]any{},
	}
	if u != nil && u.Kinds != nil {
		tot["kinds"] = u.Kinds
	}
	// Python parity: keys = store views (key_id/active) merged with usage
	// tallies, as an array.
	tallies := s.usage.KeyUsage(sess.U)
	keys := []map[string]any{}
	for _, kv := range s.store.ListKeys(sess.U) {
		entry := map[string]any{
			"key_id": kv.KeyID,
			"active": kv.Active,
			"usage": map[string]int{
				"requests":      0,
				"prompt_tokens": 0,
				"output_tokens": 0,
			},
		}
		if t, ok := tallies[kv.KeyID]; ok {
			entry["usage"] = t
		}
		keys = append(keys, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"username": sess.U,
		"totals":   tot,
		"keys":     keys,
	})
}

func uint0(u *UserUsage) int {
	if u == nil {
		return 0
	}
	return u.Requests
}

func pint(u *UserUsage) int {
	if u == nil {
		return 0
	}
	return u.PromptTokens
}

func pout(u *UserUsage) int {
	if u == nil {
		return 0
	}
	return u.OutputTokens
}

// handleAPIUsageHistory: GET /api/usage/history — per-day series for charts.
// Admins get energy/cost + per-user; users get only their own series.
func (s *Server) handleAPIUsageHistory(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, 401, "authentication required")
		return
	}
	isAdmin := sess.Role == "admin"
	s.usage.Flush()
	days, tokensByDay := s.usage.History(sess.U, isAdmin)
	out := map[string]any{
		"days":          days,
		"tokens_by_day": tokensByDay,
		"is_admin":      isAdmin,
	}
	if isAdmin {
		out["energy_by_day"] = s.energyByDay()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// energyByDay merges energy.daily across ninfer backends.
func (s *Server) energyByDay() map[string]map[string]float64 {
	merged := map[string]map[string]float64{}
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	for u := range s.backends {
		urls = append(urls, u)
	}
	s.mu.Unlock()
	for _, u := range urls {
		payload, ok := getJSON(s.client, u+"/usage", 3*time.Second)
		if !ok {
			continue
		}
		en, _ := payload["energy"].(map[string]any)
		daily, _ := en["daily"].(map[string]any)
		for day, vals := range daily {
			vm, _ := vals.(map[string]any)
			e := merged[day]
			if e == nil {
				e = map[string]float64{"kwh": 0, "cost_usd": 0, "tokens": 0}
				merged[day] = e
			}
			if f, ok := vm["kwh"].(float64); ok {
				e["kwh"] += f
			}
			if f, ok := vm["cost_usd"].(float64); ok {
				e["cost_usd"] += f
			}
			if f, ok := vm["tokens"].(float64); ok {
				e["tokens"] += f
			}
		}
	}
	return merged
}
