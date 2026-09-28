package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cardEngine: a NInfer-shaped engine serving model id "m" with a model card
// (including deployment internals that must be redacted for non-admins).
// withCard=false mimics an older engine with no card support.
func cardEngine(t *testing.T, withCard bool) *httptest.Server {
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
			if !withCard {
				fmt.Fprint(w, `{"id":"m","max_model_len":240000}`)
				return
			}
			fmt.Fprint(w, `{"id":"m","max_model_len":240000,"model_card":{"schema":"ninfer-model-card/1",
				"name":"test card","quantization":{"weights":{"format":"nvfp4","artifact_path":"/home/x/m.ninfer"},
				"kv_cache":{"dtype":"nvfp4"}},"serving":{"endpoint":"http://0.0.0.0:8006",
				"speculative_decoding":{"backend":"mtp","draft_tokens":4},"accounting":{"request_log":"/home/x/log"}},
				"hardware":{"gpu":"NVIDIA RTX 5090"},"context":{"max_context_len":240000},
				"notes":["weights at /home/x/m.ninfer"]}}`)
		case "/v1/models/m/details":
			fmt.Fprint(w, `{"id":"m","hardware":{"host":{"kernel":"6.17"}}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cardServer(t *testing.T) (*Server, *httptest.Server, *httptest.Server, string) {
	a, b := cardEngine(t, true), cardEngine(t, false)
	conf := `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"qwen":{"members":[{"model_id":"m","backend":"` + a.URL + `"},{"model_id":"m","backend":"` + b.URL + `"}]}}}`
	s := testServer(t, conf, nil)
	adminKey, _, err := s.store.CreateKey("admin", "adm", "admin", false)
	if err != nil {
		t.Fatal(err)
	}
	s.PollOnce() // caches windows + cards
	return s, a, b, adminKey
}

func getJSON2(t *testing.T, s *Server, path, key string) (int, map[string]any, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	} else {
		r.RemoteAddr = "192.168.1.50:5555" // LAN user, not admin
	}
	s.Handler().ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m, w.Body.String()
}

// A pool shows each member's card; non-admins get it redacted and without
// backend addresses; engines without cards show null.
func TestPoolModelCardsRedacted(t *testing.T) {
	s, a, b, _ := cardServer(t)
	code, m, raw := getJSON2(t, s, "/v1/models/qwen", "")
	if code != 200 || m["pool"] != true {
		t.Fatalf("status %d: %s", code, raw)
	}
	members := m["members"].([]any)
	if len(members) != 2 || m["max_model_len"] != float64(240000) {
		t.Fatalf("pool view = %s", raw)
	}
	for _, leak := range []string{"/home/x", "artifact_path", "0.0.0.0:8006", "request_log", `"model_id"`,
		strings.TrimPrefix(a.URL, "http://"), strings.TrimPrefix(b.URL, "http://")} {
		if strings.Contains(raw, leak) {
			t.Fatalf("non-admin pool view leaks %q: %s", leak, raw)
		}
	}
	first := members[0].(map[string]any)
	sum, _ := first["card_summary"].(map[string]any)
	if sum["weights"] != "nvfp4" || sum["speculative"] != "mtp" || sum["gpu"] != "NVIDIA RTX 5090" {
		t.Fatalf("card summary = %v", sum)
	}
	if members[1].(map[string]any)["model_card"] != nil {
		t.Fatalf("card-less engine should report null card: %v", members[1])
	}
}

// Admins see the full card and which backend serves each member.
func TestPoolModelCardsAdminFull(t *testing.T) {
	s, a, _, key := cardServer(t)
	_, _, raw := getJSON2(t, s, "/v1/models/qwen", key)
	if !strings.Contains(raw, "artifact_path") || !strings.Contains(raw, a.URL) {
		t.Fatalf("admin view missing internals: %s", raw)
	}
}

// Pool members stay hidden from non-admins (as in the catalog); admins
// reach a member's own card; /details is admin-only; unknown ids 404.
func TestMemberCardVisibility(t *testing.T) {
	s, _, _, key := cardServer(t)
	if code, _, _ := getJSON2(t, s, "/v1/models/m", ""); code != 404 {
		t.Fatalf("member visible to non-admin: %d", code)
	}
	code, m, raw := getJSON2(t, s, "/v1/models/m", key)
	if code != 200 || m["model_card"] == nil || m["gpu_label"] == nil {
		t.Fatalf("admin member card: %d %s", code, raw)
	}
	if code, _, _ := getJSON2(t, s, "/v1/models/qwen/details", ""); code != 403 {
		t.Fatalf("details for non-admin = %d, want 403", code)
	}
	code, _, raw = getJSON2(t, s, "/v1/models/qwen/details", key)
	if code != 200 || !strings.Contains(raw, "kernel") {
		t.Fatalf("admin details: %d %s", code, raw)
	}
	if code, _, _ := getJSON2(t, s, "/v1/models/nope", key); code != 404 {
		t.Fatalf("unknown model = %d", code)
	}
}

func TestRedactCard(t *testing.T) {
	in := map[string]any{"a": "/home/u/x", "endpoint": "http://h", "n": []any{"/models/x", "ok"}, "keep": 1.0}
	out := redactCard(in).(map[string]any)
	if out["a"] != "[redacted]" || out["endpoint"] != nil || out["n"].([]any)[0] != "[redacted]" || out["keep"] != 1.0 {
		t.Fatalf("redacted = %v", out)
	}
}
