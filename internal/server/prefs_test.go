package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

// Preferences save per user, apply partially, and the theme reaches the
// page before it paints; moved pages redirect to their new homes.
func TestPrefsAndSettings(t *testing.T) {
	if err := InitUI(); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	h := s.Handler()
	do := func(user, method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: user, Role: "user"}, time.Hour)})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("pat", "POST", "/api/prefs", `{"theme":"light","chat_model":"m1"}`); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	do("pat", "POST", "/api/prefs", `{"chat_web":true}`) // partial: keeps the rest
	var p map[string]any
	json.Unmarshal(do("pat", "GET", "/api/prefs", "").Body.Bytes(), &p)
	if p["theme"] != "light" || p["chat_model"] != "m1" || p["chat_web"] != true {
		t.Fatalf("prefs = %v", p)
	}
	if w := do("pat", "POST", "/api/prefs", `{"theme":"neon"}`); w.Code != 400 {
		t.Fatalf("bad theme accepted: %d", w.Code)
	}
	json.Unmarshal(do("quinn", "GET", "/api/prefs", "").Body.Bytes(), &p)
	if p["theme"] != "" || p["chat_model"] != "" {
		t.Fatalf("another user's prefs = %v", p)
	}
	if w := do("pat", "GET", "/settings", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `data-theme="light"`) {
		t.Fatalf("settings page: %d, theme applied: %v", w.Code, strings.Contains(w.Body.String(), `data-theme="light"`))
	}
	for old, dst := range map[string]string{"/account": "/settings#account", "/admin/system": "/admin/overview",
		"/admin/analytics/page": "/admin/performance"} {
		if w := do("pat", "GET", old, ""); w.Code != 301 || w.Header().Get("Location") != dst {
			t.Fatalf("%s = %d %s", old, w.Code, w.Header().Get("Location"))
		}
	}
}
