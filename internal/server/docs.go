package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"llmgateway/internal/auth"
)

// humaAuth is a per-operation middleware: runs the standard gateway auth
// (session cookie OR Bearer key OR LAN trust) and 401s via huma errors.
func (s *Server) humaAuth(ctx huma.Context) error {
	r, w := humago.Unwrap(ctx)
	if _, _, ok := s.checkAuth(w, r); !ok {
		return huma.Error401Unauthorized("Invalid or missing API key")
	}
	return nil
}

func (s *Server) humaSession(ctx huma.Context) (auth.Claims, bool) {
	r, _ := humago.Unwrap(ctx)
	return s.sessionFrom(r)
}

var secBearer = map[string][]string{"bearer": {}}
var secSession = map[string][]string{"session": {}}

func secs(secs ...map[string][]string) []map[string][]string { return secs }

// SetupDocs mounts the huma API: OpenAPI JSON/YAML + docs UI, gated behind
// the gateway auth. Typed ops for JSON APIs; bare ops (docs-only) for
// streaming endpoints whose real handlers stay on the raw mux.
func (s *Server) SetupDocs() {
	docMux := http.NewServeMux()
	cfg := huma.DefaultConfig("LLM Gateway", "2.0.0")
	cfg.Info.Description = "Self-hosted, engine-aware LLM gateway: pool routing " +
		"by KV-cache affinity, invite-only identity, per-key usage accounting. " +
		"All endpoints require auth unless gateway.trust_local_networks covers " +
		"the caller (127.0.0.1 is treated as Cloudflare tunnel traffic and is " +
		"NEVER trusted)."
	cfg.Servers = []*huma.Server{{URL: "/"}}
	cfg.Components = &huma.Components{
		SecuritySchemes: map[string]*huma.SecurityScheme{
			"bearer": {
				Type:         "http",
				Scheme:       "bearer",
				BearerFormat: "API key (sk-...)",
				Description:  "Gateway API key minted via /keys or the web UI.",
			},
			"session": {
				Type:        "apiKey",
				In:          "cookie",
				Name:        sessionCookie,
				Description: "Web session cookie issued by /login.",
			},
		},
	}
	cfg.Security = secs(secBearer, secSession)

	api := humago.New(docMux, cfg)
	s.huma = api

	// Docs themselves are auth-gated: session OR key OR LAN trust.
	s.docHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.docsAuth(w, r) {
			return // docsAuth already wrote 401
		}
		docMux.ServeHTTP(w, r)
	})

	s.registerSystemDocs()
	s.registerInferenceDocs()
	s.registerIdentityDocs()
	s.registerAdminDocs()
}

// ---- system ----

type HealthOutput struct{ Body string }

func (s *Server) registerSystemDocs() {
	// /health is handled (unauthenticated) on the raw mux. Docs entry only.
	s.registerBare(huma.Operation{
		OperationID: "health", Method: http.MethodGet, Path: "/health",
		Summary: "Liveness probe (unauthenticated by design)",
		Tags:    []string{"system"},
		Responses: map[string]*huma.Response{
			"200": {Description: "text/plain OK"},
		},
	})
}

// ---- inference (typed where non-streaming; bare for proxies) ----

type ModelsOutput struct {
	Body struct {
		Object string       `json:"object"`
		Data   []ModelEntry `json:"data"`
	}
}

