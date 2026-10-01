package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

// Prove the /memories template parses and renders (a template syntax error
// would 500/panic here).
func TestRenderMemoriesPage(t *testing.T) {
	if err := InitUI(); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	r := httptest.NewRequest("GET", "/memories", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "dana", Role: "user"}, time.Hour)})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("render /memories = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Memories", "memList", "/api/memories", "memText"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered page missing %q", want)
		}
	}
}
