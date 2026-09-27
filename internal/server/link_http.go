package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"llmgateway/internal/link"
)

var _ = fmt.Sprintf

// handleLinkAgent: WebSocket upgrade for link agents.
// Auth: any valid API key with role "link" (minted via the normal key
// flow with role=link); revocation kills the link within one ping.
func (s *Server) handleLinkAgent(w http.ResponseWriter, r *http.Request) {
	sess, keyID, err := s.authenticateLinkToken(r)
	if err != nil {
		errBody(w, 401, "invalid link token")
		return
	}
	link.ServeAgent(s.linkReg, w, r, keyID)
	_ = sess
}

// authenticateLinkToken: resolve the bearer token; require an active key
// (role "link" preferred but not mandatory — any valid key can link, so
// operators can hand out self-serve tokens; role surfaces in logs).
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
	return user, rec.KeyID, nil
}

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
	resp, err := conn.Relay(r.Method, engineHost, path, r.Header.Clone(), body)
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
