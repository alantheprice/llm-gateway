package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"llmgateway/internal/link"
)

// handleLinkAgent: WebSocket upgrade for link agents.
// Auth: an active key that is either role "link" or owned by an admin.
// Every prompt routed to a link reaches the agent's machine, so ordinary
// user keys cannot open one. The key's user owns the agent label.
func (s *Server) handleLinkAgent(w http.ResponseWriter, r *http.Request) {
	user, keyID, err := s.authenticateLinkToken(r)
	if err != nil {
		log.Printf("link: agent connection from %s refused: %v", r.RemoteAddr, err)
		errBody(w, 401, "invalid link token")
		return
	}
	link.ServeAgentAs(s.linkReg, w, r, user, keyID)
}

// authenticateLinkToken: resolve the bearer token to (user, key id);
// requires an active key with role "link" or an admin owner.
func (s *Server) authenticateLinkToken(r *http.Request) (string, string, error) {
	// Header only: a token in the query string ends up in proxy and
	// access logs. (Agents before 0.2 sent ?key= and are refused.)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return "", "", fmt.Errorf("missing Authorization: Bearer <link token>")
	}
	user, rec, ok := s.store.LookupKey(tok)
	if !ok || !rec.Active {
		return "", "", fmt.Errorf("invalid token")
	}
	if rec.Role != "link" && s.store.RoleOf(user) != "admin" {
		return "", "", fmt.Errorf("key %q (user %s) is not a link key and not admin-owned", rec.KeyID, user)
	}
	return user, rec.KeyID, nil
}

// linkForwardHeaders: the only client headers relayed to a link agent.
// Authorization and cookies carry the CLIENT's gateway credentials and
// must never reach a remote machine; the agent supplies its own engine
// key if the engine needs one.
var linkForwardHeaders = []string{"Content-Type", "Accept"}

// relayViaLink: dispatch a request through a registered link (virtual
// backend URL http://link/...). Mirrors s.dispatch's contract: returns
// status, header, buffered body (for small/no-stream), stream reader,
// error.
func (s *Server) relayViaLink(virtualURL, path string, r *http.Request, body []byte) (int, http.Header, []byte, io.Reader, error) {
	conn := s.linkReg.Lookup(virtualURL)
	if conn == nil {
		return http.StatusBadGateway, nil, nil, nil, fmt.Errorf("link %q is not connected", virtualURL)
	}
	// virtual URL http://link/<agent>:<port> → the agent relays to its
	// OWN loopback engine on <port>.
	engineHost := linkEngineHost(virtualURL)
	fwd := http.Header{}
	for _, k := range linkForwardHeaders {
		if v := r.Header.Values(k); len(v) > 0 {
			fwd[k] = append([]string(nil), v...)
		}
	}
	resp, err := conn.RelayContext(r.Context(), r.Method, engineHost, path, fwd, body)
	if err != nil {
		return http.StatusBadGateway, nil, nil, nil, err
	}
	// Non-SSE: buffer (matches s.dispatch semantics for failover).
	// SSE/streams: hand the pipe reader to s.relay directly.
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "event-stream") || strings.Contains(ct, "octet-stream") {
		return resp.StatusCode, resp.Header, nil, resp.Body, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return http.StatusBadGateway, nil, nil, nil, err
	}
	return resp.StatusCode, resp.Header, b, nil, nil
}

// isLinkURL: virtual backend served over a link agent's socket.
func isLinkURL(u string) bool { return strings.HasPrefix(u, "http://link/") }

// linkEngineHost: the agent-local engine address behind a virtual URL
// (http://link/<agent>:<port> → 127.0.0.1:<port>).
func linkEngineHost(virtualURL string) string {
	rest := strings.TrimPrefix(virtualURL, "http://link/")
	return "127.0.0.1:" + rest[strings.LastIndex(rest, ":")+1:]
}

