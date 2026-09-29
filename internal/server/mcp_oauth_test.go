package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

// fakeOAuthMCP: an MCP server whose 401 points at protected-resource
// metadata, and an authorization server with registration, PKCE and
// refresh — the MCP authorization spec, end to end.
func fakeOAuthMCP(t *testing.T) (*httptest.Server, *sync.Map) {
	seen := &sync.Map{}
	var base string
	var challenge string
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp", scope="read"`)
			w.WriteHeader(401)
			return
		}
		seen.Store("mcp-auth", r.Header.Get("Authorization"))
		w.WriteHeader(200)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base + "/auth"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": base + "/auth",
			"authorization_endpoint": base + "/auth/authorize", "token_endpoint": base + "/auth/token",
			"registration_endpoint": base + "/auth/register", "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		seen.Store("redirect_uris", in["redirect_uris"])
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "cid-1"})
	})
	mux.HandleFunc("/auth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "cid-1" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != base+"/mcp" || q.Get("scope") != "read" {
			http.Error(w, "bad authorize request: "+r.URL.RawQuery, 400)
			return
		}
		challenge = q.Get("code_challenge")
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-1&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "code-1" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("resource") != base+"/mcp" {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3600})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-1" {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at-2", "expires_in": 3600})
		}
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestMCPOAuthSignIn(t *testing.T) {
	srv, seen := fakeOAuthMCP(t)
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	h := s.Handler()
	s.store.CreateKey("root", "k", "admin", false)
	// Admin: the fake servers are on 127.0.0.1.
	code, m := mcpCall(t, s, "root", "admin", "POST", `{"action":"add","name":"svc","url":"`+srv.URL+`/mcp","auth":"oauth"}`)
	if code != 200 {
		t.Fatalf("add: %d %v", code, m)
	}
	v := m["servers"].([]any)[0].(map[string]any)
	if v["auth"] != "oauth" || v["signed_in"] == true {
		t.Fatalf("view before sign-in = %v", v)
	}
	id := v["id"].(string)
	tok := s.store.SignSession(auth.Claims{U: "root", Role: "admin"}, time.Hour)
	withSession := func(r *http.Request) *http.Request {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		return r
	}

	// Start: discovery + registration, then off to the authorization server.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, withSession(httptest.NewRequest("GET", "https://gw.example/api/mcp/oauth/start?id="+id, nil)))
	if w.Code != 302 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if ru, _ := seen.Load("redirect_uris"); fmt.Sprint(ru) != "[https://gw.example/api/mcp/oauth/callback]" {
		t.Fatalf("registered redirect = %v", ru)
	}
	// The authorization server approves and sends the browser back.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(w.Header().Get("Location"))
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("authorize: %v %v", resp.Status, err)
	}
	cb, _ := url.Parse(resp.Header.Get("Location"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, withSession(httptest.NewRequest("GET", "https://gw.example/api/mcp/oauth/callback?"+cb.RawQuery, nil)))
	if !strings.Contains(w.Body.String(), "Connected") {
		t.Fatalf("callback: %s", w.Body.String())
	}
	// The state is single-use.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, withSession(httptest.NewRequest("GET", "https://gw.example/api/mcp/oauth/callback?"+cb.RawQuery, nil)))
	if !strings.Contains(w.Body.String(), "expired or was already used") {
		t.Fatalf("replayed callback accepted: %s", w.Body.String())
	}

	_, m = mcpCall(t, s, "root", "admin", "GET", "")
	if v := m["servers"].([]any)[0].(map[string]any); v["signed_in"] != true {
		t.Fatalf("view after sign-in = %v", v)
	}
	raw, _ := os.ReadFile(s.cfg.Gateway.UsersFile)
	if strings.Contains(string(raw), "at-1") || strings.Contains(string(raw), "rt-1") {
		t.Fatal("tokens stored in plain text")
	}

	// Handed to the agent as a bearer token; refreshed once it's expiring.
	agentBody := func() map[string]any {
		var out map[string]any
		json.Unmarshal(s.withMCPServers(context.Background(), []byte(`{"messages":[]}`), "root", "k"), &out)
		return out
	}
	hdr := func(b map[string]any) any {
		return b["mcp_servers"].([]any)[0].(map[string]any)["headers"].(map[string]any)["Authorization"]
	}
	if got := hdr(agentBody()); got != "Bearer at-1" {
		t.Fatalf("agent header = %v", got)
	}
	list := s.store.MCPServersOf("root")
	list[0].OAuth.Expiry = time.Now().Add(-time.Minute).Unix()
	s.store.SetMCPServers("root", list)
	if got := hdr(agentBody()); got != "Bearer at-2" {
		t.Fatalf("after refresh = %v", got)
	}
	// Signed out: the agent gets a notice instead of the server.
	mcpCall(t, s, "root", "admin", "POST", `{"action":"sign_out","id":"`+id+`"}`)
	if b := agentBody(); b["mcp_servers"] != nil || !strings.Contains(fmt.Sprint(b["mcp_notices"]), "not signed in") {
		t.Fatalf("after sign-out = %v", b)
	}
}
