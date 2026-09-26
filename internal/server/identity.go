package server

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/pb"
)

// ---- session middleware (SPEC §1.3) ----

const sessionTTL = 7 * 24 * time.Hour

func (s *Server) sessionFrom(r *http.Request) (auth.Claims, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return auth.Claims{}, false
	}
	return s.store.VerifySession(c.Value)
}

func (s *Server) setSession(w http.ResponseWriter, c auth.Claims) {
	tok := s.store.SignSession(c, sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// ---- login (PB-backed; escalating per-IP+user backoff, SPEC parity) ----

var loginFails = struct {
	sync.Mutex
	m map[string]failEnt
}{m: map[string]failEnt{}}

type failEnt struct {
	fails int
	last  time.Time
}

const loginMaxWindow = 900 // forget after 15 min silence

func loginBackoffRemaining(ip, username string) time.Duration {
	loginFails.Lock()
	defer loginFails.Unlock()
	ent, ok := loginFails.m[ip+"|"+username]
	if !ok {
		return 0
	}
	if time.Since(ent.last) > loginMaxWindow*time.Second {
		delete(loginFails.m, ip+"|"+username)
		return 0
	}
	wait := time.Duration(0)
	if ent.fails > 0 {
		wait = time.Duration(1<<uint(ent.fails)) * time.Second
		if wait > time.Minute {
			wait = time.Minute
		}
	}
	rem := wait - time.Since(ent.last)
	if rem < 0 {
		return 0
	}
	return rem
}

func loginRecordFailure(ip, username string) {
	loginFails.Lock()
	defer loginFails.Unlock()
	ent := loginFails.m[ip+"|"+username]
	ent.fails++
	if ent.fails > 10 {
		ent.fails = 10
	}
	ent.last = time.Now()
	loginFails.m[ip+"|"+username] = ent
	if len(loginFails.m) > 1000 {
		for k := range loginFails.m {
			delete(loginFails.m, k)
			if len(loginFails.m) <= 800 {
				break
			}
		}
	}
}

func loginClearFailures(ip, username string) {
	loginFails.Lock()
	delete(loginFails.m, ip+"|"+username)
	loginFails.Unlock()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errBody(w, 405, "POST only")
		return
	}
	if err := r.ParseForm(); err != nil {
		errBody(w, 400, "bad form")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	ip := clientIP(r)

	if rem := loginBackoffRemaining(ip, username); rem > 0 {
		errBody(w, http.StatusTooManyRequests,
			fmt.Sprintf("Too many failed attempts - retry in %ds", int(rem.Seconds())+1))
		return
	}
	rec, err := s.pb.Authenticate(username, password)
	if err != nil {
		loginRecordFailure(ip, username)
		msg := "Invalid credentials (or account service unavailable)."
		if rem := loginBackoffRemaining(ip, username); rem > 500*time.Millisecond {
			msg += fmt.Sprintf(" Retry in %ds.", int(rem.Seconds())+1)
		}
		errBody(w, 401, msg)
		return
	}
	loginClearFailures(ip, username)
	claims := auth.Claims{U: rec.Username, Role: rec.Role, Ep: s.store.Epoch(rec.Username)}
	redirect := "/chat"
	if s.mustChangePW(rec.Username) {
		claims.MCP = true
		redirect = "/change-password"
	}
	s.setSession(w, claims)
	log.Printf("Web login: %s (role=%s) from %s", rec.Username, rec.Role, ip)
	w.Header().Set("Location", redirect)
	w.WriteHeader(http.StatusFound)
}

func (s *Server) mustChangePW(username string) bool {
	return s.store.MustChangePW(username)
}

func (s *Server) setMustChangePW(username string, v bool) {
	s.store.SetMustChangePW(username, v)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusFound)
}

// ---- /chat/config: session info + ui api key (SPEC parity) ----

// uiKeyFor returns the user's stable "auto" chat key: minted once, then
// reused across sessions AND restarts (persisted as a `ui` record with a
// fixed key_id "auto"). Old ui-* keys from the previous per-mint scheme
// are deactivated on first sight and pruned to zero.
func (s *Server) uiKeyFor(username, role string) string {
	if plain, ok := s.store.AutoKeyFor(username); ok {
		s.store.SetKeyRoleIfDiffers(username, "auto", role)
		return plain
	}
	plain, _, err := s.store.CreateUIKey(username, role)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	s.uiKeys[username] = plain
	s.mu.Unlock()
	return plain
}

func (s *Server) handleChatConfig(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, 401, "login required")
		return
	}
	if sess.MCP {
		w.Header().Set("Location", "/change-password")
		w.WriteHeader(http.StatusFound)
		return
	}
	apiKey := s.uiKeyFor(sess.U, sess.Role)
	// Persist mint-time role on the key record (admin Bearer checks use it).
	s.store.SetKeyRoleIfDiffers(sess.U, apiKey, sess.Role)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"username": sess.U,
		"role":     sess.Role,
		"api_key":  apiKey,
		"models":   s.modelIDs(),
	})
}

