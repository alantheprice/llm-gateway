package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const statsChunk = `data: {"choices":[],"stats":{"ttft_ms":123.5,"queue_wait_ms":7,"prefix_reuse_path":"private_response_replay","speculative":{"draft_tokens":56,"accepted_tokens":18}}}` + "\n\n"

// Injected stats are stripped from streams, even when split across reads;
// client-requested stats pass through with the gateway block added.
func TestSSEFilterStats(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" + statsChunk + "data: [DONE]\n\n"
	f := &sseFilter{stripStats: true}
	var out []byte
	for i := 0; i < len(stream); i += 9 {
		j := min(i+9, len(stream))
		out = append(out, f.write([]byte(stream[i:j]))...)
	}
	out = append(out, f.flush()...)
	if strings.Contains(string(out), "stats") || !strings.Contains(string(out), `"hi"`) || !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("injected stats not stripped cleanly:\n%s", out)
	}
	f = &sseFilter{gateway: map[string]any{"gpu_label": "A :8006", "via": "link"}}
	out = append(f.write([]byte(stream)), f.flush()...)
	if !strings.Contains(string(out), `"gateway":{"gpu_label":"A :8006","via":"link"}`) || !strings.Contains(string(out), "ttft_ms") {
		t.Fatalf("client stats missing gateway block:\n%s", out)
	}
}

// Engine stats feed the recorded telemetry on both paths.
func TestEngineStatsExtracted(t *testing.T) {
	_, _, _, ex := usageFromSSE([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n"+statsChunk), 1)
	if ex.EngineTTFTms != 123.5 || ex.QueueWaitms != 7 || ex.ReusePath != "private_response_replay" || ex.DraftN != 56 {
		t.Fatalf("SSE stats = %+v", ex)
	}
	js := `{"usage":{"prompt_tokens":10,"completion_tokens":2},"stats":{"ttft_ms":88,"queue_wait_ms":3,"speculative":{"draft_tokens":10,"accepted_tokens":4}}}`
	_, _, _, ex = usageFromJSON([]byte(js), 1)
	if ex.EngineTTFTms != 88 || ex.QueueWaitms != 3 || ex.DraftN != 10 || ex.DraftAccepted != 4 {
		t.Fatalf("JSON stats = %+v", ex)
	}
}

// statsEngine: a card-serving engine that honours return_stats and records
// whether the gateway asked for it.
func statsEngine(t *testing.T, withCard bool, asked *[]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/slots":
			fmt.Fprint(w, `{"max_concurrency":8,"requests_processing":0,"requests_waiting":0}`)
		case "/usage":
			fmt.Fprint(w, `{}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"m","max_model_len":240000}]}`)
		case "/v1/models/m":
			if withCard {
				fmt.Fprint(w, `{"id":"m","model_card":{"schema":"ninfer-model-card/1"}}`)
			} else {
				fmt.Fprint(w, `{"id":"m"}`)
			}
		case "/v1/chat/completions":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			want, _ := req["return_stats"].(bool)
			*asked = append(*asked, want)
			if req["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
				io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n")
				if want {
					io.WriteString(w, statsChunk)
				}
				io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			resp := map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
				"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 1},
			}
			if want {
				resp["stats"] = map[string]any{"ttft_ms": 42.0, "queue_wait_ms": 1.0}
			}
			json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func statsRequest(t *testing.T, s *Server, body string) string {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// End to end: card engines are asked for stats; clients that didn't ask get
// none; clients that asked get stats plus the gateway block; engines without
// cards are never sent the field.
func TestStatsPassthroughE2E(t *testing.T) {
	var asked []bool
	eng := statsEngine(t, true, &asked)
	conf := `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"qwen":{"members":[{"model_id":"m","backend":"` + eng.URL + `"}]}}}`
	s := testServer(t, conf, nil)
	s.PollOnce()

	plain := statsRequest(t, s, `{"model":"qwen","messages":[{"role":"user","content":"a"}]}`)
	if strings.Contains(plain, "stats") || asked[len(asked)-1] != true {
		t.Fatalf("non-stream: engine asked=%v, client body=%s", asked, plain)
	}
	withStats := statsRequest(t, s, `{"model":"qwen","return_stats":true,"messages":[{"role":"user","content":"a"}]}`)
	if !strings.Contains(withStats, `"ttft_ms":42`) || !strings.Contains(withStats, `"gateway"`) {
		t.Fatalf("client stats missing: %s", withStats)
	}
	stream := statsRequest(t, s, `{"model":"qwen","stream":true,"messages":[{"role":"user","content":"a"}]}`)
	if strings.Contains(stream, "stats") || !strings.Contains(stream, "[DONE]") {
		t.Fatalf("stream leaked injected stats:\n%s", stream)
	}
	streamStats := statsRequest(t, s, `{"model":"qwen","stream":true,"return_stats":true,"messages":[{"role":"user","content":"a"}]}`)
	if !strings.Contains(streamStats, "ttft_ms") || !strings.Contains(streamStats, `"gateway"`) {
		t.Fatalf("stream client stats missing:\n%s", streamStats)
	}

	var asked2 []bool
	old := statsEngine(t, false, &asked2)
	conf2 := strings.ReplaceAll(conf, eng.URL, old.URL)
	s2 := testServer(t, conf2, nil)
	s2.PollOnce()
	statsRequest(t, s2, `{"model":"qwen","messages":[{"role":"user","content":"a"}]}`)
	if len(asked2) != 1 || asked2[0] {
		t.Fatalf("card-less engine was sent return_stats: %v", asked2)
	}
}