// backendGet: GET path from a backend, directly or over its link. Every
// metrics/health fetch goes through here so link engines are polled,
// scored and metered exactly like LAN backends.
func (s *Server) backendGet(backend, path string, timeout time.Duration) (int, []byte, error) {
	if isLinkURL(backend) {
		conn := s.linkReg.Lookup(backend)
		if conn == nil {
			return 0, nil, fmt.Errorf("link %s is not connected", backend)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		resp, err := conn.RelayContext(ctx, http.MethodGet, linkEngineHost(backend), path,
			http.Header{"Accept": {"application/json"}}, nil)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return resp.StatusCode, b, err
	}
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(backend + path)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, err
}

// backendJSON: backendGet decoded as a JSON object (200 only).
func (s *Server) backendJSON(backend, path string, timeout time.Duration) (map[string]any, bool) {
	status, b, err := s.backendGet(backend, path, timeout)
	if err != nil || status != http.StatusOK {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	return m, true
}

// handleAdminLinks: GET /admin/links — live link agents and the health of
// each engine they serve (admin only). The operator's view for moving
// GPUs behind links: is the agent connected, is each engine polled and
// up, which pools reference it.
func (s *Server) handleAdminLinks(w http.ResponseWriter, r *http.Request) {
	if !s.adminGate(w, r) { // same gate as /backends and /usage
		return
	}
	inPools := s.linkPoolMembership()

	type engineView struct {
		URL            string         `json:"url"`
		ModelID        string         `json:"model_id"`
		MaxConcurrency int            `json:"max_concurrency"`
		Down           bool           `json:"down"`
		Polled         bool           `json:"polled"`
		Running        int            `json:"running"`
		Lanes          int            `json:"lanes"`
		Score          float64        `json:"score"`
		Pools          []string       `json:"pools"`
		CardSummary    map[string]any `json:"card_summary,omitempty"`
	}
	type agentView struct {
		Agent      string       `json:"agent"`
		Owner      string       `json:"owner"`
		Version    string       `json:"agent_version"`
		RemoteAddr string       `json:"remote_addr"`
		Since      time.Time    `json:"connected_since"`
		InFlight   int          `json:"in_flight"`
		Engines    []engineView `json:"engines"`
	}
	out := []agentView{}
	live := map[string]bool{}
	for _, c := range s.linkReg.Conns() {
		av := agentView{Agent: c.Agent, Owner: c.Owner, Version: c.Version,
			RemoteAddr: c.RemoteAddr, Since: c.Since.UTC(), InFlight: c.InFlight()}
		for _, e := range c.Engines() {
			u := link.VirtualURL(c.Agent, e.Port)
			live[u] = true
			ev := engineView{URL: u, ModelID: e.ModelID, MaxConcurrency: e.MaxConcurrency,
				Down: s.tracker.IsDown(u), Score: round3(s.tracker.Score(u)), Pools: inPools[u],
				CardSummary: cardSummary(s.engineInfo(u).card)}
			if l := s.tracker.Get(u); l != nil {
				ev.Polled, ev.Running, ev.Lanes = true, l.Running, l.Lanes
			}
			av.Engines = append(av.Engines, ev)
		}
		out = append(out, av)
	}
	// Pool members pointing at links that are not connected right now.
	missing := []string{}
	for u := range inPools {
		if !live[u] {
			missing = append(missing, u)
		}
	}
	sort.Strings(missing)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agents":                  out,
		"pool_members_not_linked": missing,
		"min_agent_version":       link.MinAgentVersion,
	})
}

// poolLinkURLs: live link engines that some pool references — shared
// serving infrastructure, listed alongside LAN backends in views any user
// can see. Links no pool uses (e.g. a user's private GPU) stay out.
func (s *Server) poolLinkURLs() []string {
	inPool := s.linkPoolMembership()
	var out []string
	for _, u := range s.linkReg.LiveURLs() {
		if len(inPool[u]) > 0 {
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// linkBackendInfo: the /backends-style info for a live link engine.
func (s *Server) linkBackendInfo(u string) *BackendInfo {
	conn := s.linkReg.Lookup(u)
	if conn == nil {
		return nil
	}
	for _, e := range conn.Engines() {
		if link.VirtualURL(conn.Agent, e.Port) == u {
			return &BackendInfo{Models: []string{e.ModelID}, Chat: true}
		}
	}
	return nil
}
