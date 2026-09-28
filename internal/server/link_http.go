package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

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
	_ = keyID
	link.ServeAgent(s.linkReg, w, r, user)
}

// authenticateLinkToken: resolve the bearer token to (user, key id);
// requires an active key with role "link" or an admin owner.
func (s *Server) authenticateLinkToken(r *http.Request) (string, string, error) {
	h := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(h, "Bearer ")
	if tok == "" {
		// also accept ?key= for agent convenience behind restrictive shells
		tok = r.URL.Query().Get("key")
	}
	if tok == "" {
		return "", "", fmt.Errorf("missing token")
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
	rest := strings.TrimPrefix(virtualURL, "http://link/")
	port := rest[strings.LastIndex(rest, ":")+1:]
	engineHost := "127.0.0.1:" + port
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
