package auth

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

//go:embed golden_test.json
var goldenData []byte

type goldenFile struct {
	PBKDF2  []struct{ Secret, Salt, Want string } `json:"pbkdf2"`
	Session struct {
		Secret string         `json:"secret"`
		Claims map[string]any `json:"claims"`
		Exp    int64          `json:"exp"`
		Token  string         `json:"token"`
	} `json:"session"`
}

func TestGoldenPBKDF2(t *testing.T) {
	var g goldenFile
	if err := json.Unmarshal(goldenData, &g); err != nil {
		t.Fatal(err)
	}
	for _, v := range g.PBKDF2 {
		if got := HashSecret(v.Secret, v.Salt); got != v.Want {
			t.Errorf("HashSecret(%q,%q) = %s, want %s", v.Secret, v.Salt, got, v.Want)
		}
	}
}

// TestSessionCrossPython proves the Go signer produces the exact token the
// Python signer produces for the same secret/claims/exp (golden from Python).
func TestSessionCrossPython(t *testing.T) {
	var g goldenFile
	if err := json.Unmarshal(goldenData, &g); err != nil {
		t.Fatal(err)
	}
	s := &Store{SessionSecret: g.Session.Secret, LocalKeys: map[string][]*KeyRecord{},
		SessionEpochs: map[string]int{"bob": 3}} // golden claims carry ep=3
	tok := s.SignSessionOrdered(g.Session.Exp,
		KV{"u", "bob"}, KV{"role", "user"}, KV{"ep", 3})
	if tok != g.Session.Token {
		t.Fatalf("token mismatch:\n got %s\nwant %s", tok, g.Session.Token)
	}
	claims, ok := s.VerifySession(g.Session.Token)
	if !ok {
		t.Fatal("verify failed on golden token")
	}
	if claims.U != "bob" || claims.Role != "user" || claims.Ep != 3 {
		t.Errorf("claims = %+v", claims)
	}
}

func TestSessionExpiryAndEpoch(t *testing.T) {
	s := &Store{SessionSecret: newSecret(16), LocalKeys: map[string][]*KeyRecord{},
		SessionEpochs: map[string]int{"bob": 0}}
	tok := s.SignSession(Claims{U: "bob", Role: "user"}, time.Hour)
	if _, ok := s.VerifySession(tok); !ok {
		t.Fatal("fresh token should verify")
	}
	// Expired token
	old := s.SignSessionOrdered(time.Now().Add(-time.Hour).Unix(),
		KV{"u", "bob"}, KV{"role", "user"})
	if _, ok := s.VerifySession(old); ok {
		t.Fatal("expired token must not verify")
	}
	// Tampered signature
	if _, ok := s.VerifySession(tok[:len(tok)-4] + "beef"); ok {
		t.Fatal("tampered token must not verify")
	}
	// Epoch bump kills outstanding sessions
	s.BumpEpoch("bob")
	if _, ok := s.VerifySession(tok); ok {
		t.Fatal("token with stale epoch must not verify after bump")
	}
}

func TestKeyLifecycle(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "users.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	plain, rec, err := s.CreateKey("bob", "main", "user", false)
	if err != nil {
		t.Fatal(err)
	}
	// Python parity: prefix = plaintext[:11], salt = prefix + "salt"
	if rec.Prefix != plain[:11] {
		t.Errorf("prefix = %s, want %s", rec.Prefix, plain[:11])
	}
	if rec.Salt != rec.Prefix+"salt" {
		t.Errorf("salt = %s, want prefix+salt", rec.Salt)
	}
	// Round trip: reload from disk, lookup must work.
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	user, k, ok := s2.LookupKey(plain)
	if !ok || user != "bob" || k.KeyID != "main" {
		t.Fatalf("LookupKey = %v %v %v", user, k, ok)
	}
	if _, _, ok := s2.LookupKey("sk-wrong"); ok {
		t.Fatal("wrong key must not resolve")
	}
	// Rotation grace: rotate => old key keeps working until grace passes.
	// Uses the production RotateKey (Python parity: rename to -retired-,
	// ISO grace_until, new key under original id).
	newPlain, graceUntil, err := s2.RotateKey("bob", "main", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if graceUntil == nil {
		t.Fatal("rotation must report grace deadline")
	}
	if _, _, ok := s2.LookupKey(plain); !ok {
		t.Fatal("retired key within grace must resolve")
	}
	if _, _, ok := s2.LookupKey(newPlain); !ok {
		t.Fatal("replacement key must resolve")
	}
	// After grace passes the retired one must not.
	rec2Grace := *graceUntil
	past := time.Now().Add(-time.Minute).Format(isoLayout)
	_ = rec2Grace
	for _, k := range s2.LocalKeys["bob"] {
		if strings.HasPrefix(k.KeyID, "main-retired-") {
			k.GraceUntil = &past
		}
	}
	if _, _, ok := s2.LookupKey(plain); ok {
		t.Fatal("expired rotation must not resolve")
	}
	// Revoke kills both the live key and its retired siblings.
	if !s2.RevokeKey("bob", "main") {
		t.Fatal("revoke should find the key")
	}
	if _, _, ok := s2.LookupKey(newPlain); ok {
		t.Fatal("revoked key must not resolve")
	}
}

func TestFileMode0600(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "users.json")
	s, _ := Open(p)
	if _, _, err := s.CreateKey("bob", "k", "user", false); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("users.json mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestLegacyKeys(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "keys.list")
	os.WriteFile(kf, []byte("# comment\nsk-legacy-one\nsk-legacy-two\n\n"), 0o600)
	s := &Store{LocalKeys: map[string][]*KeyRecord{}, SessionEpochs: map[string]int{},
		LegacyKeysFile: kf}
	got := s.LegacyKeys()
	if len(got) != 2 || got[0] != "sk-legacy-one" {
		t.Fatalf("legacy = %v", got)
	}
}
