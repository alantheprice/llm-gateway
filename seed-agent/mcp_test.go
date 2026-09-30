package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sprout-foundry/seed/core"
)

// handleRPC answers one JSON-RPC message the way a small MCP server does.
func handleRPC(t *testing.T, msg map[string]any) (any, bool) {
	id, hasID := msg["id"]
	if !hasID {
		return nil, false // notification
	}
	var result any
	switch msg["method"] {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "fake", "version": "1"}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "add", "description": "Add two numbers",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}}}}}}
	case "tools/call":
		p := msg["params"].(map[string]any)
		args := p["arguments"].(map[string]any)
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(args["a"].(float64) + args["b"].(float64))}}}
	default:
		return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "no such method"}}, true
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, true
}

// Streamable HTTP; replies to tools/call as an SSE stream (with a progress
// notification first), everything else as JSON. Requires the auth header
// and the session id after initialize.
func streamableServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(200)
			return
		}
		var msg map[string]any
		json.NewDecoder(r.Body).Decode(&msg)
		if msg["method"] != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
			w.WriteHeader(400)
			return
		}
		reply, ok := handleRPC(t, msg)
		if !ok {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-1")
		if msg["method"] == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			b, _ := json.Marshal(reply)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reply)
	}))
}

// Older HTTP+SSE transport: POST to the stream URL is refused; GET opens
// the stream, which names the message endpoint and carries the replies.
func legacyServer(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	var streams []chan []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: endpoint\ndata: /messages?sid=1\n\n")
		fl.Flush()
		ch := make(chan []byte, 8)
		mu.Lock()
		streams = append(streams, ch)
		mu.Unlock()
		for {
			select {
			case b := <-ch:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &msg)
		w.WriteHeader(202)
		if reply, ok := handleRPC(t, msg); ok {
			b, _ := json.Marshal(reply)
			mu.Lock()
			for _, ch := range streams {
				ch <- b
			}
			mu.Unlock()
		}
	})
	return httptest.NewServer(mux)
}

func runAdd(t *testing.T, cfg mcpServerCfg) {
	t.Helper()
	ex, problems := connectMCP(context.Background(), NewSearchExecutor(""), []mcpServerCfg{cfg}, true)
	defer ex.Close()
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	var names []string
	for _, tl := range ex.GetTools() {
		names = append(names, tl.Function.Name)
	}
	if strings.Join(names, ",") != "web_search,fetch_page,calc__add" {
		t.Fatalf("tools = %v", names)
	}
	out := ex.Execute(context.Background(), []core.ToolCall{{ID: "c1", Function: core.ToolCallFunction{Name: "calc__add", Arguments: `{"a":2,"b":3}`}}})
	if len(out) != 1 || out[0].Content != "5" || out[0].ToolCallID != "c1" {
		t.Fatalf("result = %+v", out)
	}
}

func TestMCPStreamableHTTP(t *testing.T) {
	srv := streamableServer(t)
	defer srv.Close()
	runAdd(t, mcpServerCfg{Name: "calc", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer secret"}})

	// Wrong credentials: reported, not fatal.
	ex, problems := connectMCP(context.Background(), NewSearchExecutor(""), []mcpServerCfg{{Name: "calc", URL: srv.URL}}, true)
	ex.Close()
	if len(problems) != 1 || !strings.Contains(problems[0], "credentials") || len(ex.GetTools()) != 2 {
		t.Fatalf("problems = %v", problems)
	}
}

func TestMCPLegacySSE(t *testing.T) {
	srv := legacyServer(t)
	defer srv.Close()
	runAdd(t, mcpServerCfg{Name: "calc", URL: srv.URL + "/sse"})
}

// Users other than admins can't reach loopback or LAN addresses.
func TestMCPRefusesPrivateAddresses(t *testing.T) {
	srv := streamableServer(t)
	defer srv.Close()
	_, problems := connectMCP(context.Background(), NewSearchExecutor(""),
		[]mcpServerCfg{{Name: "calc", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer secret"}}}, false)
	if len(problems) != 1 || !strings.Contains(problems[0], "not public") {
		t.Fatalf("problems = %v", problems)
	}
}

func TestMCPToolName(t *testing.T) {
	if n := mcpToolName("my server", "get.issue"); n != "my_server__get_issue" {
		t.Fatal(n)
	}
	if n := mcpToolName(strings.Repeat("s", 40), strings.Repeat("t", 40)); len(n) != 64 {
		t.Fatal(len(n))
	}
}
