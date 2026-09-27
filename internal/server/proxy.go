package server

import (
	"bytes"
	"context"
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

	// Virtual link backends (http://link/...) relay over the agent socket
	// instead of dialing.
	if strings.HasPrefix(url, "http://link/") {
		return s.relayViaLink(url, r.URL.Path, r, body)
	}

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
	user, keyID, model string, est int, backendURL string) {

	copyHeader(w.Header(), hdr)
	w.WriteHeader(status)
	if stream != nil {
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32*1024)
		var captured ringBuf // capture LAST 1MB (usage+timings live at stream end)
		strip := wantsUsageStrip(r)
		filter := newSSEFilter(strip)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				out := filter.write(buf[:n])
				if len(out) > 0 {
					w.Write(out)
					if f := flusher; f != nil {
						f.Flush()
					}
				}
				captured.write(buf[:n])
			}
			if err != nil {
				if tail := filter.flush(); len(tail) > 0 {
					w.Write(tail)
					if f := flusher; f != nil {
						f.Flush()
					}
				}
				break
			}
		}
		pt, ot, cached := usageFromSSE(captured.bytes(), est)
		if status < 400 { // failed requests don't burn quota or count as usage
			s.usage.RecordDetailed(user, keyID, model, pt, ot, cached)
		}
		s.observePeak(model, backendURL, captured.bytes(), false)
		return
	}
	w.Write(stripIfInjected(r, buffered))
	pt, ot, cached := usageFromJSON(buffered, est)
	if status < 400 {
		s.usage.RecordDetailed(user, keyID, model, pt, ot, cached)
	}
	s.observePeak(model, backendURL, buffered, true)
}

// observePeak feeds the engine's timings block (final SSE chunk or embedded
// JSON) into the peak-throughput memory that anchors capacity pricing.
func (s *Server) observePeak(modelID, backendURL string, body []byte, isJSON bool) {
	var tg, pp, prompt float64
	if isJSON {
		var resp struct {
			Timings struct {
				PredictedPerSecond float64 `json:"predicted_per_second"`
				PromptPerSecond    float64 `json:"prompt_per_second"`
				PromptN            float64 `json:"prompt_n"`
			} `json:"timings"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &resp) != nil {
			return
		}
		tg, pp = resp.Timings.PredictedPerSecond, resp.Timings.PromptPerSecond
		prompt = float64(resp.Usage.PromptTokens)
	} else {
		// Last data: line with timings. NInfer ends streams with
		// "data: [DONE]" — skip it and keep scanning backwards.
		for i := len(body) - 1; i >= 0; i-- {
			if body[i] != '\n' {
				continue
			}
			line := strings.TrimSpace(string(body[i+1:]))
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			if strings.TrimSpace(line[5:]) == "[DONE]" {
				continue
			}
			var chunk struct {
				Timings *struct {
					PredictedPerSecond float64 `json:"predicted_per_second"`
					PromptPerSecond    float64 `json:"prompt_per_second"`
					PromptN            float64 `json:"prompt_n"`
				} `json:"timings"`
				Usage *struct {
					PromptTokens int `json:"prompt_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(line[5:]), &chunk) == nil && chunk.Timings != nil {
				tg = chunk.Timings.PredictedPerSecond
				pp = chunk.Timings.PromptPerSecond
				if chunk.Usage != nil {
					prompt = float64(chunk.Usage.PromptTokens)
				}
			}
			break
		}
	}
	if tg <= 0 && pp <= 0 {
		return
	}
	if prompt <= 0 {
		prompt = 1 // unknown prompt size: <8k bucket (honest floor)
	}
	s.peaks.Observe(backendURL, prompt, tg, pp)
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

// usageFromJSON extracts prompt/completion tokens (and engine-reported
// cache reuse) from a non-stream response.
func usageFromJSON(body []byte, est int) (int, int, int) {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) == nil && resp.Usage.PromptTokens > 0 {
		return resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.PromptDetails.CachedTokens
	}
	return est, 0, 0
}

type ctxKeyUsageHint struct{}

// withUsageHintCtx marks a request whose upstream stream got the injected
// usage chunk (so relay can strip it before returning to the client).
func withUsageHintCtx(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeyUsageHint{}, true))
}

func wantsUsageStrip(r *http.Request) bool {
	v, ok := r.Context().Value(ctxKeyUsageHint{}).(bool)
	return ok && v
}

