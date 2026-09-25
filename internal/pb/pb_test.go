package pb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockPB is a tiny fake PocketBase covering the endpoints the client uses.
func mockPB(t *testing.T, authOK bool) (*Client, *[]string) {
	t.Helper()
	var calls []string
	sr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/collections/users/auth-with-password":
			var body struct{ Identity, Password string }
			json.NewDecoder(r.Body).Decode(&body)
			if !authOK || body.Identity == "" {
				w.WriteHeader(400)
				w.Write([]byte(`{"message":"failed"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"token": "user-token",
				"record": map[string]any{
					"id": "rec1", "username": body.Identity,
					"role": "user", "verified": true,
				}})
		case r.URL.Path == "/api/collections/_superusers/auth-with-password":
			json.NewEncoder(w).Encode(map[string]any{"token": "super-token"})
		case r.URL.Path == "/api/collections/users/records" && r.Method == "GET":
			if !strings.Contains(r.URL.RawQuery, "perPage=100") {
				// filter lookup — echo the decoded filter back for assertion
				json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
					{"id": "rec9", "username": "found", "role": "user"},
				}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
				{"id": "rec1", "username": "bob", "role": "user"},
				{"id": "rec2", "username": "admin", "role": "admin"},
			}})
		case r.URL.Path == "/api/collections/users/records" && r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["passwordConfirm"] != body["password"] {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "rec-new", "username": body["username"], "role": body["role"]})
		case strings.HasPrefix(r.URL.Path, "/api/collections/users/records/") && r.Method == "PATCH":
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/api/collections/users/records/") && r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(sr.Close)
	return New(sr.URL), &calls
}

func TestAuthenticate(t *testing.T) {
	c, _ := mockPB(t, true)
	rec, err := c.Authenticate("bob", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Username != "bob" || rec.Role != "user" || rec.ID != "rec1" {
		t.Fatalf("record = %+v", rec)
	}
	// Failure path
	c2, _ := mockPB(t, false)
	if _, err := c2.Authenticate("bob", "bad"); err == nil {
		t.Fatal("expected error on failed auth")
	}
}

func TestAdminTokenCaching(t *testing.T) {
	c, calls := mockPB(t, true)
	c.SetSuperuser("admin@example.com", "pw")
	for i := 0; i < 3; i++ {
		if _, err := c.AdminToken(); err != nil {
			t.Fatal(err)
		}
	}
	superCalls := 0
	for _, c := range *calls {
		if strings.Contains(c, "_superusers") {
			superCalls++
		}
	}
	if superCalls != 1 {
		t.Fatalf("superuser auth called %d times, want 1 (cached)", superCalls)
	}
}

func TestFindUserEscapesQuotes(t *testing.T) {
	c, calls := mockPB(t, true)
	c.SetSuperuser("a@b.c", "pw")
	if _, err := c.FindUser("x' || true || '"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range *calls {
		if strings.Contains(c, "username%3D") || strings.Contains(c, "username=") {
			if strings.Contains(c, "''") || strings.Contains(c, "%27%27") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("filter not escaped: %v", *calls)
	}
}

func TestCreateUserSendsConfirm(t *testing.T) {
	c, _ := mockPB(t, true)
	c.SetSuperuser("a@b.c", "pw")
	rec, err := c.CreateUser("newuser", "n@llm.local", "pw123456", "user")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Username != "newuser" {
		t.Fatalf("created = %+v", rec)
	}
}
