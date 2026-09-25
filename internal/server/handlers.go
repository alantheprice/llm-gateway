package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"

	"llmgateway/internal/config"
	"llmgateway/internal/routing"
	"llmgateway/internal/web"
)

// staticVerValue backs the /static cache-busting version (set in New).
var staticVerValue = func() *atomic.Value {
	v := &atomic.Value{}
	v.Store("1")
	return v
}()

func init() { web.SetIconFunc(iconSVG) }

// SetConfig swaps the active config after a hot reload (or admin save).
func (s *Server) SetConfig(nc *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = nc
	// local_networks live in a package-level parsed set; re-derive so
	// network edits apply without a restart.
	s.ParseNetworks()
	s.tracker = routing.NewTracker(routing.Weights{
		NinferLane:     nc.Metrics.NinferLaneWeight,
		NinferQueue:    nc.Metrics.NinferQueueWeight,
		NinferPressure: nc.Metrics.NinferPressureWt,
		StaleSeconds:   float64(nc.Metrics.StaleThreshold),
	})
}

// FlushUsage persists usage counters (called periodically + on shutdown).
func (s *Server) FlushUsage() { s.usage.Flush() }

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

// methodSwitch dispatches per HTTP method; 405 otherwise.
func (s *Server) methodSwitch(handlers map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.Method]; ok {
			h(w, r)
			return
		}
		errBody(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleChangePWSubmit is the form fallback (the UI posts JSON via
// /chat/password; this form endpoint mirrors Python's /change-password POST).
func (s *Server) handleChangePWSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		errBody(w, http.StatusBadRequest, "bad form")
		return
	}
	sess, ok := s.sessionFrom(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	oldPW := r.FormValue("old_password")
	newPW := r.FormValue("new_password")
	if len(newPW) < 8 {
		http.Redirect(w, r, "/change-password?error=new+password+must+be+at+least+8+characters", http.StatusFound)
		return
	}
	rec, err := s.pb.Authenticate(sess.U, oldPW)
	if err != nil || rec == nil {
		http.Redirect(w, r, "/change-password?error=temporary+password+incorrect", http.StatusFound)
		return
	}
	if err := s.pb.PatchUser(rec.ID, map[string]any{
		"oldPassword": oldPW, "password": newPW, "passwordConfirm": newPW}); err != nil {
		http.Redirect(w, r, "/change-password?error=password+update+failed", http.StatusFound)
		return
	}
	s.store.SetMustChangePW(sess.U, false)
	s.setSession(w, sessionClaims{U: sess.U, Role: sess.Role, Ep: s.store.Epoch(sess.U)})
	log.Printf("First-login password set: %s", sess.U)
	http.Redirect(w, r, "/chat", http.StatusFound)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/agent/chat", s.handleAgentChat)
	mux.HandleFunc("/v1/completions", s.handlePassthrough)
	mux.HandleFunc("/v1/embeddings", s.handleEmbeddings)
	mux.HandleFunc("/v1/", s.handleV1Other)
	mux.HandleFunc("/usage", s.handleUsageRich)
	mux.HandleFunc("/usage/users", s.handleUsageUsers)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/config/reload", s.handleConfigReload)
	mux.HandleFunc("/admin/config", s.methodSwitch(map[string]http.HandlerFunc{
		http.MethodGet:  s.handleAdminConfigGet,
		http.MethodPost: s.handleAdminConfigPost,
	}))
	mux.HandleFunc("/admin/config/page", s.handleAdminConfigPage)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/metrics", s.handleMetricsRich)
	mux.HandleFunc("/slots", s.handleSlots)
	mux.HandleFunc("/backends", s.handleBackendsRich)

	// Identity plane (SPEC parity with Python gateway)
	mux.HandleFunc("/login", s.methodSwitch(map[string]http.HandlerFunc{
		http.MethodPost: s.handleLogin, // GET /login = 405, Python parity (login page lives at /)
	}))
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/chat", s.handleChatPage)
	mux.HandleFunc("/chat/config", s.handleChatConfig)
	mux.HandleFunc("/chat/password", s.handleChatPassword)
	mux.HandleFunc("/change-password", s.methodSwitch(map[string]http.HandlerFunc{
		http.MethodGet:  s.handleChangePWPage,
		http.MethodPost: s.handleChangePWSubmit,
	}))
	mux.HandleFunc("/keys", s.methodSwitch(map[string]http.HandlerFunc{
		http.MethodGet:  s.handleKeysPage, // page (Python parity)
		http.MethodPost: s.handleKeys,     // API action
	}))
	mux.HandleFunc("/api/keys", s.handleKeys) // UI JS calls /api/keys; same API handler
	mux.HandleFunc("/account", s.handleAccountPage)
	mux.HandleFunc("/account/update", s.handleAccountUpdate)
	mux.HandleFunc("/me", s.handleMe)
	mux.HandleFunc("/usage/me", s.handleMyUsagePage)
	mux.HandleFunc("/api/usage/me", s.handleAPIUsageMe)
	mux.HandleFunc("/api/usage/history", s.handleAPIUsageHistory)
	mux.HandleFunc("/admin/users/page", s.handleAdminUsersPage)
	mux.HandleFunc("/admin/users", s.handleAdminUsers)
	mux.HandleFunc("/admin/system", s.handleAdminSystemPage)
	mux.HandleFunc("/admin/costs", s.handleAdminCostsPage)
	mux.HandleFunc("/usage/costs", s.handleUsageCosts)

	// Embedded UI assets.
	mux.Handle("/static/", web.StaticHandler(staticVerValue))

	// Docs (huma-generated OpenAPI + UI), auth-gated.
	s.SetupDocs()
	mux.Handle("/openapi.json", s.docHandler)
	mux.Handle("/openapi.yaml", s.docHandler)
	mux.Handle("/docs", s.docHandler)
	mux.Handle("/schemas/", s.docHandler)

	mux.HandleFunc("/", s.handleRoot)
	return s.withThrottle(mux)
}

func (s *Server) withThrottle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" && !s.allow(r) {
			rateLimited(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.handleLoginPage(w, r)
}

// checkAuth: key-or-LAN (the /v1 inference-plane contract; sessions do NOT
// count here, matching the Python gateway).
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	user, keyID, ok := s.authorized(r)
	if !ok {
		unauthorized(w)
		return "", "", false
	}
	if user == "" {
		user = "local" // LAN-trusted unkeyed requests (Python parity)
	}
	return user, keyID, true
}

