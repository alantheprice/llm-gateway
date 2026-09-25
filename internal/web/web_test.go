package web

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestStaticServes(t *testing.T) {
	h := StaticHandler(&atomic.Value{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, p := range []string{"/static/app.css", "/static/chat.js"} {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("GET %s = %d, want 200", p, w.Code)
		}
	}
}
