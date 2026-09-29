package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

func chatsCall(t *testing.T, h http.Handler, s *Server, user, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: user, Role: "user"}, time.Hour)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// Conversations save per user, list without their messages, refuse a
// stale overwrite, and delete as a tombstone other devices can see.
func TestChatHistoryAPI(t *testing.T) {
	app, _, _ := testPB(t)
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	s.SetOps(app)
	h := s.Handler()
	id := fmt.Sprintf("s%d", time.Now().UnixNano()) // the database is shared across tests
	save := func(user string, updated int64, text string) int {
		code, _ := chatsCall(t, h, s, user, "PUT", "/api/chats/"+id, fmt.Sprintf(
			`{"title":"Trip","model":"m","created":1,"updated":%d,"messages":[{"role":"user","content":%q}]}`, updated, text))
		return code
	}
	if c := save("dana", 100, "first"); c != 200 {
		t.Fatalf("save: %d", c)
	}
	if c := save("dana", 200, "second"); c != 200 {
		t.Fatalf("newer save: %d", c)
	}
	if c := save("dana", 150, "stale"); c != 409 {
		t.Fatalf("stale save = %d, want 409", c)
	}
	_, got := chatsCall(t, h, s, "dana", "GET", "/api/chats/"+id, "")
	if msgs := got["messages"].([]any); got["title"] != "Trip" || msgs[0].(map[string]any)["content"] != "second" {
		t.Fatalf("get = %v", got)
	}
	// Other users see nothing of it.
	if c, _ := chatsCall(t, h, s, "erin", "GET", "/api/chats/"+id, ""); c != 404 {
		t.Fatalf("other user's get = %d", c)
	}
	_, list := chatsCall(t, h, s, "dana", "GET", "/api/chats?since=150", "")
	items := list["chats"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["msg_count"] != 1.0 || items[0].(map[string]any)["messages"] != nil {
		t.Fatalf("list = %v", list)
	}
	// Delete: gone, listed as a tombstone, and an older copy can't revive it.
	if c, _ := chatsCall(t, h, s, "dana", "DELETE", "/api/chats/"+id+"?at=300", ""); c != 200 {
		t.Fatalf("delete: %d", c)
	}
	if c, _ := chatsCall(t, h, s, "dana", "GET", "/api/chats/"+id, ""); c != 404 {
		t.Fatalf("get after delete = %d", c)
	}
	_, list = chatsCall(t, h, s, "dana", "GET", "/api/chats?since=250", "")
	if items := list["chats"].([]any); len(items) != 1 || items[0].(map[string]any)["deleted"] != true {
		t.Fatalf("tombstone = %v", list)
	}
	if c := save("dana", 250, "from an offline device"); c != 409 {
		t.Fatalf("revive deleted with older copy = %d", c)
	}
	if c, _ := chatsCall(t, h, s, "dana", "PUT", "/api/chats/bad%20id", `{"messages":[]}`); c != 400 {
		t.Fatalf("bad id = %d", c)
	}
}
