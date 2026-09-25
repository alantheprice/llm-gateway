package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"llmgateway/internal/config"
	"llmgateway/internal/routing"
)

type poolCfgT = config.PoolCfg

func routingEstimate(msgs []map[string]any) int { return routing.EstimateTokens(msgs) }

// estimateFrom extracts est tokens from a chat body (SPEC §5.1).
func estimateFrom(body []byte) int {
	var req struct {
		Messages []map[string]any `json:"messages"`
		Prompt   string           `json:"prompt"`
	}
	if json.Unmarshal(body, &req) != nil {
		return 1
	}
	if len(req.Messages) > 0 {
		return routingEstimate(req.Messages)
	}
	if req.Prompt != "" {
		n := len([]rune(req.Prompt)) / 4
		if n < 1 {
			n = 1
		}
		return n
	}
	return 1
}

// sessionKey implements SPEC §5.2.
func sessionKey(r *http.Request, keyID string) string {
	if sid := strings.TrimSpace(r.Header.Get("X-Session-Id")); sid != "" {
		if len(sid) > 120 {
			sid = sid[:120]
		}
		return "hdr:" + sid
	}
	if keyID != "" {
		return "key:" + keyID
	}
	return ""
}

// dispatch sends the request to a backend, buffering just enough to know the
// status + content-type before anything reaches the client. For event-stream
// responses, respBody is nil and reader streams the remainder.
func (s *Server) dispatch(r *http.Request, url string, body []byte) (
	status int, hdr http.Header, buffered []byte, reader io.Reader, err error) {

	req, err := http.NewRequestWithContext(r.Context(), r.Method, url+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	// No overall client timeout: SSE generations run minutes. Failover
	// detection uses the status/CT available as soon as headers arrive.
	resp, err := s.streamClient.Do(req)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	hdr = resp.Header
	status = resp.StatusCode
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Stream: read nothing extra; reader is the live body.
		return status, hdr, nil, resp.Body, nil
	}
	buf := &bytes.Buffer{}
	tee := io.TeeReader(resp.Body, buf)
	rest, _ := io.ReadAll(tee)
	resp.Body.Close()
	return status, hdr, rest, nil, nil
}

// relay writes a backend response to the client, streaming SSE if applicable,
// and records usage once on the serving member (SPEC §8).
func (s *Server) relay(w http.ResponseWriter, r *http.Request,
	hdr http.Header, buffered []byte, stream io.Reader, status int,
	user, keyID, model string, est int) {

	copyHeader(w.Header(), hdr)
	w.WriteHeader(status)
	if stream != nil {
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32*1024)
		var captured []byte
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if f := flusher; f != nil {
					f.Flush()
				}
				if len(captured) < 1<<20 { // capture ≤1MB tail for usage
					captured = append(captured, buf[:n]...)
				}
			}
			if err != nil {
				break
			}
		}
		pt, ot := usageFromSSE(captured, est)
		s.usage.Record(user, keyID, model, pt, ot)
		return
	}
	w.Write(buffered)
	pt, ot := usageFromJSON(buffered, est)
	s.usage.Record(user, keyID, model, pt, ot)
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if k == "Content-Length" || k == "Connection" || k == "Transfer-Encoding" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// usageFromJSON extracts prompt/completion tokens from a non-stream response.
func usageFromJSON(body []byte, est int) (int, int) {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) == nil && resp.Usage.PromptTokens > 0 {
		return resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	}
	return est, 0
}

// usageFromSSE scans captured SSE text for the OpenAI usage chunk; falls
// back to counting completion tokens from delta content + est prompt.
func usageFromSSE(captured []byte, est int) (int, int) {
	pt, ot := 0, 0
	for _, line := range strings.Split(string(captured), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			pt, ot = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		for _, ch := range chunk.Choices {
			ot4 := len([]rune(ch.Delta.Content)) // rough if usage absent
			if ot4 > 0 && pt == 0 {
				ot += ot4/4 + 1
			}
		}
	}
	if pt == 0 {
		pt = est
	}
	return pt, ot
}

// proxy handles non-pool direct requests (SPEC §2) with usage recording.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, url string, body []byte,
	user, keyID, reqModel, modelID string) {
	status, hdr, buffered, stream, err := s.dispatch(r, url, body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"message":"backend connect failed","type":"proxy_error"}}`))
		return
	}
	s.relay(w, r, hdr, buffered, stream, status, user, keyID, modelID, estimateFrom(body))
}

// tryOverflow implements SPEC §9: if primary backend score >= threshold,
// use the fallback backend/model. Returns true if it handled the response.
func (s *Server) tryOverflow(w http.ResponseWriter, r *http.Request, model string,
	pair *config.OverflowPair, body []byte, user, keyID string) bool {

	url, _ := s.resolve(model)
	if score := s.tracker.Score(url); score < pair.Threshold {
		return false // primary has headroom; caller proxies directly
	} // Rewrite model id to fallback and dispatch there.
	var req map[string]any
	if json.Unmarshal(body, &req) == nil {
		req["model"] = pair.FallbackModelID
		if nb, err := json.Marshal(req); err == nil {
			body = nb
		}
	}
	status, hdr, buffered, stream, err := s.dispatch(r, pair.FallbackBackend, body)
	if err != nil || status >= 500 {
		return false // fall through to direct attempt
	}
	s.relay(w, r, hdr, buffered, stream, status, user, keyID, pair.FallbackModelID, estimateFrom(body))
	return true
}

// --- observability (SPEC §12) ---



func (s *Server) handleSlots(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.checkAuth(w, r); !ok {
		return
	}
	out := map[string]any{}
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	for u := range s.backends {
		urls = append(urls, u)
	}
	s.mu.Unlock()
	for _, u := range urls {
		if m, ok := getJSON(s.client, u+"/slots", 3*time.Second); ok {
			out[u] = m
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}


// backendSnapshot polls fresh and returns per-backend score/load info.
func (s *Server) backendSnapshot() map[string]map[string]any {
	s.PollOnce()
	backends := map[string]map[string]any{}
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	for u := range s.backends {
		urls = append(urls, u)
	}
	s.mu.Unlock()
	for _, u := range urls {
		l := s.tracker.Get(u)
		if l == nil {
			continue
		}
		backends[u] = map[string]any{
			"engine":           l.Engine,
			"score":            round3(s.tracker.Score(u)),
			"lanes":            l.Lanes,
			"running":          l.Running,
			"waiting":          l.Waiting,
			"tps":              round3(l.DecodeTPS),
			"energy_daily_kwh": round3(l.EnergyKWH),
			"cache_hit_pct":    round3(l.CacheHitPct),
			"last_updated":     l.LastUpdated.UTC().Format(time.RFC3339),
		}
	}
	return backends
}

// usageMerge fetches fresh /usage payloads from all backends keyed by URL.
func (s *Server) usageMerge() map[string]map[string]any {
	s.PollOnce()
	out := map[string]map[string]any{}
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	for u := range s.backends {
		urls = append(urls, u)
	}
	s.mu.Unlock()
	for _, u := range urls {
		if m, ok := getJSON(s.client, u+"/usage", 3*time.Second); ok {
			out[u] = m
		}
	}
	return out
}

func round3(f float64) float64 {
	if f == 0 {
		return 0
	}
	return float64(int(f*1000+0.5*sign(f))) / 1000
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

var _ = fmt.Sprintf
