package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

// Prove the merged /data template parses and renders (a template syntax
// error would 500/panic here), and that the old /documents + /memories
// URLs redirect to the right section of it.
func TestRenderDataPage(t *testing.T) {
	if err := InitUI(); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	r := httptest.NewRequest("GET", "/data", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "dana", Role: "user"}, time.Hour)})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("render /data = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		// both sections on one page
		`id="documents"`, `id="memories"`,
		// the data the page loads
		"/api/documents", "/api/memories", "memText", "docFile",
		// the toggle-status hooks
		"docToolHint", "memToolHint",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered page missing %q", want)
		}
	}
}

// TestDataPageRedirects: /documents and /memories (pre-merge URLs, incl.
// anchors in old links) land on the matching section of /data.
func TestDataPageRedirects(t *testing.T) {
	if err := InitUI(); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	for path, want := range map[string]string{
		"/documents": "/data#documents",
		"/memories":  "/data#memories",
	} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "dana", Role: "user"}, time.Hour)})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusFound {
			t.Fatalf("GET %s = %d, want 302", path, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != want {
			t.Fatalf("GET %s Location = %q, want %q", path, loc, want)
		}
	}
}
