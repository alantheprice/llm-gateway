package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// bytesReader is a tiny alias to keep imports tidy in this file.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// handleAgentChat proxies the seed-agent sidecar (SPEC §2 identity plane of
// the Python gateway: auth-gated at the gateway, sidecar loopback-only).
// SSE stream is relayed byte-for-byte; failover doesn't apply (single sidecar).
func (s *Server) handleAgentChat(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.checkAuth(w, r); !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		errBody(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.agentURL+"/v1/agent/chat", bytesReader(body))
	if err != nil {
		errBody(w, http.StatusBadGateway, "agent request build failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		// Pass the caller's key through: the sidecar calls back through the
		// gateway with it, preserving per-user usage attribution + pinning.
		req.Header.Set("Authorization", auth)
	}
	if sid := r.Header.Get("X-Session-Id"); sid != "" {
		req.Header.Set("X-Session-Id", sid)
	}

	resp, err := s.streamClient.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "agent service unavailable: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client went away
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}