// withUsageHint: for streaming chat requests, ask the engine for the
// usage chunk (stream_options.include_usage). If the client didn't ask
// for it, the gateway strips the extra usage chunk before relaying (the
// engine emits it as a choices-empty final chunk). Non-streaming bodies
// pass through unchanged.
func withUsageHint(body []byte) []byte {
	var req struct {
		Stream        bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(body, &req) != nil || !req.Stream || req.StreamOptions != nil {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["stream_options"] = map[string]any{"include_usage": true}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// stripUsageChunk: remove the trailing choices-empty usage chunk from an
// SSE stream when the client did not request include_usage.
func stripUsageChunk(captured []byte) []byte {
	lines := strings.Split(string(captured), "\n")
	out := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "data:") {
			p := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
			if p != "" && p != "[DONE]" {
				var ch struct {
					Choices []any `json:"choices"`
				}
				if json.Unmarshal([]byte(p), &ch) == nil && len(ch.Choices) == 0 {
					continue // usage-only chunk injected by us
				}
			}
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// ringBuf: fixed-capacity tail buffer (keeps the most recent bytes).
type ringBuf struct {
	buf []byte
	cap int
}

func (r *ringBuf) write(p []byte) {
	const capBytes = 1 << 20
	if r.cap == 0 {
		r.cap = capBytes
	}
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		// Drop the head by slicing (no copy); compaction happens at
		// read time — bytes() copies only when it would be viewed.
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
}

func (r *ringBuf) bytes() []byte { return r.buf }

// sseFilter: pass-through pipe that removes exactly one SSE event kind —
// a data event with an empty choices array and a usage block (the chunk
// injected by withUsageHint). Buffers partial lines across reads.
type sseFilter struct {
	strip bool
	carry []byte // partial line carried between reads
}

func newSSEFilter(strip bool) *sseFilter { return &sseFilter{strip: strip} }

func (f *sseFilter) write(p []byte) []byte {
	if !f.strip {
		return p
	}
	data := append(f.carry, p...)
	f.carry = nil
	// Split into complete lines; the last (possibly partial) line carries.
	lines := strings.SplitAfter(string(data), "\n")
	out := make([]byte, 0, len(data))
	for i, line := range lines {
		isLast := i == len(lines)-1
		t := strings.TrimSpace(line)
		if isLast && t != "" {
			f.carry = append(f.carry, line...) // incomplete: carry over
			break
		}
		if f.isInjectedUsage(line) {
			continue
		}
		out = append(out, line...)
	}
	return out
}

func (f *sseFilter) flush() []byte {
	if len(f.carry) == 0 {
		return nil
	}
	tail := f.carry
	f.carry = nil
	if f.isInjectedUsage(string(tail)) {
		return nil
	}
	return tail
}

// isInjectedUsage: a data line whose event has empty choices + a usage block.
func (f *sseFilter) isInjectedUsage(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "data:") {
		return false
	}
	p := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
	if p == "" || p == "[DONE]" {
		return false
	}
	var ch struct {
		Choices []any `json:"choices"`
		Usage   *struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	return json.Unmarshal([]byte(p), &ch) == nil &&
		len(ch.Choices) == 0 && ch.Usage != nil
}

// usageFromSSE extracts usage from the captured tail. Sources in priority
// order: (1) the OpenAI usage chunk (present when stream_options
// include_usage is set), (2) the engine's timings block (prompt_n /
// predicted_n / cache_n — NInfer always sends it in the final chunk),
// (3) rough estimate. reasoning_content counts toward output.
func usageFromSSE(captured []byte, est int) (int, int, int) {
	pt, ot, cached := 0, 0, 0
	var tPrompt, tPredicted, tCache float64
	haveTimings := false
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
				PromptDetails    struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Timings *struct {
				PromptN    int `json:"prompt_n"`
				PredictedN int `json:"predicted_n"`
				CacheN     int `json:"cache_n"`
			} `json:"timings"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Timings != nil && chunk.Timings.PromptN > 0 {
			// keep the LAST timings seen (final chunk is authoritative)
			tPrompt = float64(chunk.Timings.PromptN)
			tPredicted = float64(chunk.Timings.PredictedN)
			tCache = float64(chunk.Timings.CacheN)
			haveTimings = true
		}
		if chunk.Usage != nil && chunk.Usage.PromptTokens > 0 {
			pt, ot = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
			cached = chunk.Usage.PromptDetails.CachedTokens
		}
		for _, ch := range chunk.Choices {
			// chars/4 ≈ tokens (the fallback only runs when the engine
			// sent no usage chunk and no timings)
			ot += (len([]rune(ch.Delta.Content)) + len([]rune(ch.Delta.Reasoning))) / 4
		}
	}
	// Priority: (1) the OpenAI usage block — always present now that we
	// request include_usage, and prompt_tokens is the FULL prompt;
	// (2) engine timings — exact but prompt_n counts only the UNCACHED
	// suffix, so prompt = prompt_n + cache_n; (3) estimate.
	if pt > 0 {
		return pt, ot, cached
	}
	if haveTimings {
		return int(tPrompt + tCache), int(tPredicted), int(tCache)
	}
	if pt == 0 {
		pt = est
	}
	return pt, ot, cached
}

// proxy handles non-pool direct requests (SPEC §2) with usage recording.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, url string, body []byte,
	user, keyID, reqModel, modelID string) {
	needsHint := streamRequested(body) && !clientAskedIncludeUsage(body)
	if needsHint {
		r = withUsageHintCtx(r)
		body = withUsageHint(body)
	}
	status, hdr, buffered, stream, err := s.dispatch(r, url, body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"message":"backend connect failed","type":"proxy_error"}}`))
		return
	}
	s.relay(w, r, hdr, stripIfInjected(r, buffered), stream, status, user, keyID, modelID, estimateFrom(body), url)
}

func streamRequested(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &req) == nil && req.Stream
}

func clientAskedIncludeUsage(body []byte) bool {
	var req struct {
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	return json.Unmarshal(body, &req) == nil && req.StreamOptions != nil && req.StreamOptions.IncludeUsage
}

func stripIfInjected(r *http.Request, buffered []byte) []byte {
	if wantsUsageStrip(r) && len(buffered) > 0 {
		return stripUsageChunk(buffered)
	}
	return buffered
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
	s.relay(w, r, hdr, buffered, stream, status, user, keyID, pair.FallbackModelID, estimateFrom(body), pair.FallbackBackend)
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
