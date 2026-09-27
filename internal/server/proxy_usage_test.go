package server

import (
	"strings"
	"testing"
)

// Fixtures use real NInfer final-chunk shapes (captured from the live
// engine): usage chunk with empty choices, then data: [DONE].

const realStreamTail = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"index\":0}]}\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\" world\"},\"index\":0}]}\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0,\"usage\":null}]}\n" +
	"data: {\"choices\":[],\"usage\":{\"completion_tokens\":26,\"prompt_tokens\":54,\"prompt_tokens_details\":{\"cached_tokens\":49},\"total_tokens\":80}}\n" +
	"data: [DONE]\n"

// 2a: the usage block is the source of truth — prompt is the FULL prompt
// (54, not the 5-token uncached suffix), cached is 49.
func TestUsageFromSSE_PrefersUsageBlock(t *testing.T) {
	pt, ot, cached := usageFromSSE([]byte(realStreamTail), 999)
	if pt != 54 {
		t.Fatalf("prompt = %d, want 54 (prompt_n is uncached-only; usage block has the full count)", pt)
	}
	if cached != 49 {
		t.Fatalf("cached = %d, want 49", cached)
	}
	if ot < 20 || ot > 40 { // completion_tokens=26; content chars are not double-counted
		t.Fatalf("output = %d, want ≈26", ot)
	}
}

// 2a (timings fallback): with no usage block, prompt = prompt_n + cache_n.
func TestUsageFromSSE_TimingsPromptIncludesCache(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n" +
		"data: {\"choices\":[],\"timings\":{\"prompt_n\":5,\"predicted_n\":26,\"cache_n\":49}}\n" +
		"data: [DONE]\n"
	pt, ot, cached := usageFromSSE([]byte(stream), 999)
	if pt != 54 {
		t.Fatalf("prompt = %d, want 54 (5 uncached + 49 cached)", pt)
	}
	if ot != 26 {
		t.Fatalf("output = %d, want 26", ot)
	}
	if cached != 49 {
		t.Fatalf("cached = %d, want 49", cached)
	}
}

// 2b: the injected usage chunk (empty choices + usage) is stripped from
// the client stream, including when it arrives in the same read as the
// finish chunk and [DONE]; everything else passes through intact.
func TestSSEFilter_StripsOnlyInjectedUsage(t *testing.T) {
	f := newSSEFilter(true)
	var out []byte
	// Feed the whole tail in ONE read — the usage chunk, finish chunk and
	// [DONE] share a buffer (the original bug).
	out = append(out, f.write([]byte(realStreamTail))...)
	out = append(out, f.flush()...)
	s := string(out)
	if strings.Contains(s, "\"choices\":[]") {
		t.Fatalf("injected usage chunk leaked to client:\n%s", s)
	}
	if !strings.Contains(s, "Hello") || !strings.Contains(s, " world") {
		t.Fatalf("content chunks were dropped:\n%s", s)
	}
	if !strings.Contains(s, "[DONE]") {
		t.Fatalf("[DONE] was dropped:\n%s", s)
	}
}

// 2b: filter disabled (client asked for include_usage) passes everything.
func TestSSEFilter_PassthroughWhenNotStripping(t *testing.T) {
	f := newSSEFilter(false)
	out := f.write([]byte(realStreamTail))
	if !strings.Contains(string(out), "\"choices\":[]") {
		t.Fatal("filter should pass everything through when stripping is off")
	}
}

// 2b: partial lines carry across reads without duplication or loss.
func TestSSEFilter_CarriesPartialLines(t *testing.T) {
	f := newSSEFilter(true)
	var out []byte
	// split the usage-chunk line mid-JSON
	full := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":54}}\n"
	out = append(out, f.write([]byte(full[:20]))...)
	out = append(out, f.write([]byte(full[20:]))...)
	if len(out) != 0 {
		t.Fatalf("usage chunk survived a split read: %q", string(out))
	}
	// a real content line split across reads still comes through complete
	f2 := newSSEFilter(true)
	line := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n"
	var out2 []byte
	out2 = append(out2, f2.write([]byte(line[:15]))...)
	out2 = append(out2, f2.write([]byte(line[15:]))...)
	if !strings.Contains(string(out2), "\"content\":\"hi\"") {
		t.Fatalf("content line mangled across split read: %q", string(out2))
	}
}

// 2c: the no-usage/no-timings fallback estimates output as chars/4.
func TestUsageFromSSE_FallbackCharsOver4(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"abcdefghijklmnop\"}}]}\n" + // 16 chars → 4 tokens
		"data: [DONE]\n"
	_, ot, _ := usageFromSSE([]byte(stream), 1)
	if ot != 4 {
		t.Fatalf("output = %d, want 4 (16 chars / 4)", ot)
	}
}

// #6: status >= 400 gates recording — covered in relay(); this asserts
// the helper semantics the gate relies on stay correct.
func TestClientAskedIncludeUsage(t *testing.T) {
	if !clientAskedIncludeUsage([]byte(`{"stream":true,"stream_options":{"include_usage":true}}`)) {
		t.Fatal("client asked for usage — should be true")
	}
	if clientAskedIncludeUsage([]byte(`{"stream":true}`)) {
		t.Fatal("client did not ask — should be false")
	}
}