type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) registerInferenceDocs() {
	huma.Get(s.huma, "/v1/models", func(_ctx context.Context, _ *struct{}) (*ModelsOutput, error) {
		out := &ModelsOutput{}
		out.Body.Object = "list"
		out.Body.Data = s.catalog()
		return out, nil
	}, func(o *huma.Operation) {
		o.Tags = []string{"inference"}
		o.Summary = "Model catalog"
		o.Description = "Lists advertised models. Pool members collapse to the virtual pool name; public_models filters when configured."
		o.Security = secs(secBearer, secSession)
		o.Middlewares = huma.Middlewares{func(ctx huma.Context, next func(huma.Context)) {
			if err := s.humaAuth(ctx); err != nil {
				return
			}
			next(ctx)
		}}
		o.Responses = map[string]*huma.Response{
			"200": {Description: "Catalog"},
			"401": {Description: "Missing/invalid key"},
		}
	})

	for _, op := range []huma.Operation{
		{
			OperationID: "chat-completions", Method: http.MethodPost, Path: "/v1/chat/completions",
			Summary:     "Chat completion (OpenAI-compatible)",
			Description: "Routes through the model pool (session pinning, size affinity, reactive failover before first byte) or directly to the resolved backend. Supports \"stream\": true (SSE). Usage is recorded once on the serving member.",
			Tags:        []string{"inference"},
			Security:    secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Completion JSON or SSE stream"},
				"401": {Description: "Missing/invalid key"},
				"502": {Description: "All pool members failed"},
			},
		},
		{
			OperationID: "create-embeddings", Method: http.MethodPost, Path: "/v1/embeddings",
			Summary:  "Embeddings (OpenAI-compatible)",
			Tags:     []string{"inference"},
			Security: secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Embeddings response"},
				"404": {Description: "No embedding backend"},
			},
		},
		{
			OperationID: "agent-chat", Method: http.MethodPost, Path: "/v1/agent/chat",
			Summary:     "Agentic chat (seed-agent sidecar)",
			Description: "Proxies the agentic loop (web_search + fetch tools via Jina). SSE stream: start/tool_start/tool_end/content/done events.",
			Tags:        []string{"inference"},
			Security:    secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "SSE event stream"},
				"401": {Description: "Missing/invalid key"},
				"502": {Description: "Sidecar unavailable"},
			},
		},
		{
			OperationID: "backends-snapshot", Method: http.MethodGet, Path: "/backends",
			Summary:     "Per-backend engine load snapshot",
			Description: "Engine-aware scores (0=idle, 1=saturated), lanes, running/waiting, decode tps, daily energy kWh, cache-hit pct per backend. Triggers a fresh metrics poll.",
			Tags:        []string{"observability"},
			Security:    secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Backend map"},
				"401": {Description: "Missing/invalid key"},
			},
		},
		{
			OperationID: "engine-usage", Method: http.MethodGet, Path: "/usage",
			Summary:     "Aggregate engine usage",
			Description: "Merged /usage payloads from all discovered backends keyed by backend URL (tokens, throughput, energy, KV health).",
			Tags:        []string{"observability"},
			Security:    secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Usage map"},
				"401": {Description: "Missing/invalid key"},
			},
		},
		{
			OperationID: "engine-metrics", Method: http.MethodGet, Path: "/metrics",
			Summary:  "Prometheus metrics (merged backends)",
			Tags:     []string{"observability"},
			Security: secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Prometheus text exposition"},
				"401": {Description: "Missing/invalid key"},
			},
		},
		{
			OperationID: "engine-slots", Method: http.MethodGet, Path: "/slots",
			Summary:  "Engine slot state (merged backends)",
			Tags:     []string{"observability"},
			Security: secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Slots map"},
				"401": {Description: "Missing/invalid key"},
			},
		},
		{
			OperationID: "usage-users", Method: http.MethodGet, Path: "/usage/users",
			Summary:     "Per-user token accounting (admin)",
			Description: "All-time and today per-user requests/tokens with per-key and per-kind breakdowns. Deliberately no per-user energy: NVML meters whole-GPU power.",
			Tags:        []string{"observability"},
			Security:    secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Usage by user"},
				"403": {Description: "Admin only"},
			},
		},
		{
			OperationID: "get-config", Method: http.MethodGet, Path: "/config",
			Summary:  "Show current configuration (admin)",
			Tags:     []string{"admin"},
			Security: secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "Active config JSON"},
				"403": {Description: "Admin only"},
			},
		},
		{
			OperationID: "reload-config", Method: http.MethodPost, Path: "/config/reload",
			Summary:  "Reload configuration from disk (admin)",
			Tags:     []string{"admin"},
			Security: secs(secBearer, secSession),
			Responses: map[string]*huma.Response{
				"200": {Description: "reloaded or unchanged"},
				"403": {Description: "Admin only"},
			},
		},
	} {
		op := op
		s.registerBare(op)
	}
}

// ---- identity ----

type ChatConfigOutput struct {
	Body struct {
		Username string   `json:"username"`
		Role     string   `json:"role"`
		APIKey   string   `json:"api_key"`
		Models   []string `json:"models"`
	}
}

type KeysOutput struct {
	Body struct {
		Username string         `json:"username"`
		Keys     []auth.KeyView `json:"keys"`
	}
}

type KeyActionInput struct {
	Body struct {
		Action       string `json:"action" enum:"create_key,rotate_key,revoke_key"`
		KeyID        string `json:"key_id"`
		GraceSeconds int    `json:"grace_seconds"`
	}
}

type KeyActionOutput struct {
	Body map[string]any `json:"-"`
}