// docsAuth: session cookie OR Bearer key OR LAN trust — used for the
// generated docs/OpenAPI endpoints (they're identity-plane surfaces).
func (s *Server) docsAuth(w http.ResponseWriter, r *http.Request) bool {
	if _, _, ok := s.authorized(r); ok {
		return true
	}
	if _, ok := s.sessionFrom(r); ok {
		return true
	}
	unauthorized(w)
	return false
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	// Catalog is public by default (models_require_auth=false): OpenAI-
	// compatible clients probe /v1/models before they have a key. The knob
	// opts into gating it like the other /v1 surfaces.
	if s.cfg.Gateway.ModelsRequireAuth {
		if _, _, ok := s.checkAuth(w, r); !ok {
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": s.catalog()})
}

// catalog builds the model list, mirroring the live Python gateway's
// models_handler: pool MEMBER model ids are hidden (they'd let clients pin an
// engine and bypass cache-affinity routing); every other discovered model
// (embeddings, FIM, standalone) is advertised; pool virtual names are
// synthesized when no backend carries them. The legacy `public_models`
// config knob is parsed for compat but NOT applied (Python retired it).
func (s *Server) catalog() []ModelEntry {
	memberIDs := map[string]bool{}
	s.mu.Lock()
	discovered := make([]ModelEntry, 0, 8)
	for _, info := range s.backends {
		for _, id := range info.Models {
			if memberIDs[id] || id == "" {
				continue
			}
			memberIDs[id] = false
			discovered = append(discovered, ModelEntry{ID: id, Object: "model", OwnedBy: "llm-gateway"})
		}
	}
	s.mu.Unlock()
	for _, pool := range s.cfg.ModelPools {
		for _, m := range pool.Members {
			if m.ModelID != "" {
				memberIDs[m.ModelID] = true
			}
		}
	}
	var ids []ModelEntry
	known := map[string]bool{}
	for _, e := range discovered {
		if !memberIDs[e.ID] {
			known[e.ID] = true
			ids = append(ids, e)
		}
	}
	for name := range s.cfg.ModelPools {
		if !known[name] {
			// Virtual id isn't a backend-discovered model: synthesize a
			// listing so clients can discover and select it (front of list).
			ids = append([]ModelEntry{{ID: name, Object: "model", OwnedBy: "llm-gateway"}}, ids...)
			known[name] = true
		}
	}
	// Note: `public_models` is parsed for config compat but intentionally NOT
	// applied — live Python retired that filter in favor of pool-driven
	// member-hiding (llm_gateway.py models_handler).
	return ids
}

type chatReq struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, `{"error":{"message":"body too large"}}`, http.StatusRequestEntityTooLarge)
		return
	}
	var req chatReq
	if err := json.Unmarshal(body, &req); err != nil {
		unauthorized(w) // malformed JSON with missing key still 401s in py; but parse error => 400
		return
	}

	// Pool path
	s.mu.Lock()
	pool, isPool := s.cfg.ModelPools[req.Model]
	s.mu.Unlock()
	if isPool {
		s.routePool(w, r, &pool, req.Model, body, user, keyID)
		return
	}

	// Overflow-pair path
	s.mu.Lock()
	pair, hasPair := s.cfg.OverflowPairs[req.Model]
	s.mu.Unlock()
	if hasPair {
		if s.tryOverflow(w, r, req.Model, &pair, body, user, keyID) {
			return
		}
	}

	// Direct resolution
	url, mid := s.resolve(req.Model)
	if url == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
			"message": fmt.Sprintf("model %q not found", req.Model), "type": "invalid_request_error"}})
		return
	}
	s.proxy(w, r, url, body, user, keyID, req.Model, mid)
}

