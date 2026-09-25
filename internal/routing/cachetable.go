// Content-derived cache affinity: map a conversation to the GPU that
// already holds its KV prefix tree — by hashing the conversation content
// itself, not by session/key discipline.
//
// Mechanism: after a response, walk the messages once computing a chained
// hash at each exchange boundary (h0=seed, h1=H(h0,msg[0]), h2=H(h1,
// msg[1])…) and store ONLY the deepest hash → backend. On the next
// request, compute all boundary hashes and take the LONGEST match.
//
// Why this shape:
//   - Full-request hashing never matches (context grows every turn). The
//     deepest-prefix scheme matches this turn's prefix = last turn's full.
//   - Hashing only the system prompt would make every conversation from
//     the same harness collide. Depth-1 is never stored; and even a depth-2
//     collision (identical [system, opener]) self-destructs on the next
//     exchange because the assistant reply differs.
//   - Rolling chain = O(n) over the text, one pass, no re-hash. FNV-1a 64:
//     ~30µs for 8K tokens of text; GPU work is seconds. Not on any critical path.
//
// Entries are in-memory only: a stale entry costs one prefill miss, never
// correctness. TTL + LRU-ish cap + backend-restart invalidation keep it
// honest. Load guards live at the call site (routing rules), not here.
package routing

import (
	"sync"
	"time"
)

// CacheTable: conversation-hash → backend URL.
type CacheTable struct {
	mu      sync.Mutex
	entries map[uint64]cacheEntry
	ttl     time.Duration
	max     int
	// lastSweep throttles the eviction pass.
	lastSweep time.Time
}

type cacheEntry struct {
	backend string
	depth   int // boundary count of the stored hash (bigger = stronger)
	at      time.Time
}

func NewCacheTable(ttl time.Duration, max int) *CacheTable {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if max <= 0 {
		max = 8192
	}
	return &CacheTable{
		entries:   map[uint64]cacheEntry{},
		ttl:       ttl,
		max:       max,
		lastSweep: time.Now(),
	}
}

// fnv1a64: chained hashing primitive (fast, fine for affinity — nothing
// security-sensitive).
func fnv1a64(seed uint64, b []byte) uint64 {
	h := seed
	for _, c := range b {
		h ^= uint64(c)
		h *= 0x100000001b3
	}
	return h
}

const fnvOffset uint64 = 0xcbf29ce484222325

// Message is the minimal view of a chat message for hashing. MarshalText
// lets the caller feed role+content however it likes.
type Message struct {
	Role    string
	Content string
}

// BoundaryHashes: rolling hash after each message boundary. Index i in the
// result = hash of messages[0..i] (len == len(msgs)). seed should be the
// pool/model name so different models never cross-match.
func BoundaryHashes(seed string, msgs []Message) []uint64 {
	out := make([]uint64, len(msgs))
	h := fnv1a64(fnvOffset, []byte(seed))
	for i, m := range msgs {
		h = fnv1a64(h, []byte(m.Role))
		h = fnv1a64(h, []byte{'\x00'})
		h = fnv1a64(h, []byte(m.Content))
		out[i] = h
	}
	return out
}

// Record: after a successful response, remember that this conversation
// (its deepest prefix hash) lives on `backend`. Only stores depth >= 2
// (system-only conversations would match every request from the same
// harness — exactly the failure mode this design avoids).
func (t *CacheTable) Record(seed string, msgs []Message, backend string) {
	if len(msgs) < 2 || backend == "" {
		return
	}
	hashes := BoundaryHashes(seed, msgs)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.evictLocked()
	t.entries[hashes[len(hashes)-1]] = cacheEntry{
		backend: backend,
		depth:   len(msgs),
		at:      time.Now(),
	}
}

// Lookup: find the backend holding the longest known prefix of this
// conversation. Returns backend URL, matched depth (messages hashed,
// 2+), ok. Depth-1 hashes are never stored so a shared system prompt
// can't pin; the caller guards shallow matches more aggressively.
func (t *CacheTable) Lookup(seed string, msgs []Message) (string, int, bool) {
	if len(msgs) < 2 {
		return "", 0, false
	}
	hashes := BoundaryHashes(seed, msgs)
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	bestIdx := -1
	var bestBackend string
	for i := len(hashes) - 1; i >= 1; i-- { // skip index 0 (system-only); deepest first
		if e, ok := t.entries[hashes[i]]; ok {
			if now.Sub(e.at) > t.ttl {
				delete(t.entries, hashes[i])
				continue
			}
			if bestIdx == -1 {
				bestIdx, bestBackend = i, e.backend
			}
		}
	}
	if bestIdx == -1 {
		return "", 0, false
	}
	return bestBackend, bestIdx + 1, true
}

// Invalidate: drop a conversation's entry (e.g. backend died and failover
// moved it — the caller re-Records against the new backend instead).
func (t *CacheTable) Invalidate(seed string, msgs []Message) {
	hashes := BoundaryHashes(seed, msgs)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(hashes) > 0 {
		delete(t.entries, hashes[len(hashes)-1])
	}
}

// evictLocked: drop expired + oldest-over-cap. Swept at most once a
// minute; the table is tiny so even a full scan is nothing.
func (t *CacheTable) evictLocked() {
	now := time.Now()
	if now.Sub(t.lastSweep) < time.Minute && len(t.entries) < t.max {
		return
	}
	t.lastSweep = now
	for h, e := range t.entries {
		if now.Sub(e.at) > t.ttl {
			delete(t.entries, h)
		}
	}
	if len(t.entries) < t.max {
		return
	}
	// Over cap: drop the oldest third.
	type kv struct {
		h  uint64
		at time.Time
	}
	all := make([]kv, 0, len(t.entries))
	for h, e := range t.entries {
		all = append(all, kv{h, e.at})
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].at.Before(all[i].at) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	drop := len(all) / 3
	for i := 0; i < drop; i++ {
		delete(t.entries, all[i].h)
	}
}

// Len: current entry count (metrics/tests).
func (t *CacheTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
