package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sprout-foundry/seed/core"
)

// SearchExecutor implements core.ToolExecutor with Jina-powered tools:
//
//	web_search: search the internet (Jina search API)
//	fetch_page: read a URL as clean markdown (Jina Reader API)
//	document_search: search the user's own uploaded documents (gateway RAG)
type SearchExecutor struct {
	jinaKey string
	client  *http.Client
	// Per-request (set on a per-request copy, not the shared instance):
	// docsEnabled gates the document_search tool; docKey is the caller's
	// gateway key, so /v1/rag/search resolves to the caller's own docs.
	docsEnabled bool
	docKey      string
}

func NewSearchExecutor(jinaKey string) *SearchExecutor {
	return &SearchExecutor{
		jinaKey: jinaKey,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *SearchExecutor) GetTools() []core.Tool {
	tools := []core.Tool{
		{Type: "function", Function: core.ToolFunction{
			Name:        "web_search",
			Description: "Search the internet for current information. Returns titles, URLs and content snippets. Use for anything time-sensitive or outside your knowledge.",
			Parameters: core.ToolParameters{
				Type: "object",
				Properties: map[string]core.ToolParameter{
					"query": {Type: "string", Description: "The search query"},
				},
				Required: []string{"query"},
			},
		}},
		{Type: "function", Function: core.ToolFunction{
			Name:        "fetch_page",
			Description: "Fetch a web page and return its main content as clean markdown. Use after web_search to read a promising result in full.",
			Parameters: core.ToolParameters{
				Type: "object",
				Properties: map[string]core.ToolParameter{
					"url": {Type: "string", Description: "The URL to fetch"},
				},
				Required: []string{"url"},
			},
		}},
	}
	// document_search is only offered when the caller has enabled it (the
	// gateway sets docs_tool from the user's preference).
	if s.docsEnabled {
		tools = append(tools, core.Tool{Type: "function", Function: core.ToolFunction{
			Name:        "document_search",
			Description: "Search the user's own uploaded documents. Returns the best-matching passages with the source document name. Use when the user asks about something they uploaded, or references their own files, notes or papers.",
			Parameters: core.ToolParameters{
				Type: "object",
				Properties: map[string]core.ToolParameter{
					"query": {Type: "string", Description: "What to look for in the documents"},
					"top_k": {Type: "integer", Description: "How many passages to return (default 5, max 20)"},
				},
				Required: []string{"query"},
			},
		}})
	}
	return tools
}

func (s *SearchExecutor) jinaPost(ctx context.Context, endpoint string, body map[string]interface{}) ([]byte, error) {
	blob, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(blob)))
	if err != nil {
		return nil, err
	}
	// Reader API returns markdown with no Accept header; forcing
	// application/json here makes some pages 406.
	req.Header.Set("Authorization", "Bearer "+s.jinaKey)
	req.Header.Set("X-Respond-With", "no-cache")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jina returned %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return b, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *SearchExecutor) webSearch(ctx context.Context, args string) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", err
	}
	// Jina search: GET https://s.jina.ai/<query> — returns JSON array of
	// results with title/url/description/content.
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://s.jina.ai/"+url.PathEscape(p.Query), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.jinaKey)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jina search returned %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, r := range out.Data {
		fmt.Fprintf(&sb, "[%d] %s\n    URL: %s\n    %s\n\n", i+1, r.Title, r.URL, truncate(strings.TrimSpace(r.Content), 800))
	}
	if sb.Len() == 0 {
		return "No results found.", nil
	}
	return sb.String(), nil
}

func (s *SearchExecutor) fetchPage(ctx context.Context, args string) (string, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", err
	}
	if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
		return "", fmt.Errorf("invalid url")
	}
	b, err := s.jinaPost(ctx, "https://r.jina.ai/"+p.URL,
		map[string]interface{}{"retain_images": false})
	if err != nil {
		return "", err
	}
	return truncate(string(b), 12000), nil
}

// docSearch calls the gateway's /v1/rag/search with the caller's key so the
// search resolves to that user's own documents. Results are formatted in the
// same [n] citation style as web_search so the model cites them consistently.
func (s *SearchExecutor) docSearch(ctx context.Context, args string) (string, error) {
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
	body, _ := json.Marshal(map[string]interface{}{"query": p.Query, "top_k": p.TopK})
	req, err := http.NewRequestWithContext(ctx, "POST",
		gatewayBase+"/v1/rag/search", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.docKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.docKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("document search returned %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var out struct {
		Results []struct {
			Name  string  `json:"name"`
			Text  string  `json:"text"`
			Score float32 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "No matching passages in the user's documents. The document may not exist, or may not cover this topic.", nil
	}
	var sb strings.Builder
	for i, r := range out.Results {
		fmt.Fprintf(&sb, "[%d] %s (match %d%%)\n    %s\n\n",
			i+1, r.Name, int(r.Score*100), truncate(strings.TrimSpace(r.Text), 1200))
	}
	sb.WriteString("\n(These passages are from the user's own documents; cite the document name.)")
	return sb.String(), nil
}

func (s *SearchExecutor) Execute(ctx context.Context, calls []core.ToolCall) []core.Message {
	out := make([]core.Message, 0, len(calls))
	for _, call := range calls {
		var result string
		var err error
		switch call.Function.Name {
		case "web_search":
			result, err = s.webSearch(ctx, call.Function.Arguments)
		case "fetch_page":
			result, err = s.fetchPage(ctx, call.Function.Arguments)
		case "document_search":
			result, err = s.docSearch(ctx, call.Function.Arguments)
		default:
			result = fmt.Sprintf("unknown tool: %s", call.Function.Name)
		}
		status := core.ToolStatusCompleted
		if err != nil {
			result, status = fmt.Sprintf("tool error: %v", err), core.ToolStatusError
		}
		out = append(out, core.Message{
			Role:       "tool",
			Content:    result,
			ToolCallID: call.ID,
			Status:     status,
		})
	}
	return out
}
