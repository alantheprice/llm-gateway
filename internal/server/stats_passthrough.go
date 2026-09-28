package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Per-response stats (NInfer serving standard): an engine given
// "return_stats": true adds a "stats" object to the response (non-streaming
// body) or a final choices-empty "stats" chunk (stream) with the engine's
// own ttft_ms, queue_wait_ms, prefix_reuse_path, speculative acceptance, …
//
// The gateway asks every engine that serves a model card (the same builds
// support stats) so it can record engine-side timings, and:
//   - statsInjected: the client did not ask → the stats are stripped before
//     the response reaches the client (like the injected usage chunk);
//   - statsClient: the client asked → stats pass through with a "gateway"
//     block naming the GPU that served the request and the path to it.
// Engines without cards are never sent the field (some reject unknown
// parameters).

type statsMode int

const (
	statsNone statsMode = iota
	statsInjected
	statsClient
)

type ctxKeyStats struct{}

func statsModeOf(r *http.Request) statsMode {
	m, _ := r.Context().Value(ctxKeyStats{}).(statsMode)
	return m
}

// supportsStats: the backend serves a NInfer-standard model card.
func (s *Server) supportsStats(backend string) bool {
	card := s.engineInfo(backend).card
	schema, _ := card["schema"].(string)
	return strings.HasPrefix(schema, "ninfer-model-card/")
}

// prepareStats decides the stats mode for one dispatch to `backend` and, when
// the gateway injects the request, returns the rewritten body. Only chat
// completions carry stats.
func (s *Server) prepareStats(r *http.Request, body []byte, backend string) (*http.Request, []byte) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		return r, body
	}
	var probe struct {
		ReturnStats *bool `json:"return_stats"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return r, body
	}
	mode := statsNone
	switch {
	case probe.ReturnStats != nil && *probe.ReturnStats:
		mode = statsClient
	case probe.ReturnStats == nil && s.supportsStats(backend):
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			m["return_stats"] = true
			if nb, err := json.Marshal(m); err == nil {
				body, mode = nb, statsInjected
			}
		}
	}
	if mode == statsNone {
		return r, body
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKeyStats{}, mode)), body
}

// gatewayBlock: what the gateway adds to client-requested stats.
func (s *Server) gatewayBlock(backend, model string) map[string]any {
	id := s.gpuIdentity(backend)
	label := id.Label
	if id.Host == "" {
		label = "unlisted GPU" // don't expose internal addresses
	}
	return map[string]any{"gpu_label": label, "via": id.Via, "served_model": model}
}

// finalizeJSONStats applies the stats mode to a non-streaming response body.
func (s *Server) finalizeJSONStats(r *http.Request, body []byte, backend, model string) []byte {
	mode := statsModeOf(r)
	if mode == statsNone || len(body) == 0 {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, has := m["stats"]; !has {
		return body
	}
	if mode == statsInjected {
		delete(m, "stats")
	} else {
		m["gateway"] = s.gatewayBlock(backend, model)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// statsFromEvent: the engine stats object in one parsed SSE payload or JSON body.
type engineStats struct {
	TTFTms      float64 `json:"ttft_ms"`
	QueueWaitms float64 `json:"queue_wait_ms"`
	ReusePath   string  `json:"prefix_reuse_path"`
	Speculative *struct {
		DraftTokens    int64 `json:"draft_tokens"`
		AcceptedTokens int64 `json:"accepted_tokens"`
	} `json:"speculative"`
}

// applyEngineStats folds engine stats into the request telemetry. Engine
// timings are authoritative for queue wait and reuse path; draft counts fill
// in only when the timings block did not carry them.
func applyEngineStats(ex *SSEExtras, st *engineStats) {
	if st == nil {
		return
	}
	ex.EngineTTFTms = st.TTFTms
	ex.QueueWaitms = st.QueueWaitms
	if st.ReusePath != "" {
		ex.ReusePath = st.ReusePath
	}
	if st.Speculative != nil && ex.DraftN == 0 {
		ex.DraftN, ex.DraftAccepted = st.Speculative.DraftTokens, st.Speculative.AcceptedTokens
	}
}
