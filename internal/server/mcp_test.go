package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

func mcpCall(t *testing.T, s *Server, user, role, method, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/api/mcp", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/api/mcp", nil)
	}
	tok := s.store.SignSession(auth.Claims{U: user, Role: role}, time.Hour)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	s.Handler().ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// Users add public https servers; secrets come back masked and survive an
// update that echoes the mask; LAN and plain-http servers are admin-only.
func TestMCPServerAPI(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	code, m := mcpCall(t, s, "carol", "user", "POST",
		`{"action":"add","name":"GitHub","url":"https://93.184.216.34/mcp","headers":{"Authorization":"Bearer ghp_supersecret1234"}}`)
	if code != 200 {
		t.Fatalf("add: %d %v", code, m)
	}
	sv := m["servers"].([]any)[0].(map[string]any)
	if sv["name"] != "github" || sv["headers"].(map[string]any)["Authorization"] != "••••1234" || sv["enabled"] != true {
		t.Fatalf("view = %v", sv)
	}
	id := sv["id"].(string)
	mcpCall(t, s, "carol", "user", "POST", `{"action":"update","id":"`+id+`","enabled":false,"headers":{"Authorization":"••••1234","X-Org":"acme"}}`)
	got := s.store.MCPServersOf("carol")[0]
	if got.Enabled || got.Headers["Authorization"] != "Bearer ghp_supersecret1234" || got.Headers["X-Org"] != "acme" {
		t.Fatalf("stored = %+v", got)
	}
	for _, bad := range []string{
		`{"action":"add","name":"lan","url":"https://192.168.1.5/mcp"}`,
		`{"action":"add","name":"loop","url":"https://localhost/mcp"}`,
		`{"action":"add","name":"plain","url":"http://93.184.216.34/mcp"}`,
		`{"action":"add","name":"github","url":"https://93.184.216.35/mcp"}`,
		`{"action":"add","name":"Bad Name!","url":"https://93.184.216.35/mcp"}`,
		`{"action":"add","name":"h","url":"https://93.184.216.35/mcp","headers":{"Mcp-Session-Id":"x"}}`,
	} {
		if code, m := mcpCall(t, s, "carol", "user", "POST", bad); code != 400 {
			t.Fatalf("%s: %d %v", bad, code, m)
		}
	}
	if code, _ := mcpCall(t, s, "root", "admin", "POST", `{"action":"add","name":"lan","url":"http://192.168.1.5:3000/mcp"}`); code != 200 {
		t.Fatalf("admin LAN server refused: %d", code)
	}
	// Another user can't touch carol's server.
	if code, _ := mcpCall(t, s, "dave", "user", "POST", `{"action":"delete","id":"`+id+`"}`); code != 404 {
		t.Fatalf("cross-user delete: %d", code)
	}
	if code, m := mcpCall(t, s, "carol", "user", "POST", `{"action":"delete","id":"`+id+`"}`); code != 200 || len(m["servers"].([]any)) != 0 {
		t.Fatalf("delete: %d %v", code, m)
	}
}

// The agent request carries the caller's enabled servers (with secrets)
// and nobody else's; a client can't smuggle servers or LAN access in.
func TestAgentRequestGetsCallersMCPServers(t *testing.T) {
	var got map[string]any
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: done\ndata: {\"content\":\"hi\"}\n\n")
	}))
	defer agent.Close()
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	s.agentURL = agent.URL
	s.store.SetMCPServers("carol", []auth.MCPServer{
		{ID: "a", Name: "on", URL: "https://a.example/mcp", Headers: map[string]string{"Authorization": "Bearer x"}, Enabled: true},
		{ID: "b", Name: "off", URL: "https://b.example/mcp", Enabled: false},
	})
	s.store.SetMCPServers("dave", []auth.MCPServer{{ID: "c", Name: "daves", URL: "https://c.example/mcp", Enabled: true}})
	key, _, _ := s.store.CreateKey("carol", "k", "user", false)
	req := httptest.NewRequest("POST", "/v1/agent/chat", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"mcp_servers":[{"name":"evil","url":"http://127.0.0.1:22"}],"allow_private":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("agent chat: %d %s", w.Code, w.Body.String())
	}
	servers, _ := got["mcp_servers"].([]any)
	if len(servers) != 1 || got["allow_private"] != false {
		t.Fatalf("forwarded = %v", got)
	}
	sv := servers[0].(map[string]any)
	if sv["name"] != "on" || sv["headers"].(map[string]any)["Authorization"] != "Bearer x" {
		t.Fatalf("server = %v", sv)
	}
}
