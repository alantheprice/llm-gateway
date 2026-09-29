package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"llmgateway/internal/config"
	"llmgateway/internal/embeddedpb"
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

// requestedName: the model name the client called, else the engine's.
func requestedName(r *http.Request, model string) string {
	if cm := clientModelOf(r); cm != "" {
		return cm
	}
	return model
}

// dispatchCounted is dispatch that counts the request as in flight on url
// from the moment it is sent until release() — call release when the
// response has been relayed (or the attempt abandoned). Load scoring blends
// in-flight counts, so every route (pool, cache affinity, overflow, direct)
// must count, and from send time: a non-streamed request is otherwise
// invisible until the engine has already finished it.
func (s *Server) dispatchCounted(r *http.Request, url string, body []byte) (
	status int, hdr http.Header, buffered []byte, reader io.Reader, err error, release func()) {
	s.tracker.InFlightInc(url)
	var once sync.Once
	release = func() { once.Do(func() { s.tracker.InFlightDec(url) }) }
	status, hdr, buffered, reader, err = s.dispatch(r, url, body)
	return
}

// relay writes a backend response to the client, streaming SSE if applicable,
// and records usage once on the serving member (SPEC §8).
func (s *Server) relay(w http.ResponseWriter, r *http.Request,
	hdr http.Header, buffered []byte, stream io.Reader, status int,
	user, keyID, model string, est int, backendURL string) {

	ai := accessFrom(r)
	if ai != nil {
		ai.user, ai.model, ai.backend, ai.stream = user, model, backendURL, stream != nil
	}
	copyHeader(w.Header(), hdr)
	w.WriteHeader(status)
	if stream != nil {
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32*1024)
		var captured ringBuf // capture LAST 1MB (usage+timings live at stream end)
		strip := wantsUsageStrip(r)
		filter := newSSEFilter(strip)
		filter.model = clientModelOf(r)
		switch statsModeOf(r) {
		case statsInjected:
			filter.stripStats = true
		case statsClient:
			filter.gateway = s.gatewayBlock(backendURL, model)
		}
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				out := filter.write(buf[:n])
				if len(out) > 0 {
					if ai != nil && ai.ttft == 0 {
						// Time to first token as the user sees it: request
						// received → first streamed byte, including routing,
						// queueing, relay hops and prefill.
						ai.ttft = time.Since(ai.start)
					}
					w.Write(out)
					if f := flusher; f != nil {
						f.Flush()
					}
				}
				captured.write(buf[:n])
			}
			if err != nil {
				if ai := accessFrom(r); ai != nil && err != io.EOF {
					ai.streamErr = err.Error() // backend died or client left mid-stream
				}
				if tail := filter.flush(); len(tail) > 0 {
					w.Write(tail)
					if f := flusher; f != nil {
						f.Flush()
					}
				}
				break
			}
		}
		pt, ot, cached, extras := usageFromSSE(captured.bytes(), est)
		if ai != nil && ai.ttft > 0 {
			extras.TTFTms = float64(ai.ttft) / float64(time.Millisecond)
		}
		if status < 400 { // failed requests don't burn quota or count as usage
			s.usage.RecordDetailed(user, keyID, model, pt, ot, cached)
			s.reqLog.Add(embeddedpb.RequestRecord{
				User: user, KeyID: keyID, Model: model, Backend: backendURL,
				Kind: kindFor(model), Status: status,
				Prompt: int64(pt), Cached: int64(cached), Output: int64(ot),
				TTFTms: extras.TTFTms, Prefillms: extras.Prefillms,
				Decodems: extras.Decodems, Totalms: extras.Totalms,
				TokPerSec: extras.TokPerSec, ReusePath: extras.ReusePath,
				DraftN: extras.DraftN, DraftAccepted: extras.DraftAccepted,
				QueueWaitms: extras.QueueWaitms, Requested: requestedName(r, model),
			})
		}
		if extras.QueueWaitms > 0 {
			s.tracker.ObserveQueueWait(backendURL, extras.QueueWaitms)
		}
		s.observePeak(model, backendURL, captured.bytes(), false)
		return
	}
	out := s.finalizeJSONStats(r, stripIfInjected(r, buffered), backendURL, model)
	if cm := clientModelOf(r); cm != "" && status < 400 {
		out = setModelField(out, cm)
	}
	if rp := revisedPromptOf(r); rp != "" && status < 400 {
		out = withRevisedPrompt(out, rp)
	}
	w.Write(out)
	pt, ot, cached, extras := usageFromJSON(buffered, est)
	if extras.TTFTms == 0 {
		// Non-streamed: nothing reaches the client before the end. Prefer
		// the engine's own ttft (stats block); else its prefill time.
		extras.TTFTms = extras.EngineTTFTms
		if extras.TTFTms == 0 {
			extras.TTFTms = extras.Prefillms
		}
	}
	if status < 400 {
		s.usage.RecordDetailed(user, keyID, model, pt, ot, cached)
		s.reqLog.Add(embeddedpb.RequestRecord{
			User: user, KeyID: keyID, Model: model, Backend: backendURL,
			Kind: kindFor(model), Status: status,
			Prompt: int64(pt), Cached: int64(cached), Output: int64(ot),
			TTFTms: extras.TTFTms, Prefillms: extras.Prefillms,
			Decodems: extras.Decodems, Totalms: extras.Totalms,
			TokPerSec: extras.TokPerSec, ReusePath: extras.ReusePath,
			DraftN: extras.DraftN, DraftAccepted: extras.DraftAccepted,
			QueueWaitms: extras.QueueWaitms, Requested: requestedName(r, model),
		})
	}
	if extras.QueueWaitms > 0 {
		s.tracker.ObserveQueueWait(backendURL, extras.QueueWaitms)
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
func usageFromJSON(body []byte, est int) (int, int, int, SSEExtras) {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Timings struct {
			PromptMS    float64 `json:"prompt_ms"`
			PredictedMS float64 `json:"predicted_ms"`
			TTFTS       float64 `json:"ttft_s"`
			ReusePath   string  `json:"reuse_path"`
			DraftN      int64   `json:"draft_n"`
			DraftAcc    int64   `json:"draft_n_accepted"`
		} `json:"timings"`
		Stats *engineStats `json:"stats"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage.PromptTokens <= 0 {
		return est, 0, 0, SSEExtras{}
	}
	extras := SSEExtras{
		Prefillms:     resp.Timings.PromptMS,
		Decodems:      resp.Timings.PredictedMS,
		Totalms:       resp.Timings.PromptMS + resp.Timings.PredictedMS,
		TTFTms:        resp.Timings.TTFTS * 1000,
		ReusePath:     resp.Timings.ReusePath,
		DraftN:        resp.Timings.DraftN,
		DraftAccepted: resp.Timings.DraftAcc,
	}
	applyEngineStats(&extras, resp.Stats)
	if resp.Timings.PredictedMS > 0 {
		extras.TokPerSec = float64(resp.Usage.CompletionTokens) /
			(resp.Timings.PredictedMS / 1000)
	}
	return resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.PromptDetails.CachedTokens, extras
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
	strip      bool           // drop the usage chunk the gateway injected
	stripStats bool           // drop the stats chunk the gateway injected
	gateway    map[string]any // client asked for stats: add this block to the stats chunk
	model      string         // client-requested model name to put in each chunk
	carry      []byte         // partial line carried between reads
}

func newSSEFilter(strip bool) *sseFilter { return &sseFilter{strip: strip} }

func (f *sseFilter) active() bool {
	return f.strip || f.stripStats || f.gateway != nil || f.model != ""
}

func (f *sseFilter) write(p []byte) []byte {
	if !f.active() {
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
		out = append(out, f.transform(line)...)
	}
	return out
}

func (f *sseFilter) flush() []byte {
	if len(f.carry) == 0 {
		return nil
	}
	tail := f.carry
	f.carry = nil
	return []byte(f.transform(string(tail)))
}

// transform applies the filter to one SSE line: drops injected usage/stats
// chunks, or adds the gateway block to a client-requested stats chunk.
func (f *sseFilter) transform(line string) string {
	if f.strip && f.isInjectedUsage(line) {
		return ""
	}
	if f.model != "" && strings.HasPrefix(strings.TrimSpace(line), "data:") {
		line = string(setModelField([]byte(line), f.model))
	}
	if !f.stripStats && f.gateway == nil {
		return line
	}
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "data:") {
		return line
	}
	p := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
	if p == "" || p == "[DONE]" || !strings.Contains(p, `"stats"`) {
		return line
	}
	var m map[string]any
	if json.Unmarshal([]byte(p), &m) != nil {
		return line
	}
	if _, has := m["stats"]; !has {
		return line
	}
	if choices, _ := m["choices"].([]any); len(choices) > 0 {
		return line // stats only ride on the terminal choices-empty chunk
	}
	if f.stripStats {
		delete(m, "stats")
		if _, hasUsage := m["usage"]; !hasUsage || f.strip {
			return "" // nothing left the client asked for
		}
	} else {
		m["gateway"] = f.gateway
	}
	b, err := json.Marshal(m)
	if err != nil {
		return line
	}
	return "data: " + string(b) + "\n"
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

// SSEExtras: request telemetry beyond tokens, from the final chunk.
type SSEExtras struct {
	TTFTms    float64
	Prefillms float64
	Decodems  float64
	Totalms   float64
	TokPerSec float64
	ReusePath string
	// Speculative decoding: tokens drafted / accepted (engine timings
	// draft_n / draft_n_accepted; 0 when the engine runs no draft model).
	DraftN        int64
	DraftAccepted int64
	// From the engine's opt-in "stats" block (return_stats).
	EngineTTFTms float64
	QueueWaitms  float64
}

// usageFromSSE extracts usage from the captured tail. Sources in priority
// order: (1) the OpenAI usage chunk (present when stream_options
// include_usage is set), (2) the engine's timings block (prompt_n /
// predicted_n / cache_n — NInfer always sends it in the final chunk),
// (3) rough estimate. reasoning_content counts toward output.
// extras carries the timing telemetry when present.
func usageFromSSE(captured []byte, est int) (int, int, int, SSEExtras) {
	pt, ot, cached := 0, 0, 0
	var tPrompt, tPredicted, tCache float64
	var extras SSEExtras
	var stats *engineStats // opt-in engine stats chunk (return_stats)
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
				PromptN     int     `json:"prompt_n"`
				PredictedN  int     `json:"predicted_n"`
				CacheN      int     `json:"cache_n"`
				PromptMS    float64 `json:"prompt_ms"`
				PredictedMS float64 `json:"predicted_ms"`
				TTFTS       float64 `json:"ttft_s"`
				ReusePath   string  `json:"reuse_path"`
				DraftN      int64   `json:"draft_n"`
				DraftAcc    int64   `json:"draft_n_accepted"`
			} `json:"timings"`
			Stats   *engineStats `json:"stats"`
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
			extras.Prefillms = chunk.Timings.PromptMS
			extras.Decodems = chunk.Timings.PredictedMS
			extras.Totalms = chunk.Timings.PromptMS + chunk.Timings.PredictedMS
			if chunk.Timings.PredictedMS > 0 && chunk.Timings.PredictedN > 0 {
				extras.TokPerSec = float64(chunk.Timings.PredictedN) /
					(chunk.Timings.PredictedMS / 1000)
			}
			haveTimings = true
		}
		if chunk.Timings != nil && chunk.Timings.TTFTS > 0 {
			extras.TTFTms = chunk.Timings.TTFTS * 1000
		}
		if chunk.Stats != nil {
			stats = chunk.Stats
		}
		if chunk.Timings != nil && chunk.Timings.DraftN > 0 {
			extras.DraftN, extras.DraftAccepted = chunk.Timings.DraftN, chunk.Timings.DraftAcc
		}
		if chunk.Timings != nil && chunk.Timings.ReusePath != "" {
			extras.ReusePath = chunk.Timings.ReusePath
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
	applyEngineStats(&extras, stats)
	// Priority: (1) the OpenAI usage block — always present now that we
	// request include_usage, and prompt_tokens is the FULL prompt;
	// (2) engine timings — exact but prompt_n counts only the UNCACHED
	// suffix, so prompt = prompt_n + cache_n; (3) estimate.
	if pt > 0 {
		return pt, ot, cached, extras
	}
	if haveTimings {
		return int(tPrompt + tCache), int(tPredicted), int(tCache), extras
	}
	if pt == 0 {
		pt = est
	}
	return pt, ot, cached, extras
}

// proxy handles non-pool direct requests (SPEC §2) with usage recording.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, url string, body []byte,
	user, keyID, reqModel, modelID string) {
	needsHint := streamRequested(body) && !clientAskedIncludeUsage(body)
	if needsHint {
		r = withUsageHintCtx(r)
		body = withUsageHint(body)
	}
	r, body = s.prepareStats(r, body, url) // per-response engine stats
	status, hdr, buffered, stream, err, release := s.dispatchCounted(r, url, body)
	defer release()
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away
		}
		log.Printf("backend %s connect failed (%v), marking down", url, err)
		s.tracker.MarkDown(url)
		poolUnavailable(w) // single-backend model: same 503 + Retry-After
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
	urls = append(urls, s.poolLinkURLs()...)
	for _, u := range urls {
		if m, ok := s.backendJSON(u, "/slots", 3*time.Second); ok {
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
		if m, ok := s.backendJSON(u, "/usage", 3*time.Second); ok {
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
