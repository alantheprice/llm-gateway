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
	w := routing.Weights{
		NinferLane:     nc.Metrics.NinferLaneWeight,
		NinferQueue:    nc.Metrics.NinferQueueWeight,
		NinferPressure: nc.Metrics.NinferPressureWt,
		StaleSeconds:   float64(nc.Metrics.StaleThreshold),
	}
	// Keep live load state across config saves: only the weights change.
	if s.tracker != nil {
		s.tracker.SetWeights(w)
	} else {
		s.tracker = routing.NewTracker(w)
	}
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
// /chat/password; this form endpoint mirrors the /change-password POST).
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
	// Vault: rekey BEFORE the password patch (see handleChatPassword for the
	// why) so the vault and the password can never end up mismatched on disk.
	if s.vault != nil {
		if err := s.vault.RekeyOnPasswordChange(sess.U, oldPW, newPW); err != nil {
			log.Printf("vault rekey before first-login pw set: %s: %v — password NOT changed", sess.U, err)
			http.Redirect(w, r, "/change-password?error=encrypted+data+re-key+failed%2C+password+not+changed", http.StatusFound)
			return
		}
	}
	if err := s.pb.PatchUser(rec.ID, map[string]any{
		"oldPassword": oldPW, "password": newPW, "passwordConfirm": newPW}); err != nil {
		if s.vault != nil {
			if rerr := s.vault.RekeyOnPasswordChange(sess.U, newPW, oldPW); rerr != nil {
				log.Printf("vault re-key-back after failed first-login pw patch: %s: %v", sess.U, rerr)
			}
		}
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
	mux.HandleFunc("/v1/models/", s.handleModelInfo) // per-model: pool members + model cards
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/agent/chat", s.handleAgentChat)
	mux.HandleFunc("/v1/completions", s.handlePassthrough)
	mux.HandleFunc("/v1/images/generations", s.handleImages)
	mux.HandleFunc("/v1/embeddings", s.handleEmbeddings)
	mux.HandleFunc("/v1/rag/search", s.handleRAGSearch)
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
	mux.HandleFunc("/admin/routing", s.handleRoutingPage)
	mux.HandleFunc("/api/routing", s.handleRoutingAPI)
	mux.HandleFunc("/api/routing/test", s.handleRoutingTest)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/metrics", s.handleMetricsRich)
	mux.HandleFunc("/slots", s.handleSlots)
	mux.HandleFunc("/backends", s.handleBackendsRich)

	// Identity plane
	mux.HandleFunc("/login", s.methodSwitch(map[string]http.HandlerFunc{
		http.MethodPost: s.handleLogin, // GET /login = 405 (login page lives at /)
	}))
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/chat", s.handleChatPage)
	mux.HandleFunc("/chat/config", s.handleChatConfig)
	mux.HandleFunc("/chat/password", s.handleChatPassword)
	mux.HandleFunc("/change-password", s.methodSwitch(map[string]http.HandlerFunc{
		http.MethodGet:  s.handleChangePWPage,
		http.MethodPost: s.handleChangePWSubmit,
	}))
	mux.HandleFunc("/gpus", s.handleGPUsPage)
	mux.HandleFunc("/models", s.handleModelsPage)
	mux.HandleFunc("/documents", s.handleDocumentsPage)
	mux.HandleFunc("/images", s.handleImagesPage)
	mux.HandleFunc("/embeddings", s.handleEmbeddingsPage)
	mux.HandleFunc("/api/gpus", s.handleAPIGPUs)
	mux.HandleFunc("/api/mcp", s.handleAPIMCP)
	mux.HandleFunc("/api/mcp/oauth/start", s.handleMCPOAuthStart)
	mux.HandleFunc("/api/mcp/oauth/callback", s.handleMCPOAuthCallback)
	mux.HandleFunc("/api/chats", s.handleAPIChats)
	mux.HandleFunc("/api/alerts", s.handleAPIAlerts)
	mux.HandleFunc("/api/chats/", s.handleAPIChats)
	mux.HandleFunc("/api/documents", s.handleAPIDocs)
	mux.HandleFunc("/api/documents/", s.handleAPIDocs)
	mux.HandleFunc("/api/vault/wrapinfo", s.handleVaultWrapInfo)
	mux.HandleFunc("/api/vault", s.handleAPIVault)
	mux.HandleFunc("/api/vault/", s.handleAPIVault)
	mux.HandleFunc("/downloads/", s.handleAgentDownload)
	mux.HandleFunc("/keys", s.methodSwitch(map[string]http.HandlerFunc{
		http.MethodGet:  s.handleKeysPage, // page
		http.MethodPost: s.handleKeys,     // API action
	}))
	mux.HandleFunc("/api/keys", s.handleKeys) // UI JS calls /api/keys; same API handler
	mux.HandleFunc("/account", s.handleAccountPage)
	mux.HandleFunc("/settings", s.handleSettingsPage)
	mux.HandleFunc("/api/prefs", s.handleAPIPrefs)
	mux.HandleFunc("/account/update", s.handleAccountUpdate)
	mux.HandleFunc("/me", s.handleMe)
	mux.HandleFunc("/usage/me", s.handleMyUsagePage)
	mux.HandleFunc("/api/usage/me", s.handleAPIUsageMe)
	mux.HandleFunc("/api/usage/history", s.handleAPIUsageHistory)
	mux.HandleFunc("/api/usage/range", s.handleUsageRange)
	mux.HandleFunc("/bootstrap", s.handleBootstrapPage)
	mux.HandleFunc("/static/setup-key.sh", s.handleSetupKeyScript)
	mux.HandleFunc("/link/agent", s.handleLinkAgent)
	mux.HandleFunc("/admin/links", s.handleAdminLinks)
	mux.HandleFunc("/guide", s.guideHandler)
	mux.HandleFunc("/guide/", s.guideHandler)
	mux.HandleFunc("/admin/users/page", s.handleAdminUsersPage)
	mux.HandleFunc("/admin/users", s.handleAdminUsers)
	mux.HandleFunc("/admin/overview", s.handleAdminSystemPage)
	mux.HandleFunc("/admin/system", redirectTo("/admin/overview"))
	mux.HandleFunc("/admin/costs", s.handleAdminCostsPage)
	mux.HandleFunc("/admin/performance", s.handleAnalyticsPage)
	mux.HandleFunc("/admin/analytics/page", redirectTo("/admin/performance"))
	mux.HandleFunc("/api/analytics", s.handleAnalytics)
	mux.HandleFunc("/admin/costs/backfill", s.handleCostsBackfill)
	mux.HandleFunc("/usage/costs", s.handleUsageCosts)
	mux.HandleFunc("/api/costs/history", s.handleCostHistory)

	// Embedded UI assets.
	mux.Handle("/static/", web.StaticHandler(staticVerValue))

	// Docs (huma-generated OpenAPI + UI), auth-gated.
	s.SetupDocs()
	mux.Handle("/openapi.json", s.docHandler)
	mux.Handle("/openapi.yaml", s.docHandler)
	mux.Handle("/docs", s.docHandler)
	mux.Handle("/schemas/", s.docHandler)

	mux.HandleFunc("/", s.handleRoot)
	return s.withAccessLog(s.withThrottle(mux))
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
	// First-run bootstrap: an install with zero PB accounts has no way to
	// log in. Serve a one-time create-admin form instead of a dead login
	// (only while the users collection is empty; disappears after use).
	// Uses the embedded app directly — the PB HTTP client would need a
	// superuser, which a fresh install doesn't have yet.
	// MUST be panic-safe: a broken embedded PB (DB closed mid-life, port
	// stolen at boot) previously nil-deref'd here and 502'd every "/"
	// request through the tunnel. Degrade to the login page instead.
	if app := s.EmbeddedPB(); app != nil && s.embeddedPBHealthy() {
		empty := func() (ok bool) {
			defer func() { _ = recover() }()
			n, err := app.CountUsers()
			return err == nil && n == 0
		}()
		if empty {
			s.handleBootstrapPage(w, r)
			return
		}
	}
	s.handleLoginPage(w, r)
}

