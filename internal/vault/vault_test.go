package vault

import (
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

// TestKDFGoldenVector pins the browser<->gateway KDF contract. The browser
// derives the wrap key with a pure-JS PBKDF2-SHA256 (see internal/web/static/
// vault.js); this test computes the same vector with x/crypto and checks it
// against a value independently verified in Node + Python. If the browser KDF
// ever diverges (a transcription or iteration bug), the derived keys would
// never match and every user's data would be unrecoverable — so the golden
// value here is the contract.
func TestKDFGoldenVector(t *testing.T) {
	const pw = "correct horse battery staple-テスト"
	salt := make([]byte, 32)
	for i := range salt {
		salt[i] = byte(i)
	}
	got := pbkdf2.Key([]byte(pw), salt, PBKDF2Iterations, KeySize, sha256.New)
	want := "2a58cf5a0b7a8af7018d4eb831cb2a97e3da741edd4c9d2196a3fef3eb2bb705"
	if hexEncode(got) != want {
		t.Fatalf("PBKDF2 golden vector mismatch:\n got %s\n want %s", hexEncode(got), want)
	}
}

// hexEncode avoids importing encoding/hex just for the test.
func hexEncode(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = h[c>>4]
		out[i*2+1] = h[c&0x0f]
	}
	return string(out)
}

func TestEnvelopeRoundTrip(t *testing.T) {
	dek := Random(KeySize)
	env, err := NewEnvelope(dek)
	if err != nil {
		t.Fatal(err)
	}
	plain := "hello vault 你好 — a longer message than one block, " +
		strings.Repeat("padding to force multiple GCM blocks. ", 20)
	sealed := env.Seal(plain)
	if !IsSealed(sealed) {
		t.Fatal("sealed value missing prefix")
	}
	if sealed == plain {
		t.Fatal("seal returned the plaintext")
	}
	got, err := env.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != plain {
		t.Fatalf("round trip mismatch: %q", got[:40])
	}
}

func TestEnvelopePassThrough(t *testing.T) {
	env, _ := NewEnvelope(Random(KeySize))
	// Seal is idempotent on already-sealed + empty values, and encrypts
	// legacy plaintext (the migration path). Open passes un-prefixed
	// values through unchanged (legacy read path).
	if got := env.Seal(""); got != "" {
		t.Errorf("Seal(\"\") = %q, want pass-through", got)
	}
	sealed := env.Seal("payload")
	if env.Seal(sealed) != sealed {
		t.Error("Seal is not idempotent on sealed values")
	}
	if legacy := "legacy plaintext stays readable"; IsSealed(env.Seal(legacy)) {
		// sealed now, but must still round-trip
		got, err := env.Open(env.Seal(legacy))
		if err != nil || got != legacy {
			t.Errorf("legacy round trip = %q, %v", got, err)
		}
	}
	// Open: un-prefixed + empty pass through; sealed opens.
	if got, _ := env.Open("plain"); got != "plain" {
		t.Errorf("Open(un-prefixed) = %q, want pass-through", got)
	}
	if got, _ := env.Open(""); got != "" {
		t.Errorf("Open(\"\") = %q", got)
	}
	if got, err := env.Open(sealed); err != nil || got != "payload" {
		t.Errorf("Open(sealed) = %q, %v", got, err)
	}
}

func TestEnvelopeWrongKey(t *testing.T) {
	dek1, dek2 := Random(KeySize), Random(KeySize)
	env1, _ := NewEnvelope(dek1)
	env2, _ := NewEnvelope(dek2)
	sealed := env1.Seal("secret")
	if _, err := env2.Open(sealed); err != ErrWrongKey {
		t.Fatalf("expected ErrWrongKey, got %v", err)
	}
}

func TestWrapUnwrapDEK(t *testing.T) {
	wrap, dek := Random(KeySize), Random(KeySize)
	wrapped, err := WrapDEK(wrap, dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapDEK(wrap, wrapped)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if string(got) != string(dek) {
		t.Fatal("DEK round trip mismatch")
	}
	if _, err := UnwrapDEK(Random(KeySize), wrapped); err != ErrWrongKey {
		t.Fatalf("expected ErrWrongKey, got %v", err)
	}
	if _, err := WrapDEK(Random(16), dek); err == nil {
		t.Fatal("short wrap key should be rejected")
	}
}

func TestSealRandomness(t *testing.T) {
	env, _ := NewEnvelope(Random(KeySize))
	a, b := env.Seal("same"), env.Seal("same")
	if a == b {
		t.Fatal("seal is not fresh-nonce per call")
	}
}

func TestRandomDistinct(t *testing.T) {
	if string(Random(32)) == string(Random(32)) {
		t.Fatal("Random returned identical output")
	}
	if _, err := NewEnvelope(Random(16)); err == nil {
		t.Fatal("wrong-size DEK should be rejected")
	}
	_ = rand.Reader
}
