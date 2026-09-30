package server

import (
	"fmt"
	"testing"
	"time"

	"llmgateway/internal/embeddedpb"
	"llmgateway/internal/vault"
)

// TestVaultLifecycleE2E: the at-rest encryption lifecycle against the real
// embedded PB + SQLite — the paths that can permanently lock a user out:
//
//  1. Enable (client-provided salt, as the login page does) + the one-shot
//     migration re-seals existing plaintext chats and doc chunks in place.
//  2. A fresh manager over the same DB starts LOCKED: seal/open fail, a
//     wrong wrap key is rejected, the right one unlocks, values round-trip.
//  3. Rekey (password change) moves the wrap under a new key; the old key
//     stops working.
//  4. ReplaceDocChunks is atomic: a mid-loop insert failure rolls back the
//     whole delete+insert, leaving the original chunks intact.
func TestVaultLifecycleE2E(t *testing.T) {
	pbApp, _, _ := testPB(t)
	if err := pbApp.InitVaultSchema(); err != nil {
		t.Fatalf("InitVaultSchema: %v", err)
	}
	u := fmt.Sprintf("vlt-e2e-%d", time.Now().UnixNano())

	// Seed plaintext data: two chats, one doc with three chunks.
	for _, c := range []embeddedpb.Chat{
		{ChatMeta: embeddedpb.ChatMeta{ID: "c1", Title: "alpha-title", Model: "m", Created: 1, Updated: 100}, Messages: `["alpha-msg"]`},
		{ChatMeta: embeddedpb.ChatMeta{ID: "c2", Title: "beta-title", Model: "m", Created: 2, Updated: 200}, Messages: `["beta-msg"]`},
	} {
		if err := pbApp.SaveChat(u, c); err != nil {
			t.Fatalf("SaveChat seed: %v", err)
		}
	}
	docChunks := []embeddedpb.DocChunk{
		{DocID: "d1", Seq: 0, Text: "chunk zero", Dims: 2},
		{DocID: "d1", Seq: 1, Text: "chunk one", Dims: 2},
		{DocID: "d1", Seq: 2, Text: "chunk two", Dims: 2},
	}
	if err := pbApp.AddDoc(u, "d1", "docname", 12, 3); err != nil {
		t.Fatalf("AddDoc: %v", err)
	}
	if err := pbApp.ReplaceDocChunks(u, "d1", docChunks); err != nil {
		t.Fatalf("ReplaceDocChunks seed: %v", err)
	}

	// ---- 1. Enable: fresh vault + migration seals the seeded rows ----
	salt := vault.Random(32) // the login page generates the salt in the browser
	wk1 := vault.Random(32)  // stands in for browser KDF(pw, salt)
	v1 := NewVault(pbApp)
	if err := v1.Enable(u, wk1, salt); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if enabled, unlocked := v1.Status(u); !enabled || !unlocked {
		t.Fatalf("Status after Enable = (%v, %v), want (true, true)", enabled, unlocked)
	}
	// Wait for the migration goroutine to finish.
	deadline := time.Now().Add(5 * time.Second)
	for v1.Migration(u).Active {
		if time.Now().After(deadline) {
			t.Fatalf("migration still active: %+v", v1.Migration(u))
		}
		time.Sleep(50 * time.Millisecond)
	}
	c1, err := pbApp.GetChat(u, "c1")
	if err != nil || c1 == nil {
		t.Fatalf("GetChat c1: %v", err)
	}
	if !vault.IsSealed(c1.Title) || !vault.IsSealed(c1.Messages) {
		t.Fatalf("migration did not seal the chat: title=%q msgs=%q", c1.Title, c1.Messages)
	}
	chunks, err := pbApp.ListDocChunks(u, "d1")
	if err != nil || len(chunks) != 3 {
		t.Fatalf("ListDocChunks: %v (got %d)", err, len(chunks))
	}
	for _, c := range chunks {
		if !vault.IsSealed(c.Text) {
			t.Fatalf("migration did not seal doc chunk seq=%d: %q", c.Seq, c.Text)
		}
	}

	// ---- 2. A fresh manager over the same DB starts locked ----
	v2 := NewVault(pbApp)
	if _, err := v2.Seal(u, "plain"); err == nil {
		t.Fatal("Seal on a locked vault succeeded, want errVaultLocked")
	}
	if _, err := v2.Open(u, c1.Messages); err == nil {
		t.Fatal("Open of a sealed value on a locked vault succeeded, want errVaultLocked")
	}
	if err := v2.Unlock(u, vault.Random(32)); err == nil {
		t.Fatal("Unlock with a wrong wrap key succeeded, want an error")
	}
	if err := v2.Unlock(u, wk1); err != nil {
		t.Fatalf("Unlock with the correct key: %v", err)
	}
	if dec, err := v2.Open(u, c1.Title); err != nil || dec != "alpha-title" {
		t.Fatalf("Open(title) = %q, %v; want %q", dec, err, "alpha-title")
	}
	if dec, err := v2.Open(u, c1.Messages); err != nil || dec != `["alpha-msg"]` {
		t.Fatalf("Open(msgs) = %q, %v", dec, err)
	}
	env, err := v2.Envelope(u)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v2.Open(u, env.Seal("roundtrip")); err != nil || got != "roundtrip" {
		t.Fatalf("Seal/Open round-trip = %q, %v", got, err)
	}

	// ---- 3. Rekey: the old wrap key stops working after the change ----
	wk2 := vault.Random(32) // stands in for KDF(new pw, salt)
	if err := v2.Rekey(u, wk1, wk2); err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	v3 := NewVault(pbApp)
	if err := v3.Unlock(u, wk1); err == nil {
		t.Fatal("old wrap key still unlocks after rekey, want an error")
	}
	if err := v3.Unlock(u, wk2); err != nil {
		t.Fatalf("new wrap key does not unlock after rekey: %v", err)
	}

	// ---- 4. ReplaceDocChunks atomicity: mid-loop failure rolls back ----
	bad := append(docChunks, embeddedpb.DocChunk{DocID: "d1", Seq: 0, Text: "dup", Dims: 2}) // PK clash on the 4th insert
	if err := pbApp.ReplaceDocChunks(u, "d1", bad); err == nil {
		t.Fatal("ReplaceDocChunks with a duplicate seq succeeded, want a PK error")
	}
	after, err := pbApp.ListDocChunks(u, "d1")
	if err != nil {
		t.Fatalf("ListDocChunks after failed replace: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("after failed atomic replace: got %d chunks, want the original 3 (rollback)", len(after))
	}

	// Cleanup: remove the vault row so re-runs start clean.
	if err := pbApp.DeleteVaultKey(u); err != nil {
		t.Fatalf("DeleteVaultKey: %v", err)
	}
}
