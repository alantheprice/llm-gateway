package auth

import (
	"path/filepath"
	"testing"
)

// Minting a UI key must not invalidate prior ui keys beyond a small ring:
// with several gateway surfaces sharing users.json, each mints on
// restart/login and a hard clobber killed other surfaces' live sessions
// (observed 2026-09-25: agent chat 401'd 13s after a login on another port).
func TestCreateUIKeyKeepsRing(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	k1, _, err := store.CreateUIKey("bob", "user")
	if err != nil {
		t.Fatal(err)
	}
	k2, _, err := store.CreateUIKey("bob", "user")
	if err != nil {
		t.Fatal(err)
	}
	k3, _, err := store.CreateUIKey("bob", "user")
	if err != nil {
		t.Fatal(err)
	}
	// All three still authenticate...
	for i, k := range []string{k1, k2, k3} {
		if _, _, ok := store.LookupKey(k); !ok {
			t.Fatalf("ui key #%d no longer valid after later mints", i+1)
		}
	}
	// ...and named keys survive untouched.
	plain, _, err := store.CreateKey("bob", "laptop", "user", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateUIKey("bob", "user"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.LookupKey(plain); !ok {
		t.Fatal("named key clobbered by ui-key mint")
	}
	// Ring is bounded: mint 3 more, the oldest should age out.
	var last string
	for i := 0; i < 3; i++ {
		p, _, err := store.CreateUIKey("bob", "user")
		if err != nil {
			t.Fatal(err)
		}
		last = p
	}
	if _, _, ok := store.LookupKey(k1); ok {
		t.Fatal("oldest ui key should have aged out of the ring")
	}
	if _, _, ok := store.LookupKey(k3); ok {
		t.Fatal("k3 should have aged out after 3 more mints (ring = 3)")
	}
	if _, _, ok := store.LookupKey(last); !ok {
		t.Fatal("newest ui key should still be in the ring")
	}
}
