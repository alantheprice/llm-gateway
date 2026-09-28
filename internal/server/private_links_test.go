package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"llmgateway/internal/auth"
)

// The response names the model the client called, not the engine's id.
func TestPrivateLinkResponseModelName(t *testing.T) {
	rig, _ := newLinkRig(t, "carol", auth.RoleLink)
	rig.waitRegistered(t)
	carol, _, _ := rig.s.store.CreateKey("carol", "k", "user", false)
	req, _ := http.NewRequest("POST", rig.gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"e2e/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+carol)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if n := strings.Count(string(body), `"model":"e2e/m"`); n == 0 || strings.Contains(string(body), `"model":"m"`) {
		t.Fatalf("stream model names (%d rewritten):\n%.400s", n, body)
	}
}

// chatAs posts a one-message chat for model with key (Bearer; "" = none)
// through the rig's gateway.
func chatAs(t *testing.T, rig *linkRig, key, model string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", rig.gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func modelsAs(t *testing.T, rig *linkRig, key string) []string {
	t.Helper()
	req, _ := http.NewRequest("GET", rig.gw.URL+"/v1/models", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids
}

func has(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// A user's link is private: only its owner lists and calls "<gpu>/<model>";
// sharing with a named user opens it to them; pausing closes it to
// everyone; anonymous LAN callers never reach it.
func TestPrivateLinkSharing(t *testing.T) {
	rig, _ := newLinkRig(t, "carol", auth.RoleLink)
	rig.waitRegistered(t)
	carol, _, _ := rig.s.store.CreateKey("carol", "k", "user", false)
	dave, _, _ := rig.s.store.CreateKey("dave", "k", "user", false)
	const name = "e2e/m"

	if !has(modelsAs(t, rig, carol), name) {
		t.Fatalf("owner does not see %s: %v", name, modelsAs(t, rig, carol))
	}
	if has(modelsAs(t, rig, dave), name) || has(modelsAs(t, rig, ""), name) {
		t.Fatal("private link listed to another user / anonymously")
	}
	if c := chatAs(t, rig, carol, name); c != 200 {
		t.Fatalf("owner chat = %d", c)
	}
	if c := chatAs(t, rig, dave, name); c != 404 {
		t.Fatalf("unshared user chat = %d", c)
	}
	if c := chatAs(t, rig, "", name); c != 401 && c != 404 {
		t.Fatalf("anonymous chat = %d", c)
	}
	// LAN-trusted unkeyed requests run as "local" with no key id.
	if _, ok := rig.s.resolvePrivate(privateCaller("local", ""), name); ok {
		t.Fatal("anonymous LAN caller reaches a private link")
	}

	if code, m := gpusCall(t, rig.s, "carol", "user", "POST",
		`{"action":"update_link","key_id":"link-e2e","shared_with":["dave"]}`); code != 200 {
		t.Fatalf("share: %d %v", code, m)
	}
	if !has(modelsAs(t, rig, dave), name) || chatAs(t, rig, dave, name) != 200 {
		t.Fatal("shared user cannot use the link")
	}
	_, list := gpusCall(t, rig.s, "dave", "user", "GET", "")
	if sw := list["shared_with_me"].([]any); len(sw) != 1 || sw[0].(map[string]any)["owner"] != "carol" {
		t.Fatalf("shared_with_me = %v", list["shared_with_me"])
	}

	// Share with everyone: a user nobody named gets access; turning it off
	// falls back to the named list.
	erin, _, _ := rig.s.store.CreateKey("erin", "k", "user", false)
	if chatAs(t, rig, erin, name) != 404 {
		t.Fatal("unnamed user reached a link before it was shared with everyone")
	}
	gpusCall(t, rig.s, "carol", "user", "POST", `{"action":"update_link","key_id":"link-e2e","shared_all":true}`)
	if !has(modelsAs(t, rig, erin), name) || chatAs(t, rig, erin, name) != 200 {
		t.Fatal("shared-with-everyone link not usable by another user")
	}
	if _, ok := rig.s.resolvePrivate(privateCaller("local", ""), name); ok {
		t.Fatal("anonymous LAN caller reaches a link shared with everyone")
	}
	gpusCall(t, rig.s, "carol", "user", "POST", `{"action":"update_link","key_id":"link-e2e","shared_all":false}`)
	if chatAs(t, rig, erin, name) != 404 || chatAs(t, rig, dave, name) != 200 {
		t.Fatal("unsharing with everyone did not fall back to the named list")
	}

	gpusCall(t, rig.s, "carol", "user", "POST", `{"action":"update_link","key_id":"link-e2e","paused":true}`)
	if chatAs(t, rig, carol, name) != 404 || chatAs(t, rig, dave, name) != 404 {
		t.Fatal("paused link still serves")
	}
	gpusCall(t, rig.s, "carol", "user", "POST", `{"action":"update_link","key_id":"link-e2e","paused":false,"shared_with":[]}`)
	if chatAs(t, rig, carol, name) != 200 || chatAs(t, rig, dave, name) != 404 {
		t.Fatal("unpause / unshare not applied")
	}
}

// A pool that lists a user's link routes to it only after the owner
// consents to that pool (admin-owned links serve as configured).
func TestLinkPoolNeedsOwnerConsent(t *testing.T) {
	rig, _ := newLinkRig(t, "carol", auth.RoleLink)
	rig.waitRegistered(t)
	dave, _, _ := rig.s.store.CreateKey("dave", "k", "user", false)
	if c := chatAs(t, rig, dave, "qwen"); c != 503 {
		t.Fatalf("pool routed to an unconsented user link: %d", c)
	}
	_, list := gpusCall(t, rig.s, "carol", "user", "GET", "")
	eng := list["links"].([]any)[0].(map[string]any)["agent"].(map[string]any)["engines"].([]any)[0].(map[string]any)
	if p := eng["pools"].([]any); len(p) != 1 || p[0] != "qwen" || len(eng["serving"].([]any)) != 0 {
		t.Fatalf("pools/serving before consent = %v / %v", eng["pools"], eng["serving"])
	}
	if code, m := gpusCall(t, rig.s, "carol", "user", "POST",
		`{"action":"update_link","key_id":"link-e2e","pools":["qwen","no-such-pool"]}`); code != 200 ||
		len(m["settings"].(map[string]any)["pools"].([]any)) != 1 {
		t.Fatalf("consent: %d %v", code, m)
	}
	if c := chatAs(t, rig, dave, "qwen"); c != 200 {
		t.Fatalf("pool after consent = %d", c)
	}
}

// Owners control only their own links; sharing needs a real account.
func TestUpdateLinkValidation(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	gpusCall(t, s, "carol", "user", "POST", `{"action":"create_link","name":"box","model_id":"m"}`)
	if code, _ := gpusCall(t, s, "carol", "user", "POST",
		`{"action":"update_link","key_id":"link-box","shared_with":["ghost"]}`); code != 400 {
		t.Fatalf("shared with unknown user: %d", code)
	}
	if code, _ := gpusCall(t, s, "mallory", "user", "POST",
		`{"action":"update_link","key_id":"link-box","paused":true}`); code != 404 {
		t.Fatalf("cross-user update: %d", code)
	}
	s.store.CreateKey("dave", "k", "user", false)
	code, m := gpusCall(t, s, "carol", "user", "POST",
		`{"action":"update_link","key_id":"link-box","shared_with":["dave","carol","dave"]}`)
	if got := m["settings"].(map[string]any)["shared_with"].([]any); code != 200 || len(got) != 1 || got[0] != "dave" {
		t.Fatalf("share cleanup: %d %v", code, m)
	}
	// Revoking clears the link's settings.
	gpusCall(t, s, "carol", "user", "POST", `{"action":"revoke_link","key_id":"link-box"}`)
	if ls := s.store.LinkSettingsOf("carol", "link-box"); len(ls.SharedWith) != 0 {
		t.Fatalf("settings survive revoke: %+v", ls)
	}
}
