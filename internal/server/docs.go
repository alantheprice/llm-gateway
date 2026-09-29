package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"llmgateway/internal/auth"
)

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
	// Capabilities: endpoints, input/output modalities, features, source.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	// Architecture: the same modalities in the shape OpenRouter-style
	// clients read ("text+image->text").
	Architecture *Architecture `json:"architecture,omitempty"`
}

// Architecture mirrors OpenRouter's model.architecture block.
type Architecture struct {
	Modality         string   `json:"modality"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

func (s *Server) registerInferenceDocs() {
	modelsOp := huma.Operation{
		OperationID: "get-v1-models", Method: http.MethodGet, Path: "/v1/models",
		Tags: []string{"inference"}, Summary: "Model catalog",
		Description: "Lists advertised models. Pool members collapse to the pool name; the caller's private GPUs are included. Public by default — gateway.models_require_auth=true gates it behind key/session/LAN trust.",
		Responses:   map[string]*huma.Response{"200": {Description: "Catalog (public)"}},
	}
	if s.cfg.Gateway.ModelsRequireAuth {
		modelsOp.Security = secs(secBearer, secSession)
		modelsOp.Responses = map[string]*huma.Response{
			"200": {Description: "Catalog"},
			"401": {Description: "Missing/invalid key"},
		}
	}
	docOnly[ModelsOutput](s, modelsOp)

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
		Models   []string `json:"models" doc:"chat-capable models, flattened from model_groups"`
		// ModelGroups: the picker's sections (Models, your & shared GPUs,
		// and for admins the individual engines behind pools).
		ModelGroups []chatModelGroup `json:"model_groups"`
		// ImageModels: names the caller can generate images with;
		// ImagePromptModel: the text model that improves image prompts ("" = off).
		ImageModels []string `json:"image_models"`
		// EmbeddingModels: names the caller can embed text with.
		EmbeddingModels []string `json:"embedding_models"`
		// Prefs: the user's saved preferences (theme, chat defaults).
		Prefs            map[string]any `json:"prefs"`
		ImagePromptModel string         `json:"image_prompt_model"`
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
		Summary:     "Web login",
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
	docOnly[ChatConfigOutput](s, huma.Operation{
		OperationID: "get-chat-config", Method: http.MethodGet, Path: "/chat/config",
		Tags: []string{"identity"}, Summary: "Session info + UI API key",
		Description: "Mints the caller's single UI key on first use (plaintext in memory only for the session; hash persisted in the key store). models / model_groups: the chat-capable models the caller can pick.",
		Security:    secs(secSession),
		Responses: map[string]*huma.Response{
			"200": {Description: "Session + key"},
			"401": {Description: "No session"},
			"403": {Description: "Password change required (mcp fence)"},
		},
	})

	// /keys GET: typed; POST stays bare (dynamic body → JSON response).
	docOnly[KeysOutput](s, huma.Operation{
		OperationID: "get-keys", Method: http.MethodGet, Path: "/keys",
		Tags: []string{"identity"}, Summary: "List my API keys (redacted, with per-key usage)",
		Security: secs(secSession, secBearer),
		Responses: map[string]*huma.Response{
			"200": {Description: "Key list"},
			"401": {Description: "No session / invalid key"},
		},
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
	docOnly[AdminListOutput](s, huma.Operation{
		OperationID: "get-admin-users", Method: http.MethodGet, Path: "/admin/users",
		Tags: []string{"admin"}, Summary: "List all accounts + keys",
		Security: secs(secSession, secBearer),
		Responses: map[string]*huma.Response{
			"200": {Description: "Users + keys"},
			"403": {Description: "Admin only"},
			"503": {Description: "account service unavailable"},
		},
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

// docOnly declares a typed docs-only operation: O's shape documents the
// response, but the real handler lives on the raw mux (the docs router only
// serves /openapi.*, /docs and /schemas/), so the stub never runs.
func docOnly[O any](s *Server, op huma.Operation) {
	huma.Register(s.huma, op, func(_ context.Context, _ *struct{}) (*O, error) {
		return nil, huma.Error501NotImplemented("handled by raw mux")
	})
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