// embeddedPBHealthy: the attached app has open DB handles (safe to query).
func (s *Server) embeddedPBHealthy() bool {
	app := s.EmbeddedPB()
	type readier interface{ OpsReady() bool }
	r, ok := app.(readier)
	return ok && r.OpsReady()
}

// checkAuth: key-or-LAN (the /v1 inference-plane contract; sessions do NOT
// count here).
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	user, keyID, ok := s.authorized(r)
	if !ok {
		unauthorized(w)
		return "", "", false
	}
	if user == "" {
		user = "local" // LAN-trusted unkeyed requests
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
	user := s.requestUser(r)
	data := s.catalog()
	listed := map[string]bool{}
	for i, e := range data {
		listed[e.ID] = true
		data[i] = s.describe(e, user)
	}
	for _, pm := range s.privateModels(user) {
		if !listed[pm.Name] { // platform names win
			data = append(data, s.describe(ModelEntry{ID: pm.Name, Object: "model", OwnedBy: pm.Owner}, user))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// catalog builds the model list: pool MEMBER model ids are hidden (they'd
// let clients pin an engine and bypass cache-affinity routing); every other
// discovered model (embeddings, FIM, standalone) is advertised; pool
// virtual names are synthesized when no backend carries them.
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
	if !s.enforceDailyLimit(w, user) {
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
	r = withClientModel(r, req.Model) // responses carry the name the client called
	noteAccess(r, user, req.Model, req.Stream)

	// Pool path (its name or an alias). A model none of whose GPUs can
	// serve this endpoint is refused up front with a pointer to the right one.
	if poolName, pool, isPool := s.poolFor(req.Model); isPool {
		if c, _ := s.capsFor(req.Model, ""); !c.has(c.Endpoints, "chat") {
			checkCaps(w, r, req.Model, c, body)
			return
		}
		s.routePool(w, r, &pool, poolName, body, user, keyID)
		return
	}

	// Direct resolution
	url, mid := s.resolve(req.Model)
	if url != "" && !checkCaps(w, r, req.Model, s.withOverride(req.Model, s.engineCaps(url, req.Model)), body) {
		return
	}
	if url == "" {
		// A private GPU link the caller owns or was shared (never shadows
		// a platform model: resolved last).
		if pm, ok := s.resolvePrivate(privateCaller(user, keyID), req.Model); ok {
			if !checkCaps(w, r, req.Model, s.withOverride(req.Model, s.engineCaps(pm.URL, pm.ModelID)), body) {
				return
			}
			s.proxy(w, r, pm.URL, rewriteModel(body, pm.ModelID), user, keyID, req.Model, pm.ModelID)
			return
		}
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
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return
	}
	if !s.enforceDailyLimit(w, user) {
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	var req chatReq
	json.Unmarshal(body, &req)
	r = withClientModel(r, req.Model)
	noteAccess(r, user, req.Model, req.Stream)
	// Shared models serve every endpoint, not only chat (e.g. a FIM model
	// behind a link).
	if poolName, pool, isPool := s.poolFor(req.Model); isPool {
		s.routePool(w, r, &pool, poolName, body, user, keyID)
		return
	}
	url, mid := s.resolve(req.Model)
	if url == "" {
		if pm, ok := s.resolvePrivate(privateCaller(user, keyID), req.Model); ok {
			if !checkCaps(w, r, req.Model, s.withOverride(req.Model, s.engineCaps(pm.URL, pm.ModelID)), body) {
				return
			}
			s.proxy(w, r, pm.URL, rewriteModel(body, pm.ModelID), user, keyID, req.Model, pm.ModelID)
			return
		}
		http.Error(w, `{"error":{"message":"no backend"}}`, http.StatusNotFound)
		return
	}
	if !checkCaps(w, r, req.Model, s.withOverride(req.Model, s.engineCaps(url, req.Model)), body) {
		return
	}
	s.proxy(w, r, url, body, user, "", req.Model, mid)
}

// handleImages: POST /v1/images/generations (OpenAI images API) — text to
// image, routed like chat: shared model or alias, then a single engine,
// then a private link the caller can use. Image generation can take a
// while; like other non-streamed requests it has no overall timeout.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errBody(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return
	}
	if !s.enforceDailyLimit(w, user) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, `{"error":{"message":"body too large"}}`, http.StatusRequestEntityTooLarge)
		return
	}
	var req chatReq
	if json.Unmarshal(body, &req) != nil || req.Model == "" {
		errBody(w, http.StatusBadRequest, "JSON body with a model and a prompt required")
		return
	}
	r = withClientModel(r, req.Model)
	noteAccess(r, user, req.Model, false)
	// Only models that generate images get their prompt improved first.
	if c, ok := s.capsFor(req.Model, privateCaller(user, keyID)); ok {
		if !checkCaps(w, r, req.Model, c, body) {
			return
		}
		r, body = s.prepareImageRequest(r, body)
		noteAccess(r, user, req.Model, false)
	}
	if poolName, pool, isPool := s.poolFor(req.Model); isPool {
		s.routePool(w, r, &pool, poolName, body, user, keyID)
		return
	}
	if url, mid := s.resolve(req.Model); url != "" {
		if !checkCaps(w, r, req.Model, s.withOverride(req.Model, s.engineCaps(url, req.Model)), body) {
			return
		}
		s.proxy(w, r, url, body, user, keyID, req.Model, mid)
		return
	}
	if pm, ok := s.resolvePrivate(privateCaller(user, keyID), req.Model); ok {
		if !checkCaps(w, r, req.Model, s.withOverride(req.Model, s.engineCaps(pm.URL, pm.ModelID)), body) {
			return
		}
		s.proxy(w, r, pm.URL, rewriteModel(body, pm.ModelID), user, keyID, req.Model, pm.ModelID)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"message": fmt.Sprintf("model %q not found", req.Model), "type": "invalid_request_error"}})
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return
	}
	if !s.enforceDailyLimit(w, user) {
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	var req chatReq
	json.Unmarshal(body, &req)
	r = withClientModel(r, req.Model)
	noteAccess(r, user, req.Model, false)
	// A shared model (e.g. an embedding engine behind a link).
	if poolName, pool, isPool := s.poolFor(req.Model); isPool {
		s.routePool(w, r, &pool, poolName, body, user, keyID)
		return
	}
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
	s.proxy(w, r, url, body, user, "", req.Model, mid)
}

