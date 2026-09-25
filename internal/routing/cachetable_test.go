package routing

import (
	"testing"
	"time"
)

func conv(seed string, system, u1, a1, u2, a2 string) []Message {
	msgs := []Message{{Role: "system", Content: system}, {Role: "user", Content: u1}}
	if a1 != "" {
		msgs = append(msgs, Message{Role: "assistant", Content: a1})
	}
	if u2 != "" {
		msgs = append(msgs, Message{Role: "user", Content: u2})
	}
	if a2 != "" {
		msgs = append(msgs, Message{Role: "assistant", Content: a2})
	}
	_ = seed
	return msgs
}

func TestDeepestPrefixStableAcrossTurns(t *testing.T) {
	// Turn 1: [S, U1] recorded on gpuB. Turn 2: [S, U1, A1, U2] must match
	// gpuB at depth 2 (its prefix = last turn's full conversation).
	ct := NewCacheTable(time.Hour, 100)
	ct.Record("qwen", conv("", "sys", "hello", "", "", ""), "gpuA")
	// depth guard: only len>=2 stored; [S,U1] is depth 2 → stored.
	if url, depth, ok := ct.Lookup("qwen", conv("", "sys", "hello", "reply1", "again", "")); !ok || url != "gpuA" || depth != 2 {
		t.Fatalf("turn-2 lookup = %q d%d ok%v, want gpuA d2", url, depth, ok)
	}
	// Turn 2 completes: record deeper prefix.
	ct.Record("qwen", conv("", "sys", "hello", "reply1", "again", ""), "gpuA")
	// Turn 3 must match at depth 4.
	if url, depth, ok := ct.Lookup("qwen", conv("", "sys", "hello", "reply1", "again", "reply2")); !ok || url != "gpuA" || depth != 4 {
		t.Fatalf("turn-3 lookup = %q d%d ok%v, want gpuA d4", url, depth, ok)
	}
}

func TestFullRequestHashNeverStaleMatches(t *testing.T) {
	// A conversation that GREW: old deepest hash is still a prefix of the
	// new request — must match (this is the whole point).
	ct := NewCacheTable(time.Hour, 100)
	ct.Record("qwen", conv("", "sys", "u1", "a1", "", ""), "gpuA")
	got, _, ok := ct.Lookup("qwen", conv("", "sys", "u1", "a1", "u2-extra-tokens-xyz", ""))
	if !ok || got != "gpuA" {
		t.Fatalf("grown conversation lost its cache: %q ok%v", got, ok)
	}
}

func TestSharedSystemPromptDoesNotCollide(t *testing.T) {
	// Same system prompt, different openers: no depth-1 storage → no match.
	ct := NewCacheTable(time.Hour, 100)
	ct.Record("qwen", conv("", "shared-system", "unique-opener-A", "", "", ""), "gpuA")
	if _, _, ok := ct.Lookup("qwen", conv("", "shared-system", "unique-opener-B", "", "", "")); ok {
		t.Fatal("different openers must not match (harness collision)")
	}
	// Identical opener collides at depth 2 — and that's a GENUINE hit
	// (the GPU really holds [S,U1]); the guards price it accordingly.
	ct.Record("qwen", conv("", "shared-system", "same-opener", "", "", ""), "gpuA")
	if url, depth, ok := ct.Lookup("qwen", conv("", "shared-system", "same-opener", "", "", "")); !ok || url != "gpuA" || depth != 2 {
		t.Fatal("identical prefix should match at depth 2")
	}
	// Divergence rule: a different conversation sharing only [S,U1] never
	// reaches a deep match on this entry — its continuation hashes differ.
	ct.Record("qwen", conv("", "shared-system", "same-opener", "reply-one", "more", ""), "gpuA")
	diverged := conv("", "shared-system", "same-opener", "completely-different-reply", "next", "")
	if url, depth, ok := ct.Lookup("qwen", diverged); !ok || url != "gpuA" || depth != 2 {
		t.Fatalf("diverged conversation: got %q d%d ok%v, want genuine gpuA d2 only", url, depth, ok)
	}
}

func TestModelSeedSeparatesEngines(t *testing.T) {
	ct := NewCacheTable(time.Hour, 100)
	msgs := conv("", "sys", "same text", "", "", "")
	ct.Record("qwen3.8-27b", msgs, "gpuA")
	if _, _, ok := ct.Lookup("qwen3.5-35b", msgs); ok {
		t.Fatal("same text on another model must not match (seed mismatch)")
	}
}

func TestTTLExpiry(t *testing.T) {
	ct := NewCacheTable(10*time.Millisecond, 100)
	ct.Record("qwen", conv("", "sys", "u1", "a1", "", ""), "gpuA")
	time.Sleep(15 * time.Millisecond)
	if _, _, ok := ct.Lookup("qwen", conv("", "sys", "u1", "a1", "u2", "")); ok {
		t.Fatal("expired entry must not match")
	}
}

func TestCapEviction(t *testing.T) {
	ct := NewCacheTable(time.Hour, 10)
	for i := 0; i < 30; i++ {
		ct.Record("qwen", conv("", "sys", string(rune('a'+i%26))+string(rune('a'+i/26)), "a", "", ""), "gpuA")
		time.Sleep(time.Millisecond) // distinct timestamps for oldest-first
	}
	if ct.Len() > 10 {
		t.Fatalf("cap not enforced: %d entries", ct.Len())
	}
}

func TestPickCacheGuards(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	ct := NewCacheTable(time.Hour, 100)
	msgs := conv("", "sys", "u1", "a1", "u2", "")
	members := []Member{
		{URL: "gpuA", ModelID: "m-a", Lanes: 6},
		{URL: "gpuB", ModelID: "m-b", Lanes: 8},
	}
	tr.Set("gpuA", &Load{Engine: "ninfer", Running: 0, Lanes: 6, LastUpdated: time.Now()})

	ct.Record("pool", msgs, "gpuA")
	// Deep match (depth 4) + idle GPU → hit.
	pick := PickCache(ct, "pool", members, tr, msgs, nil)
	if pick == nil || pick.URL != "gpuA" || !pick.CacheHit || pick.CacheDepth != 4 {
		t.Fatalf("deep pick = %+v, want gpuA cache-hit", pick)
	}

	// Shallow match (depth 2) + busy GPU (score 0.5 >= guard 0.50) → miss.
	shallow := conv("", "sys", "u1", "", "", "")
	tr.Set("gpuA", &Load{Engine: "ninfer", Running: 3, Lanes: 6, LastUpdated: time.Now()})
	ct.Record("pool", shallow, "gpuA")
	if pick := PickCache(ct, "pool", members, tr, shallow, nil); pick != nil {
		t.Fatalf("shallow match on busy GPU must be released: %+v", pick)
	}

	// Deep match survives the same load (guard 0.90).
	deep := conv("", "sys", "u1", "a1", "u2", "")
	tr.Set("gpuA", &Load{Engine: "ninfer", Running: 3, Lanes: 6, LastUpdated: time.Now()})
	ct.Record("pool", deep, "gpuA")
	if pick := PickCache(ct, "pool", members, tr, deep, nil); pick == nil {
		t.Fatal("deep match should survive 0.5 load")
	}

	// Matched member missing from members list → miss.
	foreign := []Member{{URL: "gpuX", ModelID: "m-x", Lanes: 6}}
	if pick := PickCache(ct, "pool", foreign, tr, msgs, nil); pick != nil {
		t.Fatal("match to non-member must be dropped")
	}
}
