// Content-affinity helpers for pool routing (see routing/cachetable.go).
package server

import (
	"encoding/json"
	"log"
	"net/http"

	"llmgateway/internal/config"
	"llmgateway/internal/routing"
)

// contentText flattens a message content field (string or the array-of-
// parts OpenAI form) to plain text for hashing. Only structural text is
// wanted — image parts contribute their type token only.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		out := ""
		for _, p := range parts {
			if p.Type == "text" {
				out += p.Text + "\n"
			} else if p.Type != "" {
				out += "[" + p.Type + "]\n"
			}
		}
		return out
	}
	return ""
}

// relayPoolPick: cache-affinity fast path — same dispatch/relay/failover
// contract as the classic loop, plus a cache-table record on success.
func (s *Server) relayPoolPick(w http.ResponseWriter, r *http.Request,
	modelName string, pool *config.PoolCfg, body []byte, pick routing.PickResult,
	user, keyID string, est int, convMsgs []routing.Message) {
	s.mu.Lock()
	s.leader[modelName] = pick.URL
	s.mu.Unlock()

	fwdBody := rewriteModel(body, pick.ModelID)
	status, respHeader, respBody, reader, err := s.dispatch(r, pick.URL, fwdBody)
	if err == nil && status < 500 && status != http.StatusRequestTimeout {
		s.tracker.InFlightInc(pick.URL)
		defer s.tracker.InFlightDec(pick.URL)
		s.relay(w, r, respHeader, respBody, reader, status, user, keyID, pick.ModelID, est, pick.URL)
		s.recordConv(modelName, convMsgs, pick.URL)
		return
	}
	// Cache hit missed at dispatch (backend hiccup): fall back to the
	// classic picker, which re-records the conversation on success.
	log.Printf("Pool '%s': cache-affinity pick %s failed (status %d), falling back",
		modelName, pick.URL, status)
	s.routePoolClassic(w, r, modelName, pool, body, user, keyID, est, convMsgs)
}

// recordConv: remember that this conversation's deepest prefix now lives
// on `backend`.
func (s *Server) recordConv(poolName string, convMsgs []routing.Message, backend string) {
	if s.cacheTable != nil && len(convMsgs) >= 2 {
		s.cacheTable.Record(poolName, convMsgs, backend)
	}
}

// rewriteModel swaps the pool name for the member's model id in the body.
func rewriteModel(body []byte, modelID string) []byte {
	var reqData map[string]any
	if json.Unmarshal(body, &reqData) != nil {
		return body
	}
	if old, _ := reqData["model"].(string); old != modelID {
		reqData["model"] = modelID
		if nb, err := json.Marshal(reqData); err == nil {
			return nb
		}
	}
	return body
}
