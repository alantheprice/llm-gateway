// seed-agent: Go sidecar implementing an agentic loop (web search + fetch)
// on top of the LLM Gateway, using sprout-foundry/seed.
//
// Endpoints:
//
//	POST /v1/agent/chat   body: {messages:[{role,content}...]}  -> SSE stream
//	GET  /health
//
// Auth: requires Bearer key; verified against the gateway itself (same
// store), so agent usage is attributable and billed like chat.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sprout-foundry/seed/core"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const gatewayBase = "http://127.0.0.1:8033"

// gatewayProvider implements seed's Provider interface by calling the
// LLM Gateway's OpenAI-compatible /v1/chat/completions endpoint. All agent
// LLM calls therefore pass through gateway auth, pool routing, and usage
// accounting.
type gatewayProvider struct {
	apiKey  string
	model   string
	context int
	client  *http.Client
	// onReasoning receives streamed thinking. It goes straight to the chat,
	// not through the agent's stream handler, which would prepend it to the
	// answer text in the conversation.
	onReasoning func(string)
}

type wireMessage struct {
	Role       string      `json:"role"`
	Content    string      `json:"content"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolC `json:"tool_calls,omitempty"`
}

type wireToolC struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func toWire(msgs []core.Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolC{
				ID: tc.ID, Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{tc.Function.Name, tc.Function.Arguments},
			})
		}
		out = append(out, wm)
	}
	return out
}

func (g *gatewayProvider) chatRequest(req *core.ChatRequest) (map[string]interface{}, error) {
	tools := make([]map[string]interface{}, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": t.Function.Name, "description": t.Function.Description, "parameters": t.Function.Parameters},
		})
	}
	body := map[string]interface{}{
		"model":       g.model,
		"messages":    toWire(req.Messages),
		"max_tokens":  req.MaxTokens,
		"temperature": 0.6,
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	return body, nil
}

func (g *gatewayProvider) Chat(ctx context.Context, req *core.ChatRequest) (*core.ChatResponse, error) {
	body, err := g.chatRequest(req)
	if err != nil {
		return nil, err
	}
	blob, _ := json.Marshal(body)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", gatewayBase+"/v1/chat/completions", strings.NewReader(string(blob)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, &core.TransientError{Wrapped: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &core.RateLimitError{RetryAfter: 5 * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway returned %d", resp.StatusCode)
	}
	var wire struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls,omitempty"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, err
	}
	out := &core.ChatResponse{Model: g.model}
	if len(wire.Choices) > 0 {
		c := wire.Choices[0]
		msg := core.Message{Role: "assistant", Content: c.Message.Content}
		for _, tc := range c.Message.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{
				ID: tc.ID, Type: "function",
				Function: core.ToolCallFunction{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			})
		}
		out.Choices = append(out.Choices, core.ChatChoice{Index: 0, Message: msg, FinishReason: c.FinishReason})
	}
	return out, nil
}

// ChatStream streams one model turn from the gateway: answer text and
// thinking go to the handler as they arrive (the chat shows them live), and
// tool calls are assembled from their streamed fragments for the loop.
func (g *gatewayProvider) ChatStream(ctx context.Context, req *core.ChatRequest, handler core.StreamHandler) error {
	body, err := g.chatRequest(req)
	if err != nil {
		return err
	}
	body["stream"] = true
	blob, _ := json.Marshal(body)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", gatewayBase+"/v1/chat/completions", strings.NewReader(string(blob)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return &core.TransientError{Wrapped: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return &core.RateLimitError{RetryAfter: 5 * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	msg, finish, err := readChatStream(resp.Body, handler, g.onReasoning)
	if err != nil {
		return &core.TransientError{Wrapped: err}
	}
	out := &core.ChatResponse{Model: g.model,
		Choices: []core.ChatChoice{{Index: 0, Message: msg, FinishReason: finish}}}
	handler.OnDone(out)
	return nil
}

// readChatStream parses an OpenAI-style SSE stream into one assistant
// message, forwarding text and reasoning deltas as they arrive.
func readChatStream(r io.Reader, handler core.StreamHandler, onReasoning func(string)) (core.Message, string, error) {
	type frag struct {
		id, name string
		args     strings.Builder
	}
	var content strings.Builder
	calls := map[int]*frag{}
	finish := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			return core.Message{}, "", fmt.Errorf("%s", chunk.Error.Message)
		}
		for _, c := range chunk.Choices {
			d := c.Delta
			if d.Content != "" {
				content.WriteString(d.Content)
				handler.OnContent(d.Content)
			}
			if r := d.ReasoningContent + d.Reasoning; r != "" && onReasoning != nil {
				onReasoning(r)
			}
			for _, tc := range d.ToolCalls {
				f := calls[tc.Index]
				if f == nil {
					f = &frag{}
					calls[tc.Index] = f
				}
				if tc.ID != "" {
					f.id = tc.ID
				}
				if tc.Function.Name != "" {
					f.name += tc.Function.Name
				}
				f.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return core.Message{}, "", err
	}
	msg := core.Message{Role: "assistant", Content: content.String()}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		f := calls[i]
		if f.id == "" {
			f.id = fmt.Sprintf("call_%d", i)
		}
		msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{ID: f.id, Type: "function",
			Function: core.ToolCallFunction{Name: f.name, Arguments: f.args.String()}})
	}
	return msg, finish, nil
}

func (g *gatewayProvider) Info() core.ProviderInfo {
	return core.ProviderInfo{Model: g.model, ContextSize: g.context, MaxOutputTokens: 8192}
}

func (g *gatewayProvider) EstimateTokens(req *core.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content) / 4
	}
	return n + 8
}

type ssePublisher struct {
	send func(event string, data interface{})
}

func (p *ssePublisher) Publish(eventType string, data interface{}) {
	switch eventType {
	case core.EventTypeToolStart:
		if m, ok := data.(map[string]interface{}); ok {
			p.send("tool_start", map[string]string{
				"tool":   fmt.Sprint(m["tool_name"]),
				"args":   truncate(fmt.Sprint(m["arguments"]), 160),
				"status": "running"})
		}
	case core.EventTypeToolEnd:
		if m, ok := data.(map[string]interface{}); ok {
			summary := m["result"]
			if summary == nil {
				summary = m["error"]
			}
			p.send("tool_end", map[string]string{
				"tool":    fmt.Sprint(m["tool_name"]),
				"status":  fmt.Sprint(m["status"]),
				"summary": truncate(fmt.Sprint(summary), 240)})
		}
	case core.EventTypeStreamChunk:
		// Streamed text arrives as {"chunk", "content_type": text|reasoning}.
		if m, ok := data.(map[string]interface{}); ok {
			c, _ := m["chunk"].(string)
			if c == "" {
				c, _ = m["content"].(string)
			}
			if c == "" {
				return
			}
			if m["content_type"] == "reasoning" {
				p.send("reasoning", map[string]string{"content": c})
			} else {
				p.send("content", map[string]string{"content": c})
			}
		}
	}
}

func main() {
	apiKey := os.Getenv("GATEWAY_API_KEY")
	model := env("AGENT_MODEL", "qwen3.8-27b")
	listen := env("LISTEN", "127.0.0.1:8095")
	if apiKey == "" {
		log.Fatal("GATEWAY_API_KEY required")
	}

	executor := NewSearchExecutor(os.Getenv("JINA_API_KEY"))
	_ = executor

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/agent/chat", func(w http.ResponseWriter, r *http.Request) {
		// No CORS headers on purpose: the only legitimate caller is the
		// gateway (same-origin proxy at /v1/agent/chat). Browsers must
		// never talk to this loopback sidecar cross-origin.
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Model  string `json:"model"`
			APIKey string `json:"api_key"`
			// Set by the gateway (never trusted from browsers: the sidecar is
			// loopback-only and the gateway overwrites both fields).
			MCPServers   []mcpServerCfg `json:"mcp_servers"`
			MCPNotices   []string       `json:"mcp_notices"` // connectors the gateway couldn't hand over
			AllowPrivate bool           `json:"allow_private"`
			DocsTool     bool           `json:"docs_tool"` // caller enabled document_search
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			in.APIKey = strings.TrimPrefix(auth, "Bearer ")
		}
		if in.APIKey == "" {
			http.Error(w, `{"error":"api key required"}`, http.StatusUnauthorized)
			return
		}
		// per-request provider: agent LLM calls carry the CALLER's key so
		// gateway usage attribution lands on the right user
		// The model the user picked in chat; the configured default otherwise.
		useModel := model
		if in.Model != "" {
			useModel = in.Model
		}
		provider := &gatewayProvider{
			apiKey: in.APIKey, model: useModel, context: 240000,
			client: &http.Client{Timeout: 300 * time.Second},
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		var sendMu sync.Mutex
		send := func(event string, data interface{}) {
			sendMu.Lock()
			defer sendMu.Unlock()
			blob, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, blob)
			flusher.Flush()
		}

		provider.onReasoning = func(r string) { send("reasoning", map[string]string{"content": r}) }

		// Per-request copy of the search executor: the document_search tool
		// is gated on this caller's preference and must use this caller's
		// key so /v1/rag/search resolves to their own documents. (The shared
		// executor's client is reused; http.Client is safe for concurrency.)
		reqExec := *executor
		reqExec.docsEnabled = in.DocsTool
		reqExec.docKey = in.APIKey

		// Remote MCP servers the user connected: their tools join the loop.
		tools, problems := connectMCP(r.Context(), &reqExec, in.MCPServers, in.AllowPrivate)
		defer tools.Close()
		for _, n := range in.MCPNotices {
			send("notice", map[string]string{"message": "Connector " + n})
		}
		for _, p := range problems {
			send("notice", map[string]string{"message": "Couldn't use MCP server " + p})
		}
		prompt := "You are a helpful assistant with web access. " +
			"Use web_search for current events or facts you are unsure of (at most 2 searches). " +
			"If a snippet answers the question, stop and answer. Only fetch_page when you need " +
			"details not in the snippets (at most 2 fetches). You MUST produce a final answer with sources cited as [n] - never end on a tool call. Be concise."
		if in.DocsTool {
			prompt += " The user has uploaded documents. Use document_search to answer questions about their own files, notes or papers; cite the document name it comes from."
		}
		if names := tools.serverNames(); len(names) > 0 {
			prompt += " You also have tools from the user's connected services (" + strings.Join(names, ", ") +
				"), named <service>__<tool>. Use them when the request involves those services; the search limits above apply only to web_search and fetch_page."
		}
		agent, err := core.NewAgent(core.Options{
			Provider:       provider,
			Executor:       tools,
			MaxIterations:  12,
			EventPublisher: &ssePublisher{send: send},
			SystemPrompt:   prompt,
		})
		if err != nil {
			send("error", map[string]string{"error": "agent init failed"})
			return
		}

		// The last user message is this turn's query; everything before it
		// is the conversation so far, loaded as the agent's history so
		// follow-up questions keep their context.
		query, last := "", -1
		for i := len(in.Messages) - 1; i >= 0; i-- {
			if in.Messages[i].Role == "user" {
				query, last = in.Messages[i].Content, i
				break
			}
		}
		if last < 0 {
			send("error", map[string]string{"error": "no user message"})
			return
		}
		agent.State().SetMessages(priorHistory(in.Messages[:last]))
		send("start", map[string]string{"model": useModel})

		result, err := agent.RunStream(r.Context(), query)
		if err != nil {
			send("error", map[string]string{"error": err.Error()})
			return
		}
		send("done", map[string]string{"content": result})
	})

	// POST /v1/mcp/tools {server, allow_private}: connect to one MCP server
	// and list its tools (the gateway's "Test" button).
	mux.HandleFunc("/v1/mcp/tools", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			Server       mcpServerCfg `json:"server"`
			AllowPrivate bool         `json:"allow_private"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		c := newMCPClient(in.Server, in.AllowPrivate)
		defer c.Close()
		w.Header().Set("Content-Type", "application/json")
		err := c.Connect(ctx)
		var tools []mcpTool
		if err == nil {
			tools, err = c.ListTools(ctx)
		}
		if err != nil {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		list := make([]map[string]string, 0, len(tools))
		for _, t := range tools {
			list = append(list, map[string]string{"name": t.Name, "description": truncate(t.Description, 300)})
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "tools": list, "protocol": c.version, "legacy_sse": c.legacy})
	})

	log.Printf("seed-agent listening on %s (model %s)", listen, model)
	srv := &http.Server{Addr: listen, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// priorHistory: earlier user/assistant turns as agent messages (system
// messages are the agent's own; empty turns carry nothing).
func priorHistory(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) []core.Message {
	out := make([]core.Message, 0, len(msgs))
	for _, m := range msgs {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			continue
		}
		out = append(out, core.Message{Role: m.Role, Content: m.Content})
	}
	return out
}
