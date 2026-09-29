package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/embeddedpb"
)

// Engine log lines from before the gateway's first logged request for the
// service are imported (attributed to the engine log); later ones and ones
// past retention are skipped; replace=1 re-imports without duplicates.
func TestRequestLogImport(t *testing.T) {
	app, _, _ := testPB(t)
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	s.SetOps(app)
	h := s.Handler()
	backend := fmt.Sprintf("http://link/imp%d:8006", time.Now().UnixNano()%1e6) // unique per run (shared DB)
	now := time.Now()
	gatewayStart := now.Add(-2 * time.Hour)
	if err := app.InsertRequestsTx([]embeddedpb.RequestRecord{{TS: gatewayStart, User: "dana", Backend: backend, Kind: "chat", Status: 200, Prompt: 10, Output: 5}}); err != nil {
		t.Fatal(err)
	}
	ev := func(at time.Time, prompt int) string {
		return fmt.Sprintf(`{"event":"request_done","timestamp_unix_ms":%d,"request":{"model":"qwen","protocol":"openai_chat_completions"},`+
			`"result":{"prompt_tokens":%d,"completion_tokens":40,"prefix_cache_hit_tokens":30,"prefix_reuse_path":"device"},`+
			`"timings_seconds":{"ttft":0.2,"prefill":0.15,"decode":0.5,"total":0.7},"speculative":{"drafted_tokens":50,"accepted_tokens":35},`+
			`"engine_timing":{"queue_wait_seconds":0.01}}`, at.UnixMilli(), prompt)
	}
	body := strings.Join([]string{
		`{"event":"server_start","timestamp_unix_ms":1}`,
		ev(now.Add(-3*time.Hour), 100),  // before the gateway's records: imported
		ev(now.Add(-26*time.Hour), 200), // imported
		ev(now.Add(-1*time.Hour), 300),  // already logged by the gateway: skipped
		ev(now.AddDate(0, 0, -20), 400), // past retention: skipped
		`{"event":"request_error","timestamp_unix_ms":2}`,
	}, "\n")
	post := func(q string) map[string]any {
		r := httptest.NewRequest("POST", "/admin/analytics/import?backend="+backend+q, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("Authorization", "Bearer "+adminKey(t, s))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("import: %d %s", w.Code, w.Body.String())
		}
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}
	// Only admins may import.
	userKey, _, _ := s.store.CreateKey("imp-user", "k", "user", false)
	r := httptest.NewRequest("POST", "/admin/analytics/import?backend="+backend, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+userKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("non-admin import = %d", w.Code)
	}
	m := post("")
	if m["imported"] != 2.0 || m["skipped_already_logged"] != 1.0 || m["skipped_older_than_retention"] != 1.0 {
		t.Fatalf("result = %v", m)
	}
	m = post("&replace=1")
	if m["imported"] != 2.0 {
		t.Fatalf("re-import = %v", m)
	}
	var rows []embeddedpb.BackendUserRow
	if err := app.QueryBackendUsers(14, []string{backend}, &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.User] = r.Requests
	}
	if got[engineLogUser] != 2 || got["dana"] != 1 {
		t.Fatalf("rows by user = %v", got)
	}
}

// adminKey: an ordinary admin API key for s.
func adminKey(t *testing.T, s *Server) string {
	t.Helper()
	k, _, err := s.store.CreateKey("imp-admin", "k", "admin", false)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
