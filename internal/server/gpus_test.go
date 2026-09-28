package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

func gpusCall(t *testing.T, s *Server, user, role, method, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/api/gpus", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/api/gpus", nil)
	}
	tok := s.store.SignSession(auth.Claims{U: user, Role: role}, time.Hour)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	s.Handler().ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// Creating a link returns a one-time install script carrying the token,
// name and engine; the link shows on the user's list, not among API keys.
func TestCreateLinkInstallScript(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0,"public_base_url":"https://gw.example"}}`, nil)
	code, m := gpusCall(t, s, "carol", "user", "POST",
		`{"action":"create_link","name":"Office-4090","engine":"127.0.0.1:8009","model_id":"qwen3-8b"}`)
	if code != 200 {
		t.Fatalf("create: %d %v", code, m)
	}
	script, _ := m["script"].(string)
	for _, want := range []string{`SERVER="https://gw.example"`, `NAME="office-4090"`, "127.0.0.1:8009=qwen3-8b", "LLM_LINK_TOKEN=%s", "' 'sk-"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if m["virtual_url"] != "http://link/office-4090:8009" {
		t.Fatalf("virtual url = %v", m["virtual_url"])
	}
	_, list := gpusCall(t, s, "carol", "user", "GET", "")
	if links := list["links"].([]any); len(links) != 1 || links[0].(map[string]any)["key_id"] != "link-office-4090" {
		t.Fatalf("links = %v", list["links"])
	}
	if len(s.store.ListKeys("carol")) != 0 {
		t.Fatal("link token shows up as an API key")
	}
}

// Bad names, shell-unsafe engine values, duplicates and the per-user cap
// are rejected.
func TestCreateLinkValidation(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	for _, body := range []string{
		`{"action":"create_link","name":"bad name","model_id":"m"}`,
		`{"action":"create_link","name":"ok","engine":"127.0.0.1:80;rm -rf","model_id":"m"}`,
		`{"action":"create_link","name":"ok","model_id":"m'; curl evil"}`,
		`{"action":"create_link","name":"ok","model_id":""}`,
	} {
		if code, _ := gpusCall(t, s, "dan", "user", "POST", body); code != 400 {
			t.Fatalf("accepted %s (%d)", body, code)
		}
	}
	for i, name := range []string{"a", "b", "c"} {
		if code, m := gpusCall(t, s, "dan", "user", "POST", `{"action":"create_link","name":"`+name+`","model_id":"m"}`); code != 200 {
			t.Fatalf("link %d: %d %v", i, code, m)
		}
	}
	if code, _ := gpusCall(t, s, "dan", "user", "POST", `{"action":"create_link","name":"a","model_id":"m"}`); code == 200 {
		t.Fatal("duplicate or over-cap link accepted")
	}
	if code, _ := gpusCall(t, s, "dan", "user", "POST", `{"action":"create_link","name":"d","model_id":"m"}`); code != 400 {
		t.Fatal("per-user cap not enforced")
	}
	if code, _ := gpusCall(t, s, "root", "admin", "POST", `{"action":"create_link","name":"e","model_id":"m"}`); code != 200 {
		t.Fatal("admins are not capped")
	}
}

// A link token cannot be used as an API key.
func TestLinkTokenNotAnAPIKey(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0,"models_require_auth":true}}`, nil)
	tok, _, _ := s.store.CreateKey("carol", "link-x", auth.RoleLink, false)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("link token accepted as API key: %d", w.Code)
	}
}

// Revoking a link disconnects its live agent immediately (real agent binary).
func TestRevokeLinkDisconnectsAgent(t *testing.T) {
	rig, _ := newLinkRig(t, "carol", auth.RoleLink)
	rig.waitRegistered(t)
	u := "http://link/e2e:" + rig.engPort
	code, m := gpusCall(t, rig.s, "carol", "user", "POST", `{"action":"revoke_link","key_id":"link-e2e"}`)
	if code != 200 || m["disconnected"] != float64(1) {
		t.Fatalf("revoke: %d %v", code, m)
	}
	for i := 0; i < 40 && rig.s.linkReg.Lookup(u) != nil; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if rig.s.linkReg.Lookup(u) != nil {
		t.Fatal("agent still connected after its link was revoked")
	}
	// Another user cannot revoke carol's link.
	if code, _ := gpusCall(t, rig.s, "mallory", "user", "POST", `{"action":"revoke_link","key_id":"link-e2e"}`); code != 404 {
		t.Fatalf("cross-user revoke = %d", code)
	}
}

// The installer's download URL serves the agent binary.
func TestAgentDownload(t *testing.T) {
	f := t.TempDir() + "/agent"
	os.WriteFile(f, []byte("BINARY"), 0o755)
	t.Setenv("LLM_LINK_AGENT_BIN", f)
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/downloads/llm-link-agent-linux-amd64", nil))
	if w.Code != 200 || w.Body.String() != "BINARY" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
}

// The My GPUs page renders its walkthrough, and the guide it links to
// resolves with the anchors the page deep-links.
func TestGPUsPageAndGuide(t *testing.T) {
	if err := InitUI(); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/gpus", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "carol", Role: "user"}, time.Hour)})
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `class="wz-steps"`) {
		t.Fatalf("/gpus: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/models", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "carol", Role: "user"}, time.Hour)})
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `id="models"`) || !strings.Contains(w.Body.String(), `href="/models"`) {
		t.Fatalf("/models: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/guide/link-gpu", nil))
	for _, id := range []string{"requirements", "troubleshooting", "platform-models", "managing-a-link", "how-it-works", "step-5-use-it-and-share-it"} {
		if !strings.Contains(w.Body.String(), `id="`+id+`"`) {
			t.Fatalf("guide missing #%s (%d)", id, w.Code)
		}
	}
}