func (s *Server) handleV1Other(w http.ResponseWriter, r *http.Request) {
	if !s.allowProbe(r.URL.Path, r) {
		rateLimited(w)
		return
	}
	if _, _, ok := s.checkAuth(w, r); !ok {
		return
	}
	http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
}

// routePool implements SPEC §5.4 + §5.5 (failover before first byte).
func (s *Server) routePool(w http.ResponseWriter, r *http.Request, pool *poolCfgT,
	modelName string, body []byte, user, keyID string) {
	// Refresh member metrics (best-effort, concurrent).
	s.PollOnce()

	// Streaming requests: ask the engine for the usage chunk so accounting
	// is exact (falls back to estimate otherwise). The injected chunk is
	// stripped before the client sees it (context flag read in relay).
	if streamRequested(body) && !clientAskedIncludeUsage(body) {
		body = withUsageHint(body)
		r = withUsageHintCtx(r)
	}

	est := estimateFrom(body)
	needs := requestNeeds(r.URL.Path, body)
	members := s.poolMembersFor(modelName, pool, est+requestedOutputTokens(body), &needs)
	if len(members) == 0 {
		// Nothing can serve it: say why when it's a capability, not an outage.
		c, _ := s.capsFor(modelName, "")
		if ok, missing := c.supports(needs); !ok {
			name := clientModelOf(r)
			if name == "" {
				name = modelName
			}
			capabilityError(w, name, missing)
			return
		}
		poolUnavailable(w)
		return
	}

	// Content-affinity: if this pool opts in and the body carries a
	// messages array, try to ride the GPU that already holds the KV
	// prefix. Falls through to the classic picker on any miss/guard.
	var convMsgs []routing.Message
	if pool.CacheAffinity {
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &req) == nil && len(req.Messages) >= 2 {
			for _, m := range req.Messages {
				convMsgs = append(convMsgs, routing.Message{
					Role: m.Role, Content: contentText(m.Content),
				})
			}
		}
	}
	if pool.CacheAffinity && len(convMsgs) >= 2 { // every prompt size
		inflight := map[string]int{}
		for _, m := range members {
			inflight[m.URL] = s.tracker.InFlight(m.URL)
		}
		if cp := routing.PickCache(s.cacheTable, modelName, members, s.tracker, convMsgs, inflight,
			coldPrefillMs(est)); cp != nil {
			s.relayPoolPick(w, r, modelName, pool, body, *cp, user, keyID, est, convMsgs)
			return
		}

		// NEW conversation (no cache entry): apply the fairness cap before
		// the classic picker. Scores for every member, once.
		if pool.MaxPoolShare > 0 {
			scores := map[string]float64{}
			for _, m := range members {
				scores[m.URL] = s.tracker.Score(m.URL)
			}
			if url := routing.PickNewConversation(s.dayShare, pool.MaxPoolShare, members, scores); url != "" {
				for _, m := range members {
					if m.URL == url {
						s.dayShare.Inc(url)
						s.relayPoolPick(w, r, modelName, pool, body, routing.PickResult{
							URL: m.URL, ModelID: m.ModelID, Scores: scores,
						}, user, keyID, est, convMsgs)
						return
					}
				}
			}
		}
	}

	s.routePoolClassic(w, r, modelName, pool, body, user, keyID, est, convMsgs)
}