func (s *Server) registerIdentityDocs() {
	// /login, /logout: bare ops (form POST/redirect + cookie on raw mux).
	s.registerBare(huma.Operation{
		OperationID: "login", Method: http.MethodPost, Path: "/login",
		Summary:     "Web login (PocketBase-backed)",
		Description: "Form POST (username, password). Issues the llmgw_session cookie (HttpOnly, SameSite=Lax, 7d). Escalating per-IP+username backoff on failures.",
		Tags:        []string{"identity"},
		Responses: map[string]*huma.Response{
			"302": {Description: "Redirect to /chat (or /change-password on first login)"},
			"401": {Description: "Invalid credentials"},
			"429": {Description: "Backoff window"},
		},
	})
	s.registerBare(huma.Operation{
		OperationID: "logout", Method: http.MethodGet, Path: "/logout",
		Summary:   "Clear the session cookie",
		Tags:      []string{"identity"},
		Responses: map[string]*huma.Response{"302": {Description: "Redirect to /"}},
	})

	// /chat/config: typed, session-only.
	huma.Get(s.huma, "/chat/config", func(_ctx context.Context, _ *struct{}) (*ChatConfigOutput, error) {
		ctx := _ctx.(huma.Context)
		_, r, ok := s.humaClaims(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("login required")
		}
		sess, _ := s.sessionFrom(r)
		if sess.MCP {
			return nil, huma.Error403Forbidden("password change required")
		}
		apiKey := s.uiKeyFor(sess.U, sess.Role)
		s.store.SetKeyRoleIfDiffers(sess.U, apiKey, sess.Role)
		out := &ChatConfigOutput{}
		out.Body.Username = sess.U
		out.Body.Role = sess.Role
		out.Body.APIKey = apiKey
		out.Body.Models = s.modelIDs()
		return out, nil
	}, func(o *huma.Operation) {
		o.Tags = []string{"identity"}
		o.Summary = "Session info + UI API key"
		o.Description = "Mints the caller's single UI key on first use (plaintext in memory only for the session; hash persisted in the key store)."
		o.Security = secs(secSession)
		o.Middlewares = huma.Middlewares{func(ctx huma.Context, next func(huma.Context)) {
			if err := s.humaSessionMW(ctx); err != nil {
				return
			}
			next(ctx)
		}}
		o.Responses = map[string]*huma.Response{
			"200": {Description: "Session + key"},
			"401": {Description: "No session"},
			"403": {Description: "Password change required (mcp fence)"},
		}
	})

	// /keys GET: typed; POST stays bare (dynamic body → JSON response).
	huma.Get(s.huma, "/keys", func(_ctx context.Context, _ *struct{}) (*KeysOutput, error) {
		ctx := _ctx.(huma.Context)
		_, r, ok := s.humaAuthPair(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("login required")
		}
		sess, hasSession := s.sessionFrom(r)
		if !hasSession {
			user, _, isKey := s.authorized(r)
			if !isKey || s.store.RoleOf(user) != "admin" {
				return nil, huma.Error401Unauthorized("login required")
			}
			sess = auth.Claims{U: user, Role: "admin"}
		}
		out := &KeysOutput{}
		out.Body.Username = sess.U
		out.Body.Keys = s.keysWithUsage(sess.U)
		return out, nil
	}, func(o *huma.Operation) {
		o.Tags = []string{"identity"}
		o.Summary = "List my API keys (redacted, with per-key usage)"
		o.Security = secs(secSession, secBearer)
		o.Responses = map[string]*huma.Response{
			"200": {Description: "Key list"},
			"401": {Description: "No session / invalid key"},
		}
	})

	s.registerBare(huma.Operation{
		OperationID: "keys-action", Method: http.MethodPost, Path: "/keys",
		Summary:     "Create / rotate / revoke my keys",
		Description: "Body {\"action\":\"create_key\"|\"rotate_key\"|\"revoke_key\",\"key_id\":\"...\",\"grace_seconds\":3600}. New key plaintext shown once. Max 10 active keys. Rotation keeps the old key valid for the grace window under a -retired- id.",
		Tags:        []string{"identity"},
		Security:    secs(secSession, secBearer),
		Responses: map[string]*huma.Response{
			"200": {Description: "Action result (new key plaintext for create/rotate)"},
			"400": {Description: "Unknown action or key limit reached"},
			"404": {Description: "No such key"},
		},
	})
}

// ---- admin ----

type AdminListOutput struct {
	Body struct {
		Users               map[string]map[string]any `json:"users"`
		PocketbaseReachable bool                      `json:"pocketbase_reachable"`
	}
}

