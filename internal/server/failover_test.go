package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// deadURL returns a URL on which nothing listens (connection refused).
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func okBackend(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" { // metrics poll: must 200 like a real engine
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(404)
			return
		}
		*hits++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "hi"}}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
}

func poolRequest(s *Server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	body := `{"model":"qwen","messages":[{"role":"user","content":"hello"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	return w
}

// A member that refuses connections is marked down after the first
// failure and skipped afterwards; the live member serves every request.
func TestPoolSkipsDownMember(t *testing.T) {
	hits := 0
	alive := okBackend(t, &hits)
	defer alive.Close()
	dead := deadURL(t)
	// The dead member is the larger one (capacity_weight 4), so the capacity
	// tie-break tries it first and the failure path is exercised.
	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_B%", dead)
	conf = strings.ReplaceAll(conf, "%BACKEND_A%", alive.URL)
	s := testServer(t, conf, nil)

	for i := 0; i < 3; i++ {
		if w := poolRequest(s); w.Code != 200 {
			t.Fatalf("request %d = %d %s", i, w.Code, w.Body.String())
		}
	}
	if hits != 3 {
		t.Fatalf("live member hits = %d, want 3", hits)
	}
	if !s.tracker.IsDown(dead) {
		t.Fatalf("dead member not marked down")
	}
	if s.tracker.IsDown(alive.URL) {
		t.Fatalf("live member wrongly marked down")
	}
}

// Every member dead: the client gets 503 + Retry-After, not a 502, and
// once marked down the answer comes without touching the network.
func TestPoolAllDownReturns503Fast(t *testing.T) {
	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", deadURL(t))
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", deadURL(t))
	s := testServer(t, conf, nil)

	w := poolRequest(s)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("first all-down response = %d (Retry-After %q) %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	start := time.Now()
	w = poolRequest(s)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("second all-down response = %d", w.Code)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("all-down 503 took %s; should short-circuit", d)
	}
}

// Poll failures must not evict a backend that was never pollable (engines
// without /slots or /metrics) — only previously tracked members.
// Member A is healthy (tracked); member B is a dead port (never
// tracked). After polls: A stays healthy; B is NOT marked down by a
// metrics-poll failure (dispatch failures mark tracked members down;
// poll failures only mark members that once answered).
func TestPollFailureOnlyMarksTrackedMembers(t *testing.T) {
	hits := 0
	b := okBackend(t, &hits)
	defer b.Close()
	dead := deadURL(t)

	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", b.URL)
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", dead)
	s := testServer(t, conf, nil)
	s.PollOnce()
	s.PollOnce()             // repeated failures must not change the outcome
	defer s.CloseAnalytics() // stop background writers before TempDir cleanup
	if s.tracker.IsDown(b.URL) {
		entries, _ := os.ReadDir(t.TempDir())
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("healthy tracked member marked down; tempdir entries: %v", names)
	}
	if s.tracker.IsDown(dead) {
		t.Fatalf("never-tracked member was marked down by a metrics-poll failure")
	}
}

// The access log records the status the client actually received.
func TestAccessLogRecordsClientStatus(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", deadURL(t))
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", deadURL(t))
	s := testServer(t, conf, nil)
	poolRequest(s)
	if !strings.Contains(buf.String(), "access POST /v1/chat/completions status=503") {
		t.Fatalf("access log missing 503 line:\n%s", buf.String())
	}

	buf.Reset()
	hits := 0
	alive := okBackend(t, &hits)
	defer alive.Close()
	conf = strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", alive.URL)
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", alive.URL)
	s = testServer(t, conf, nil)
	poolRequest(s)
	line := buf.String()
	if !strings.Contains(line, "status=200") || !strings.Contains(line, "backend="+alive.URL) ||
		!strings.Contains(line, "model=m-") || !strings.Contains(line, "user=local") {
		t.Fatalf("access log line incomplete:\n%s", line)
	}
}

// The recorder must keep http.Flusher so SSE streams aren't buffered.
func TestStatusRecorderFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	var w http.ResponseWriter = &statusRecorder{ResponseWriter: rec}
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("statusRecorder does not implement http.Flusher")
	}
	w.Write([]byte("data: x\n\n"))
	f.Flush()
	if !rec.Flushed {
		t.Fatal("Flush not forwarded")
	}
}

// A backend that dies mid-stream is logged with stream_err even though the
// client already received a 200 status line.
func TestAccessLogRecordsMidStreamFailure(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	dying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close() // backend dies mid-stream
	}))
	defer dying.Close()
	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", dying.URL)
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", dying.URL)
	s := testServer(t, conf, nil)

	w := httptest.NewRecorder()
	body := `{"model":"qwen","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)
	if !strings.Contains(buf.String(), "stream=true") || strings.Contains(buf.String(), `stream_err=""`) {
		t.Fatalf("mid-stream failure not logged:\n%s", buf.String())
	}
}

// A client that leaves before any response is logged as 499, not 200.
func TestAccessLogClientClosed(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	h := s.withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(buf.String(), "status=499") {
		t.Fatalf("want status=499:\n%s", buf.String())
	}
}

// Time to first token is measured at the gateway for streams: request
// received → first byte relayed. Here the engine waits 150ms before its
// first chunk, then keeps streaming for another 150ms.
func TestAccessLogTTFT(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer eng.Close()
	conf := strings.ReplaceAll(twoBackendConf, "%BACKEND_A%", eng.URL)
	conf = strings.ReplaceAll(conf, "%BACKEND_B%", eng.URL)
	s := testServer(t, conf, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "192.168.1.50:5555"
	s.Handler().ServeHTTP(w, r)

	m := regexp.MustCompile(`dur=(\S+) ttft=(\S+) `).FindStringSubmatch(buf.String())
	if m == nil {
		t.Fatalf("no ttft in access log:\n%s", buf.String())
	}
	dur, _ := time.ParseDuration(m[1])
	ttft, err := time.ParseDuration(m[2])
	if err != nil || ttft < 150*time.Millisecond || ttft >= dur-100*time.Millisecond {
		t.Fatalf("ttft=%s dur=%s: want >=150ms and well before the end", m[2], m[1])
	}
}
