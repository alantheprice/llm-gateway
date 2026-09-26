package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLookupCacheRevalidates: a cached (already verified) key must stop
// resolving the moment its record is disabled or replaced on disk.
func TestLookupCacheRevalidates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := s.CreateKey("bob", "main", "user", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.LookupKey(plain); !ok { // populates the cache
		t.Fatal("fresh key must resolve")
	}
	if len(s.verified) != 1 {
		t.Fatalf("verified cache size = %d, want 1", len(s.verified))
	}
	s.DisableUser("bob")
	if _, _, ok := s.LookupKey(plain); ok {
		t.Fatal("disabled user's cached key must not resolve")
	}

	// External writer replaces the record (reload yields new pointers).
	plain2, _, err := s.CreateKey("carol", "main", "user", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.LookupKey(plain2); !ok {
		t.Fatal("carol's key must resolve")
	}
	s.mu.Lock()
	for _, k := range s.LocalKeys["carol"] {
		k.KeyHash = strings.Repeat("0", 64) // simulate a rewritten hash
	}
	s.mu.Unlock()
	if _, _, ok := s.LookupKey(plain2); ok {
		t.Fatal("cached key whose stored hash changed must not resolve")
	}
}

// TestLookupSamePrefixForgery: matching a key's public prefix is not enough.
func TestLookupSamePrefixForgery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := s.CreateKey("bob", "main", "user", false)
	if err != nil {
		t.Fatal(err)
	}
	forged := plain[:11] + strings.Repeat("A", len(plain)-11)
	if _, _, ok := s.LookupKey(forged); ok {
		t.Fatal("same-prefix forgery must not resolve")
	}
	if len(s.verified) != 0 {
		t.Fatal("failed lookups must not populate the cache")
	}
}

// TestLookupPrefixlessRecord: hand-edited records without a prefix are
// still checked by hash (no silent lockout).
func TestLookupPrefixlessRecord(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users.json")
	plain := "sk-handwritten-key-0123456789"
	doc := `{"session_secret":"x","local_keys":{"ops":[{"key_id":"hand","prefix":"",` +
		`"salt":"pepper","key_hash":"` + HashSecret(plain, "pepper") + `","active":true}]}}`
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if user, _, ok := s.LookupKey(plain); !ok || user != "ops" {
		t.Fatalf("prefixless record: got %q %v", user, ok)
	}
}

// TestLookupBadKeyIsCheap: with many stored keys, an unknown key must not
// run PBKDF2 against each of them (~9ms apiece).
func TestLookupBadKeyIsCheap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, _, err := s.CreateKey("bob", "k", "user", false); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	for i := 0; i < 20; i++ {
		s.LookupKey("sk-definitely-not-a-real-key")
	}
	// Old behavior: 20 × 20 × ~9ms ≈ 3.6s. Generous bound for slow CI.
	if d := time.Since(start); d > time.Second {
		t.Fatalf("20 bad-key lookups took %v", d)
	}
}
