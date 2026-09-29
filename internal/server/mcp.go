package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"llmgateway/internal/auth"
)

// Remote MCP servers: each user can connect MCP servers reached over HTTP
// (no local process). Their tools join agent chat (the chat's Tools mode):
// the gateway keeps the list and its auth headers, and hands the enabled
// ones to the agent sidecar with each agent request.

const maxMCPServersPerUser = 10

var mcpNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,23}$`)

// mcpServerView: a server as the UI sees it — header values masked.
type mcpServerView struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Enabled bool              `json:"enabled"`
	Added   string            `json:"added,omitempty"`
}

func maskSecret(v string) string {
	if len(v) <= 8 {
		return "••••"
	}
	return "••••" + v[len(v)-4:]
}

func mcpViews(list []auth.MCPServer) []mcpServerView {
	out := make([]mcpServerView, 0, len(list))
	for _, m := range list {
		h := map[string]string{}
		for k, v := range m.Headers {
			h[k] = maskSecret(v)
		}
		out = append(out, mcpServerView{ID: m.ID, Name: m.Name, URL: m.URL, Headers: h, Enabled: m.Enabled, Added: m.Added})
	}
	return out
}

// privateHost: the URL's host is loopback, private or link-local (by
// literal IP, or by what its name resolves to now). The agent re-checks
// every connection, so this only gives an early, clear error.
func privateHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
		return true
	}
	ips := []net.IP{net.ParseIP(h)}
	if ips[0] == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
		if err != nil {
			return false // unresolvable now: the agent's guard decides later
		}
		ips = ips[:0]
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// validateMCPServer checks a server before it's saved. Admins may add
// http:// and LAN servers; everyone else needs a public https:// URL.
func validateMCPServer(m auth.MCPServer, isAdmin bool) error {
	if !mcpNameRE.MatchString(m.Name) {
		return fmt.Errorf("name: 1–24 lowercase letters, digits, - or _, starting with a letter or digit")
	}
	u, err := url.Parse(m.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("URL must be an http(s) address, e.g. https://example.com/mcp")
	}
	if u.User != nil {
		return fmt.Errorf("put credentials in a header, not the URL")
	}
	if !isAdmin {
		if u.Scheme != "https" {
			return fmt.Errorf("URL must use https")
		}
		if privateHost(u.Hostname()) {
			return fmt.Errorf("the server must be on the public internet (only admins can add local or LAN servers)")
		}
	}
	for k, v := range m.Headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, " :\r\n") || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid header %q", k)
		}
		switch strings.ToLower(k) {
		case "host", "content-length", "content-type", "accept", "mcp-session-id", "mcp-protocol-version":
			return fmt.Errorf("header %q is set by the gateway", k)
		}
	}
	if len(m.Headers) > 8 {
		return fmt.Errorf("at most 8 headers")
	}
	return nil
}

func newMCPID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "mcp-" + hex.EncodeToString(b)
}

// handleAPIMCP: GET lists the signed-in user's MCP servers; POST
// {action: add | update | delete | test}.
func (s *Server) handleAPIMCP(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	user, isAdmin := sess.U, sess.Role == "admin"
	list := s.store.MCPServersOf(user)
	reply := func(extra map[string]any) {
		out := map[string]any{"servers": mcpViews(s.store.MCPServersOf(user)), "limit": maxMCPServersPerUser, "is_admin": isAdmin}
		for k, v := range extra {
			out[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
	if r.Method == http.MethodGet {
		reply(nil)
		return
	}
	if r.Method != http.MethodPost {
		errBody(w, http.StatusMethodNotAllowed, "GET or POST")
		return
	}
	var body struct {
		Action  string            `json:"action"`
		ID      string            `json:"id"`
		Name    *string           `json:"name"`
		URL     *string           `json:"url"`
		Headers map[string]string `json:"headers"` // masked values keep the stored secret
		Enabled *bool             `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		errBody(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	idx := -1
	for i, m := range list {
		if m.ID == body.ID {
			idx = i
		}
	}
	// apply: body fields onto m (headers: "••••…" keeps the stored value).
	apply := func(m auth.MCPServer) auth.MCPServer {
		if body.Name != nil {
			m.Name = strings.ToLower(strings.TrimSpace(*body.Name))
		}
		if body.URL != nil {
			m.URL = strings.TrimSpace(*body.URL)
		}
		if body.Headers != nil {
			h := map[string]string{}
			for k, v := range body.Headers {
				k = strings.TrimSpace(k)
				if k == "" {
					continue
				}
				if strings.HasPrefix(v, "••••") {
					if old, ok := m.Headers[k]; ok {
						v = old
					}
				}
				h[k] = strings.TrimSpace(v)
			}
			m.Headers = h
		}
		if body.Enabled != nil {
			m.Enabled = *body.Enabled
		}
		return m
	}
	nameTaken := func(name string, except int) bool {
		for i, m := range list {
			if i != except && m.Name == name {
				return true
			}
		}
		return false
	}

	switch body.Action {
	case "add":
		if len(list) >= maxMCPServersPerUser {
			errBody(w, http.StatusBadRequest, fmt.Sprintf("at most %d MCP servers", maxMCPServersPerUser))
			return
		}
		m := apply(auth.MCPServer{ID: newMCPID(), Enabled: true, Added: time.Now().UTC().Format(time.RFC3339)})
		if err := validateMCPServer(m, isAdmin); err != nil {
			errBody(w, http.StatusBadRequest, err.Error())
			return
		}
		if nameTaken(m.Name, -1) {
			errBody(w, http.StatusBadRequest, "you already have a server named "+m.Name)
			return
		}
		list = append(list, m)
	case "update":
		if idx < 0 {
			errBody(w, http.StatusNotFound, "no such server")
			return
		}
		m := apply(list[idx])
		if err := validateMCPServer(m, isAdmin); err != nil {
			errBody(w, http.StatusBadRequest, err.Error())
			return
		}
		if nameTaken(m.Name, idx) {
			errBody(w, http.StatusBadRequest, "you already have a server named "+m.Name)
			return
		}
		list[idx] = m
	case "delete":
		if idx < 0 {
			errBody(w, http.StatusNotFound, "no such server")
			return
		}
		list = append(list[:idx], list[idx+1:]...)
	case "test":
		// A saved server (by id), or an unsaved one from the form.
		var m auth.MCPServer
		if idx >= 0 {
			m = apply(list[idx])
		} else {
			m = apply(auth.MCPServer{Name: "test"})
		}
		if err := validateMCPServer(m, isAdmin); err != nil {
			errBody(w, http.StatusBadRequest, err.Error())
			return
		}
		res, err := s.agentMCPTools(r.Context(), m, isAdmin)
		if err != nil {
			errBody(w, http.StatusBadGateway, err.Error())
			return
		}
		reply(map[string]any{"test": res})
		return
	default:
		errBody(w, http.StatusBadRequest, "unknown action")
		return
	}
	if err := s.store.SetMCPServers(user, list); err != nil {
		errBody(w, http.StatusInternalServerError, "save failed")
		return
	}
	log.Printf("User %s %s MCP server %s", user, body.Action+"d", body.ID)
	reply(nil)
}

// agentMCPTools asks the agent sidecar to connect to m and list its tools.
func (s *Server) agentMCPTools(ctx context.Context, m auth.MCPServer, allowPrivate bool) (map[string]any, error) {
	blob, _ := json.Marshal(map[string]any{
		"server":        map[string]any{"name": m.Name, "url": m.URL, "headers": m.Headers},
		"allow_private": allowPrivate,
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.agentURL+"/v1/mcp/tools", strings.NewReader(string(blob)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent service unavailable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("the agent service is too old for MCP; update seed-agent")
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("agent service: bad reply")
	}
	return out, nil
}

// withMCPServers sets the caller's enabled MCP servers on an agent request
// body (overwriting anything the client sent: only the gateway decides
// which servers, and whether LAN addresses are allowed).
func (s *Server) withMCPServers(body []byte, user, keyID string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	var servers []map[string]any
	if privateCaller(user, keyID) != "" {
		for _, sv := range s.store.MCPServersOf(user) {
			if sv.Enabled {
				servers = append(servers, map[string]any{"name": sv.Name, "url": sv.URL, "headers": sv.Headers})
			}
		}
	}
	m["mcp_servers"] = servers
	m["allow_private"] = user != "" && s.store.RoleOf(user) == "admin"
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