// modelIDs returns the chat-UI model list: every discovered model id
// (Python parity — /chat/config serves sorted _backend_cache.keys(), which
// includes pool member ids; /v1/models is the filtered public catalog).
func (s *Server) modelIDs() []string {
	set := map[string]bool{}
	s.mu.Lock()
	for _, info := range s.backends {
		for _, id := range info.Models {
			if id != "" {
				set[id] = true
			}
		}
	}
	s.mu.Unlock()
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ---- /keys: self-service key management (SPEC parity) ----

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		// Admin Bearer fallback (session-less CLI access)
		if user, _, ok2 := s.authorized(r); ok2 && s.store.RoleOf(user) == "admin" {
			sess = auth.Claims{U: user, Role: "admin"}
		} else {
			errBody(w, 401, "login required")
			return
		}
	}
	username := sess.U
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"username": username,
			"keys":     s.keysWithUsage(username),
		})
	case http.MethodPost:
		var body struct {
			Action       string `json:"action"`
			KeyID        string `json:"key_id"`
			GraceSeconds int    `json:"grace_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			errBody(w, 400, "bad json")
			return
		}
		s.handleKeysAction(w, sess, body)
	default:
		errBody(w, 405, "GET/POST only")
	}
}

func (s *Server) handleKeysAction(w http.ResponseWriter, sess auth.Claims, body struct {
	Action       string `json:"action"`
	KeyID        string `json:"key_id"`
	GraceSeconds int    `json:"grace_seconds"`
}) {
	username := sess.U
	switch body.Action {
	case "create_key":
		if s.store.CountActiveKeys(username) >= 10 {
			errBody(w, 400, "key limit reached (10 active)")
			return
		}
		plain, rec, err := s.store.CreateKey(username, body.KeyID, "", false)
		if err != nil {
			errBody(w, 500, err.Error())
			return
		}
		log.Printf("User %s created key '%s'", username, rec.KeyID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "key": plain, "key_id": rec.KeyID,
			"note": "shown once - store it now"})
	case "rotate_key":
		plain, grace, err := s.store.RotateKey(username, body.KeyID, body.GraceSeconds)
		if err != nil {
			errBody(w, 404, err.Error())
			return
		}
		log.Printf("User %s rotated key %s", username, body.KeyID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "key": plain, "old_key_valid_until": grace,
			"note": "new key shown once - store it now"})
	case "revoke_key":
		if !s.store.RevokeKey(username, body.KeyID) {
			errBody(w, 404, "no such key")
			return
		}
		log.Printf("User %s revoked key %s", username, body.KeyID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	default:
		errBody(w, 400, "unknown action")
	}
}

// ---- /admin/users: identity-plane admin (SPEC parity) ----

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$`)

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		// Admin Bearer fallback: key record carries mint-time role.
		if user, _, ok2 := s.authorized(r); ok2 && s.store.RoleOf(user) == "admin" {
			sess = auth.Claims{U: user, Role: "admin"}
		} else {
			errBody(w, 403, "admin only")
			return
		}
	}
	if sess.Role != "admin" {
		errBody(w, 403, "admin only")
		return
	}

	if r.Method == http.MethodGet {
		users, err := s.pb.ListUsers()
		if err != nil {
			errBody(w, 503, "PocketBase unreachable")
			return
		}
		out := map[string]any{}
		for _, u := range users {
			out[u.Username] = map[string]any{
				"username": u.Username, "email": u.Email, "role": u.Role,
				"verified": u.Verified, "created": u.Created,
				"keys":   s.keysWithUsage(u.Username),
				"limits": s.userDailyStatus(u.Username),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"users": out, "pocketbase_reachable": true})
		return
	}

	var body struct {
		Action   string `json:"action"`
		Username string `json:"username"`
		Role     string `json:"role"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Limit    int    `json:"daily_token_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errBody(w, 400, "bad json")
		return
	}
	target := strings.TrimSpace(body.Username)
	if target != "" && !usernameRE.MatchString(target) {
		errBody(w, 400, "invalid username")
		return
	}

	switch body.Action {
	case "set_limit":
		if target == "" {
			errBody(w, 400, "username required")
			return
		}
		if body.Limit < 0 {
			errBody(w, 400, "limit must be >= 0 (0 = unlimited)")
			return
		}
		if err := s.store.SetDailyLimit(target, body.Limit); err != nil {
			errBody(w, 500, err.Error())
			return
		}
		log.Printf("Admin %s set %s daily_token_limit=%d", sess.U, target, body.Limit)
		jsonOK(w, map[string]any{"status": "ok", "daily_token_limit": body.Limit})
	case "create_user":
		pw := body.Password
		if pw == "" {
			pw = newTempPassword()
		}
		email := strings.TrimSpace(body.Email)
		if email == "" {
			email = target + "@llm.local"
		}
		role := body.Role
		if role == "" {
			role = "user"
		}
		rec, err := s.pb.CreateUser(target, email, pw, role)
		if err != nil {
			errBody(w, 400, pbErrMsg(err))
			return
		}
		s.setMustChangePW(target, true)
		log.Printf("Admin %s created PB user %s (must change password)", sess.U, target)
		jsonOK(w, map[string]any{"status": "ok", "username": rec.Username, "password": pw,
			"note": "password shown once - pass to user; they must set a new one on first login"})
	case "set_email":
		rec, err := s.pb.FindUser(target)
		if err != nil || rec == nil {
			errBody(w, 404, "no such user")
			return
		}
		if err := s.pb.PatchUser(rec.ID, map[string]any{"email": strings.ToLower(body.Email)}); err != nil {
			errBody(w, 400, pbErrMsg(err))
			return
		}
		log.Printf("Admin %s set %s email", sess.U, target)
		jsonOK(w, map[string]any{"status": "ok"})
	case "set_role":
		rec, err := s.pb.FindUser(target)
		if err != nil || rec == nil {
			errBody(w, 404, "no such user")
			return
		}
		if err := s.pb.PatchUser(rec.ID, map[string]any{"role": body.Role}); err != nil {
			errBody(w, 400, pbErrMsg(err))
			return
		}
		s.store.SetAllKeyRoles(target, body.Role)
		log.Printf("Admin %s set %s role=%s", sess.U, target, body.Role)
		jsonOK(w, map[string]any{"status": "ok"})
	case "reset_password":
		rec, err := s.pb.FindUser(target)
		if err != nil || rec == nil {
			errBody(w, 404, "no such user")
			return
		}
		pw := body.Password
		if pw == "" {
			pw = newTempPassword()
		}
		if err := s.pb.PatchUser(rec.ID, map[string]any{"password": pw, "passwordConfirm": pw}); err != nil {
			errBody(w, 400, pbErrMsg(err))
			return
		}
		s.store.BumpEpoch(target) // kill sessions minted before the reset
		s.setMustChangePW(target, true)
		log.Printf("Admin %s reset password for %s", sess.U, target)
		jsonOK(w, map[string]any{"status": "ok", "password": pw,
			"note": "shown once - pass to user"})
	case "delete_user":
		if target == sess.U {
			errBody(w, 400, "you cannot delete your own account")
			return
		}
		rec, err := s.pb.FindUser(target)
		if err != nil || rec == nil {
			errBody(w, 404, "no such user")
			return
		}
		if err := s.pb.DeleteUser(rec.ID); err != nil {
			errBody(w, 400, pbErrMsg(err))
			return
		}
		s.store.PurgeUser(target)
		s.store.BumpEpoch(target) // kill any live sessions
		log.Printf("Admin %s DELETED user %s (PB record + local keys)", sess.U, target)
		jsonOK(w, map[string]any{"status": "ok",
			"note": target + " deleted from PocketBase; local keys purged"})
	case "disable_user":
		s.store.DisableUser(target)
		s.store.BumpEpoch(target) // kill any live sessions
		if rec, err := s.pb.FindUser(target); err == nil && rec != nil {
			_ = s.pb.PatchUser(rec.ID, map[string]any{"role": "user"})
		}
		log.Printf("Admin %s disabled %s (keys revoked)", sess.U, target)
		jsonOK(w, map[string]any{"status": "ok",
			"note": "keys revoked; account remains in PocketBase - delete there to remove"})
	default:
		errBody(w, 400, "unknown action")
	}
}

// ---- small helpers ----

func newTempPassword() string { return randToken(12) }

func randToken(n int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	raw := make([]byte, n)
	if _, err := crand.Read(raw); err != nil {
		panic(err)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alnum[int(raw[i])%len(alnum)]
	}
	return string(b)
}

func jsonOK(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func pbErrMsg(err error) string {
	if he, ok := err.(*pb.HTTPError); ok {
		return he.Body
	}
	return err.Error()
}

// claimsFor builds session claims for Bearer-path pseudo-sessions.
func claimsFor(user, role string) auth.Claims {
	return auth.Claims{U: user, Role: role, Ep: 0}
}

// keysWithUsage merges per-key usage tallies into redacted key views.
func (s *Server) keysWithUsage(username string) []auth.KeyView {
	keys := s.store.ListKeys(username)
	tallies := s.usage.KeyUsage(username)
	for i := range keys {
		if t, ok := tallies[keys[i].KeyID]; ok {
			keys[i].Usage = t
		}
	}
	return keys
}
