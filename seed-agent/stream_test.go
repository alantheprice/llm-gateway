package main

import (
	"strings"
	"testing"

	"github.com/sprout-foundry/seed/core"
)

type recHandler struct{ content, reasoning strings.Builder }

func (h *recHandler) OnContent(s string)        { h.content.WriteString(s) }
func (h *recHandler) OnReasoning(s string)      { h.reasoning.WriteString(s) }
func (h *recHandler) OnDone(*core.ChatResponse) {}
func (h *recHandler) OnError(error)             {}

// Text and thinking are forwarded as they arrive; tool calls split across
// chunks are reassembled in index order.
func TestReadChatStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"Let me "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"look."}}]}`,
		`data: {"choices":[{"delta":{"content":"Checking"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"fetch_page","arguments":"{\"url\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"web_search","arguments":"{\"query\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"go\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"https://x\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, ``}, "\n\n")
	h := &recHandler{}
	msg, finish, err := readChatStream(strings.NewReader(stream), h, func(r string) { h.reasoning.WriteString(r) })
	if err != nil || finish != "tool_calls" {
		t.Fatal(err, finish)
	}
	if h.reasoning.String() != "Let me look." || h.content.String() != "Checking" || msg.Content != "Checking" {
		t.Fatalf("reasoning %q content %q", h.reasoning.String(), h.content.String())
	}
	if len(msg.ToolCalls) != 2 || msg.ToolCalls[0].Function.Name != "web_search" || msg.ToolCalls[0].Function.Arguments != `{"query":"go"}` ||
		msg.ToolCalls[1].ID != "b" || msg.ToolCalls[1].Function.Arguments != `{"url":"https://x"}` {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	if _, _, err := readChatStream(strings.NewReader(`data: {"error":{"message":"boom"}}`+"\n\n"), h, nil); err == nil || err.Error() != "boom" {
		t.Fatalf("stream error = %v", err)
	}
}