// routePoolClassic: score/pin-based member loop (SPEC §5.4) with
// pre-commit failover. The cache-affinity path delegates here on miss.
func (s *Server) routePoolClassic(w http.ResponseWriter, r *http.Request,
	modelName string, pool *poolCfgT, body []byte, user, keyID string, est int, convMsgs []routing.Message) {

	sess := sessionKey(r, keyID)
	s.mu.Lock()
	leader := s.leader[modelName]
	s.mu.Unlock()

	needs := requestNeeds(r.URL.Path, body)
	members := s.poolMembersFor(modelName, pool, est+requestedOutputTokens(body), &needs)
	if len(members) == 0 {
		poolUnavailable(w)
		return
	}

	var lastStatus int
	var lastBody []byte
	candidates := members
	for len(candidates) > 0 {
		pick := routing.PickPool(modelName, pool.OverflowThreshold, pool.StickyBias,
			pool.CapacityBias, candidates, s.tracker, sess, leader, pool.CacheAffinity)
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
		rs, fwdBody := s.prepareStats(r, fwdBody, pick.URL) // per-response engine stats
		status, respHeader, respBody, reader, err, release := s.dispatchCounted(rs, pick.URL, fwdBody)
		if err == nil && status < 500 && status != http.StatusRequestTimeout {
			s.mu.Lock()
			s.leader[modelName] = pick.URL
			s.mu.Unlock()
			defer release()
			s.relay(w, rs, respHeader, respBody, reader, status, user, keyID, pick.ModelID, est, pick.URL)
			s.recordConv(modelName, convMsgs, pick.URL)
			return
		}
		// Abandoned candidate: drain+close any stream reader so the
		// backend (or link pipe) sees EOF instead of a wedged writer.
		release()
		if reader != nil {
			io.Copy(io.Discard, reader)
			if closer, ok := reader.(io.Closer); ok {
				closer.Close()
			}
		}
		lastStatus, lastBody = status, respBody
		if err != nil {
			if r.Context().Err() != nil {
				return // client went away; nothing to fail over for
			}
			log.Printf("Pool '%s': %s connect failed (%v), marking down", modelName, pick.URL, err)
			s.tracker.MarkDown(pick.URL)
			lastStatus, lastBody = 0, nil
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
	if lastStatus == 0 {
		// Every member failed to connect: the pool is down, not erroring.
		poolUnavailable(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(lastStatus)
	w.Write(lastBody)
}

// poolMembers: the pool's routable members for a request needing `need`
// tokens (prompt estimate + requested output): down members removed, and
// members whose context window cannot hold the request skipped.
func (s *Server) poolMembers(name string, pool *poolCfgT, need int) []routing.Member {
	return s.poolMembersFor(name, pool, need, nil)
}

// poolMembersFor is poolMembers restricted to GPUs whose model can serve
// needs (e.g. image input, tools); nil needs = no restriction.
func (s *Server) poolMembersFor(name string, pool *poolCfgT, need int, needs *capNeeds) []routing.Member {
	s.mu.Lock()
	membersCfg := pool.Members
	s.mu.Unlock()
	members := make([]routing.Member, 0, len(membersCfg))
	for _, m := range membersCfg {
		if !s.linkServesPool(m.Backend, name) {
			continue // a user's link serves a pool only with its owner's consent
		}
		if needs != nil {
			if ok, _ := s.engineCaps(m.Backend, m.ModelID).supports(*needs); !ok {
				continue // this GPU's model can't serve the request
			}
		}
		lanes := 0
		if l := s.tracker.Get(m.Backend); l != nil {
			lanes = l.Lanes
		}
		maxCtx := m.MaxContext
		if maxCtx == 0 {
			maxCtx = s.maxContext(m.Backend)
		}
		members = append(members, routing.Member{
			URL: m.Backend, ModelID: m.ModelID, MaxContext: maxCtx,
			CapacityWeight: m.CapacityWeight, Lanes: lanes,
		})
	}
	return routing.FitContext(s.upMembers(members), need)
}

// requestedOutputTokens: the output budget a request asks for
// (max_completion_tokens, else max_tokens); 0 if unspecified. It counts
// against the context window alongside the prompt.
func requestedOutputTokens(body []byte) int {
	var req struct {
		MaxTokens           int `json:"max_tokens"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &req) != nil {
		return 0
	}
	if req.MaxCompletionTokens > 0 {
		return req.MaxCompletionTokens
	}
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return 0
}

// upMembers drops members currently marked down. If every member is down
// the result is empty and the caller answers 503 at once instead of
// spending connect timeouts on dead hosts.
func (s *Server) upMembers(members []routing.Member) []routing.Member {
	up := members[:0:0]
	for _, m := range members {
		if !s.tracker.IsDown(m.URL) {
			up = append(up, m)
		}
	}
	return up
}

// poolUnavailable: 503 + Retry-After when no pool member can serve.
func poolUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"error":{"message":"no healthy backend available for this model; retry shortly","type":"service_unavailable"}}`))
}

// coldPrefillAssumedTokS: prefill speed assumed when estimating what moving
// a conversation to another GPU costs. Conservative: the 5090's measured
// cold-prefill median (~6,000 tok/s, 2026-09-28); the 6000 Pro is faster.
const coldPrefillAssumedTokS = 6000.0

// coldPrefillMs: estimated time to re-prefill a conversation of estTokens
// on another GPU.
func coldPrefillMs(estTokens int) float64 {
	if estTokens <= 0 {
		return 0
	}
	return float64(estTokens) / coldPrefillAssumedTokS * 1000
}

// redirectTo: a permanent redirect for a page that moved.
func redirectTo(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, path, http.StatusMovedPermanently)
	}
}