func (s *Server) registerAdminDocs() {
	huma.Get(s.huma, "/admin/users", func(_ctx context.Context, _ *struct{}) (*AdminListOutput, error) {
		ctx := _ctx.(huma.Context)
		w, r, ok := s.humaAuthPair(ctx)
		if !ok {
			return nil, huma.Error403Forbidden("admin only")
		}
		sess, role, ok := s.adminIdentity(w, r)
		if !ok {
			return nil, huma.Error403Forbidden("admin only")
		}
		_ = sess
		_ = role
		users, err := s.pb.ListUsers()
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("PocketBase unreachable")
		}
		out := &AdminListOutput{}
		out.Body.PocketbaseReachable = true
		out.Body.Users = map[string]map[string]any{}
		for _, u := range users {
			out.Body.Users[u.Username] = map[string]any{
				"username": u.Username, "email": u.Email, "role": u.Role,
				"verified": u.Verified, "created": u.Created,
				"keys": s.keysWithUsage(u.Username),
			}
		}
		return out, nil
	}, func(o *huma.Operation) {
		o.Tags = []string{"admin"}
		o.Summary = "List all accounts + keys"
		o.Security = secs(secSession, secBearer)
		o.Responses = map[string]*huma.Response{
			"200": {Description: "Users + keys"},
			"403": {Description: "Admin only"},
			"503": {Description: "PocketBase unreachable"},
		}
	})

	s.registerBare(huma.Operation{
		OperationID: "admin-user-action", Method: http.MethodPost, Path: "/admin/users",
		Summary:     "Provision accounts (create_user/set_email/set_role/reset_password/delete_user/disable_user)",
		Description: "Admin session or admin Bearer key. Username regex ^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$. Passwords shown once. disable/delete/reset invalidate the target's live sessions (session epochs).",
		Tags:        []string{"admin"},
		Security:    secs(secSession, secBearer),
		Responses: map[string]*huma.Response{
			"200": {Description: "Action result"},
			"400": {Description: "Invalid username or PB rejected"},
			"403": {Description: "Admin only"},
			"404": {Description: "No such user"},
		},
	})
}

// ---- huma helpers ----

// humaAuthPair unwraps the raw writer/request after humaAuth ran.
func (s *Server) humaAuthPair(ctx huma.Context) (http.ResponseWriter, *http.Request, bool) {
	r, w := humago.Unwrap(ctx)
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return w, r, false
	}
	_ = user
	_ = keyID
	return w, r, true
}

// humaClaims unwraps and returns claims for session-authenticated calls.
func (s *Server) humaClaims(ctx huma.Context) (http.ResponseWriter, *http.Request, bool) {
	r, w := humago.Unwrap(ctx)
	sess, ok := s.sessionFrom(r)
	if !ok {
		return w, r, false
	}
	_ = sess
	return w, r, true
}

// humaSessionMW is a middleware form of the session check.
func (s *Server) humaSessionMW(ctx huma.Context) error {
	r, _ := humago.Unwrap(ctx)
	if _, ok := s.sessionFrom(r); !ok {
		return huma.Error401Unauthorized("login required")
	}
	return nil
}

// adminIdentity resolves the caller as admin (session or persisted-role key).
func (s *Server) adminIdentity(w http.ResponseWriter, r *http.Request) (auth.Claims, string, bool) {
	if sess, ok := s.sessionFrom(r); ok && sess.Role == "admin" {
		return sess, sess.Role, true
	}
	if user, _, ok := s.authorized(r); ok && s.store.RoleOf(user) == "admin" {
		return auth.Claims{U: user, Role: "admin"}, user, true
	}
	return auth.Claims{}, "", false
}

// registerBare declares a docs-only operation whose real handler lives on the
// raw mux (streaming/form/redirect endpoints). The huma-registered stub
// handler never runs in production (raw mux claims the route first), but
// keeps OpenAPI schemas generated.
func (s *Server) registerBare(op huma.Operation) {
	switch op.Method {
	case http.MethodPost:
		huma.Register(s.huma, op, func(_ context.Context, _ *struct{}) (*struct{}, error) {
			return nil, huma.Error501NotImplemented("handled by raw mux")
		})
	case http.MethodGet, "":
		op.Method = http.MethodGet
		huma.Register(s.huma, op, func(_ context.Context, _ *struct{}) (*struct{}, error) {
			return nil, huma.Error501NotImplemented("handled by raw mux")
		})
	}
}

// errBody writes the gateway's standard JSON error shape.
func errBody(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
