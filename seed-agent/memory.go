package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sprout-foundry/seed/core"
)

// Memory tools: memory_save and memory_search, executed on the SearchExecutor
// (the gateway's built-in tool base) so they join the agent loop the same way
// web_search / document_search do.
//
// Both call the gateway's key-auth endpoints (/v1/memories/save,
// /v1/memories/search) with the caller's key, so the memories belong to and
// are only visible to that user. They are offered only when the caller has
// enabled the memory toggle (the gateway sets memory_tool from the user's
// preference) — the same explicit-gate pattern as document_search.

// memoryTools returns the memory tool definitions (empty when the caller has
// not enabled the memory toggle). Read at GetTools time so the per-request
// flag is honoured.
func (s *SearchExecutor) memoryTools() []core.Tool {
	if !s.memoryEnabled {
		return nil
	}
	return []core.Tool{
		{Type: "function", Function: core.ToolFunction{
			Name:        "memory_save",
			Description: "Save a short memory about the user — a preference, decision, project detail, or recurring fact they should not have to repeat in future conversations. Store ONE self-contained fact (a few sentences max), written so it makes sense out of context (e.g. 'The user prefers responses in English'). Do not store secrets, passwords, or one-off conversation details. Call it only when the user states something durable or asks you to remember it.",
			Parameters: core.ToolParameters{
				Type: "object",
				Properties: map[string]core.ToolParameter{
					"text": {Type: "string", Description: "The single fact to remember, self-contained and concise"},
				},
				Required: []string{"text"},
			},
		}},
		{Type: "function", Function: core.ToolFunction{
			Name:        "memory_search",
			Description: "Search the user's saved memories for ones relevant to a topic or task. Use at the start of work when prior context (preferences, decisions, project facts) would help, or when the user references something you were told before. Returns the best-matching memories.",
			Parameters: core.ToolParameters{
				Type: "object",
				Properties: map[string]core.ToolParameter{
					"query": {Type: "string", Description: "What to look for in the memories"},
					"top_k": {Type: "integer", Description: "How many memories to return (default 5, max 20)"},
				},
				Required: []string{"query"},
			},
		}},
	}
}

func (s *SearchExecutor) gatewayPost(ctx context.Context, path string, body map[string]interface{}) (int, []byte, error) {
	blob, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", gatewayBase+path, strings.NewReader(string(blob)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.memoryKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.memoryKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

func (s *SearchExecutor) memorySave(ctx context.Context, args string) (string, error) {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Text) == "" {
		return "", fmt.Errorf("empty memory")
	}
	status, b, err := s.gatewayPost(ctx, "/v1/memories/save", map[string]interface{}{"text": p.Text})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("memory save returned %d: %s", status, truncate(string(b), 200))
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &out)
	return "Memory saved. It will be available to future conversations.", nil
}

func (s *SearchExecutor) memorySearch(ctx context.Context, args string) (string, error) {
	var p struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", err
	}
	if p.TopK <= 0 {
		p.TopK = 5
	}
	if p.TopK > 20 {
		p.TopK = 20
	}
	status, b, err := s.gatewayPost(ctx, "/v1/memories/search", map[string]interface{}{"query": p.Query, "top_k": p.TopK})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("memory search returned %d: %s", status, truncate(string(b), 200))
	}
	var out struct {
		Results []struct {
			Text  string  `json:"text"`
			Score float32 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "No saved memories matched this topic.", nil
	}
	var sb strings.Builder
	for i, r := range out.Results {
		fmt.Fprintf(&sb, "[%d] %s\n\n", i+1, strings.TrimSpace(r.Text))
	}
	sb.WriteString("\n(These are the user's own saved memories; use them as established context.)")
	return sb.String(), nil
}