func (s *Server) resolve(model string) (url, modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for u, info := range s.backends {
		for _, m := range info.Models {
			if m == model {
				return u, model
			}
		}
	}
	return "", ""
}

func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.checkAuth(w, r); !ok {
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	var req chatReq
	json.Unmarshal(body, &req)
	url, mid := s.resolve(req.Model)
	if url == "" {
		http.Error(w, `{"error":{"message":"no backend"}}`, http.StatusNotFound)
		return
	}
	s.proxy(w, r, url, body, "operator", "", req.Model, mid)
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.checkAuth(w, r); !ok {
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	var req chatReq
	json.Unmarshal(body, &req)
	s.mu.Lock()
	var url, mid string
	for u, info := range s.backends {
		if info.Embeds {
			for _, m := range info.Models {
				if m == req.Model {
					url, mid = u, m
				}
			}
		}
	}
	s.mu.Unlock()
	if url == "" {
		http.Error(w, `{"error":{"message":"no embedding backend"}}`, http.StatusNotFound)
		return
	}
	s.proxy(w, r, url, body, "operator", "", req.Model, mid)
}

func (s *Server) handleV1Other(w http.ResponseWriter, r *http.Request) {
	if !s.allowProbe(r.URL.Path, r) {
		rateLimited(w)
		return
	}
	if _, _, ok := s.checkAuth(w, r); !ok {
		return
	}
	url, _ := s.resolve("")
	_ = url
	http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
}

// routePool implements SPEC §5.4 + §5.5 (failover before first byte).
func (s *Server) routePool(w http.ResponseWriter, r *http.Request, pool *poolCfgT,
	modelName string, body []byte, user, keyID string) {

	// Refresh member metrics (best-effort, concurrent).
	s.PollOnce()

	s.mu.Lock()
	membersCfg := pool.Members
	s.mu.Unlock()
	members := make([]routing.Member, 0, len(membersCfg))
	for _, m := range membersCfg {
		lanes := 0
		if l := s.tracker.Get(m.Backend); l != nil {
			lanes = l.Lanes
		}
		members = append(members, routing.Member{
			URL: m.Backend, ModelID: m.ModelID,
			LargeContext: m.LargeContext, CapacityWeight: m.CapacityWeight, Lanes: lanes,
		})
	}
	if len(members) == 0 {
		http.Error(w, `{"error":{"message":"pool: no reachable members","type":"service_unavailable"}}`, http.StatusServiceUnavailable)
		return
	}

	est := estimateFrom(body)
	sess := sessionKey(r, keyID)

	s.mu.Lock()
	leader := s.leader[modelName]
	s.mu.Unlock()

	var lastStatus int
	var lastBody []byte
	candidates := members
	for len(candidates) > 0 {
		pick := routing.PickPool(modelName, pool.OverflowThreshold, pool.StickyBias,
			pool.LargePromptTokens, pool.CapacityBias, candidates, s.tracker, est, sess, leader)
		leader = pick.URL

		// Rewrite pool name -> the chosen member's backend model id
		// (SPEC §5: members serve their own ids, which usually differ
		// from the pool's virtual name).
		fwdBody := body
		var reqData map[string]any
		if json.Unmarshal(body, &reqData) == nil {
			if old, _ := reqData["model"].(string); old != pick.ModelID {
				reqData["model"] = pick.ModelID
				if nb, err := json.Marshal(reqData); err == nil {
					fwdBody = nb
				}
			}
		}

		// Failover happens BEFORE any bytes are committed to the client:
		// use a buffering probe for non-stream requests; for streams we
		// check the response status/CT before relaying.
		status, respHeader, respBody, reader, err := s.dispatch(r, pick.URL, fwdBody)
		if err == nil && status < 500 && status != http.StatusRequestTimeout {
			s.mu.Lock()
			s.leader[modelName] = pick.URL
			s.mu.Unlock()
			s.tracker.InFlightInc(pick.URL)
			defer s.tracker.InFlightDec(pick.URL)
			s.relay(w, r, respHeader, respBody, reader, status, user, keyID, pick.ModelID, est)
			return
		}
		lastStatus, lastBody = status, respBody
		if err != nil {
			lastStatus = http.StatusBadGateway
			lastBody = []byte(`{"error":{"message":"backend connect failed","type":"proxy_error"}}`)
		}
		// drop this member and retry
		var next []routing.Member
		for _, m := range candidates {
			if m.URL != pick.URL {
				next = append(next, m)
			}
		}
		candidates = next
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusOr502(lastStatus))
	w.Write(lastBody)
}

func statusOr502(code int) int {
	if code == 0 {
		return http.StatusBadGateway
	}
	return code
}
