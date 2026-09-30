# LLM Gateway — Behavioral Specification

Source of truth for the gateway's behavior. Tests are written against this
document; the implementation is written to pass the tests.

## 1. Identity & Formats

### 1.1 Key hashing (PBKDF2)
- Algorithm: PBKDF2-HMAC-SHA256, **60000 iterations**, salt = the key
  record's `salt` string (ASCII), output lowercase hex.
- `hash(sk, salt) = hex(pbkdf2_hmac_sha256(sk, salt, 60000))`
- Comparison is constant-time.

### 1.2 users.json schema

Secret values (`session_secret`, `auto_key_plaintexts` values, `mcp_servers[*].headers` values) are stored as `enc:v1:<base64(nonce‖AES-256-GCM ciphertext)>` with the key in `users.json.key`; plain values are still accepted on read (files written by older gateways).

```json
{
  "session_secret": "<hex string>",
  "local_keys": {
    "<username>": [
      {"key_id": "main", "prefix": "sk-wHz", "salt": "...", "key_hash": "<hex>",
       "created": "<iso>", "active": true, "role": "user",
       "rotating": false, "grace_until": null, "ui": false}
    ]
  },
  "must_change_pw": {"<username>": true},
  "session_epochs": {"<username>": <int>}
}
```
Rules:
- Writes are atomic (tmp + rename) and set file mode 0600.
- A key matches iff `(active OR (rotating AND grace_until not passed))`
  AND pbkdf2(plaintext, salt) == key_hash.
- Grace semantics: `rotating=true` with `grace_until` (unix seconds, may be
  null). On any store mutation check, expired rotations flip to
  `active=false, rotating=false, grace_until=null` (lazy expiry).

### 1.3 Session cookies
- Cookie name: `llmgw_session`.
- Token format: `base64url(json(claims) + "|" + <exp_unix>)` + `.` +
  `hex(hmac_sha256(body, session_secret))` — note the payload is base64 of
  the raw `{"..."}|<exp>` string, no padding stripped beyond std b64url.
- Claims: `u` (username), `role`, `ep` (session epoch int), optional `mcp`.
- Validation: HMAC constant-time compare, then `exp >= now`, then
  `claims.ep == session_epochs[u]` (missing epoch file entry = 0, token
  without `ep` = 0 → still valid for pre-existing tokens).
- TTL: 7 days. Cookie flags: HttpOnly; SameSite=Lax; Path=/.

### 1.4 Legacy keys
- File `/etc/llm-inference/api-keys.list`: one key per line, `#` comments.
  Accepted as admin/operator keys when local-store auth fails.

## 2. HTTP surface

| Method | Path | Auth | Behavior |
|---|---|---|---|
| GET | `/health` | none | `200 "OK"` text/plain |
| GET | `/v1/models` | none by default; key/LAN when `gateway.models_require_auth` | catalog; pool member ids hidden, virtual names synthesized, the caller's private GPUs added; every entry carries `context_length` (tokens — a shared model's is its members' largest) |
| POST | `/v1/chat/completions` | key or LAN-trust | route (pool or direct); stream-aware proxy |
| POST | `/v1/completions` | key or LAN-trust | direct to resolved backend |
| POST | `/v1/embeddings` | key or LAN-trust | shared model, else the embedding backend |
| POST | `/v1/images/generations` | key or LAN-trust | text to image (OpenAI images API): shared model or alias, then a single engine, then a private link the caller can use |
| GET | `/usage` | key or LAN-trust | engine aggregate usage JSON |
| GET | `/metrics` | key or LAN-trust | Prometheus text |
| GET | `/slots` | key or LAN-trust | engine slots JSON |
| GET | `/backends` | key or LAN-trust | per-backend engine/score/lanes snapshot |
| ANY | other `/v1/*` | stricter | 404; if unknown subpath: log + 60/min/IP probe throttle |

- **Auth modes**: if `gateway.trust_local_networks` is true AND source IP ∈
  `local_networks` → no key needed. Else Bearer key
  required. **127.0.0.1 is NEVER trusted** (it's the tunnel path).
- **Exception — model catalog:** `GET /v1/models` is public by default
  (`gateway.models_require_auth: false`): OpenAI-compatible clients probe
  the catalog before they hold a key. Set the knob to `true` to gate it
  like every other `/v1` surface.
- Missing/invalid key → `401 {"error":{"message":"Invalid or missing API key","type":"unauthorized"}}`.

## 3. Client IP resolution (rate-limit keys ONLY)
`clientIP(r)`:
1. TCP peer == 127.0.0.1 → `tunnel:<CF-Connecting-IP>` (trim; fallback
   `tunnel:unknown`). CF header is trusted ONLY for loopback peers.
2. Otherwise → TCP peer string.
Used by: global throttle, login backoff, probe throttle. NEVER used for
LAN-trust decisions (those use the raw peer).

## 4. Rate limiting
- **Global**: 300 req/min per clientIP, sliding window, 429 JSON
  `{"error":{"message":"rate limited","type":"rate_limited"}}`. `/health`
  exempt. Map bounded ~5000 keys (evict oldest 1000).
- **Probe throttle**: unknown /v1/* subpaths: 60/min per clientIP + warning
  log line each.

## 5. Session pinning & pool routing (the differentiator — spec exactly)

### 5.1 Prompt token estimate
`estTokens(messages)`: sum over messages of chars/4 for text content
(strings and content-part arrays; `image_url` parts count 2000). Min 1.

### 5.2 Session key
`X-Session-Id` header (trim, max 120 chars, prefix `hdr:`) > `key:<key_id>`
fallback. Empty if neither.

### 5.3 Per-backend score (0..1)
Maintained per backend from polled metrics (`last_updated` staleness > 30s
or missing → 0.0):
- **ninfer**: `min(1, max(0, 0.75*laneP + 0.15*queueP + 0.10*kvP))` where
  `laneP = min(running/lanes,1)`, `queueP = min(waiting/max(lanes/2,1),1)`,
  `kvP = 1.0 if (Δspills>0 or Δevictions>0 since last obs) else 0`
  (first observation → 0). Weights configurable: metrics.ninfer_*_weight.
- **vllm/other**: `min(1, (running/maxSeqs)*0.6 + kvUsage*0.3 +
  min(waiting,5)/5*0.1)`, maxSeqs default 3 (config metrics.default_max_seqs;
  override map backend_max_seqs).

### 5.4 Member selection (deterministic, testable)
Inputs: pool config, member scores, estTokens, sessionKey, inFlight map,
current leader (per pool, in-memory).
1. Compute scores per member.
2. **Pin**: if sessionKey non-empty: `pinned = members[md5("pool|session")
   mod N]` (md5 of the joined string, mod member list order as configured).
   Honor pin iff `pinnedRunning + inFlight[pinned] + 1 <= pinnedLanes` AND
   `pinnedScore < 0.90`. Pinned member becomes leader.
3. **In-flight blend**: `score = max(score, min((running+inFlight)/lanes,1))`.
4. **Capacity scaling** (if pool.capacity_bias > 0): for each member,
   `eff = 1 + capacity_bias * (1 - lanesWeight/maxLanesWeight)` where
   lanesWeight = member.capacity_weight or lanes; `score *= eff` (cap 1.0).
   Only applied when lane counts differ.
5. **Context fit** (before any scoring): members whose context window
   (engine `/v1/models` `max_model_len`, or the member's `max_context`
   override) is smaller than `estTokens + requested max_tokens` are
   skipped. Unknown windows count as fitting; if no member fits, all are
   kept and the engine rejects with its own error. Cache affinity applies
   at every prompt size.
6. **Threshold filter**: eligible = score < pool.overflow_threshold; if none
   eligible → least-loaded single member.
7. **Choice**: min by `(score - (member==leader ? sticky_bias:0),
   original index)`. Set leader = chosen.

### 5.5 Reactive failover
Try chosen member; on connect error OR HTTP 5xx/408 → mark tried, pick next
from remaining candidates (same selection), retry. Failover must happen
before first streamed byte. All members failing → last error status/body.
Loop guard: each member tried at most once.

## 6. Metrics polling
- Every `metrics.poll_interval` (10s): for each discovered backend, GET
  /slots + /usage (1s timeout), detect engine. **The /slots + /usage shape
  is specific to the NInfer fork** (`alantheprice/ninfer-4090`,
  `nvfp4-upstream-master`): stock engines don't serve it, and energy/cost
  reporting is fork-only (NVML + `--electricity-rate`, persisted via
  `--metrics-state`).
  - ninfer: lanes/running/waiting from /slots; spills_total/evictions_total
    from /usage; engine="ninfer".
  - vLLM: running/waiting/kv_usage from /metrics Prometheus text
    (`vllm:num_requests_running` etc.); engine="vllm".
  - Neither shape: backend stays discoverable/routable but load scores
    remain 0.0 (no overflow, no lane math).
- Scoring staleness: metrics older than `metrics.stale_threshold` (30s)
  → score 0.0.

## 7. Model discovery & catalog
- Scan `discovery.local_ports` on 127.0.0.1 and `discovery.remote_host`
  ports: GET /v1/models (1s timeout). Response model ids recorded with
  backend URL + capability hints (chat/embeddings).
- Catalog: pool MEMBER model ids are hidden (clients must use the pool
  virtual name so cache-affinity routing can't be bypassed); all other
  discovered models (embeddings, FIM, standalone) are advertised; pool
  virtual names are synthesized if no backend reports them. Output shape
  mirrors OpenAI:
  `{"object":"list","data":[{"id":...,"object":"model",...}]}`.
- `/chat/config` returns every discovered model id (sorted), member ids
  included — the chat UI can target a specific engine if it wants.

## 8. Usage accounting
`usage.json` (0600, atomic writes, flush ~60s or on mutation count):
```json
{"users": {"<username>": {"requests": N, "prompt_tokens": N,
  "output_tokens": N, "keys": {"<key_id>": {...same3...}},
  "kinds": {"chat"/"embeddings"/"fim"/"images": {...same3...}}}}}
```
- kind from model id: segments starting "embed" → embeddings, "fim" → fim,
  image-model names (FLUX, SDXL, DALL·E, …) → images, else chat.
- Recorded once per request on the serving member, streaming included
  (tokens from backend usage or final SSE chunk).
- Requests from the legacy key file (§1.4) attribute to user `operator`;
  LAN-trusted unkeyed requests attribute to user `local`.

## 9. Aliases
A pool's `aliases` are other model names that route exactly like the pool
(same member pick, same conversation affinity, keyed by the pool's name).
Resolution order: pool name or alias → engine serving that model id →
private link. Responses echo the name the client called.

## 10. Config file
Single JSON file (strict JSON — no comments), keys: gateway{port,
trust_local_networks, models_require_auth, api_keys_file,
internal_api_key_file, users_file, usage_file},
discovery{local_ports,remote_host,remote_ports},
local_networks[], metrics{...}, backend_max_seqs{}, model_pools{} (each
with members[], aliases[], cache_affinity, max_pool_share,
overflow_threshold, sticky_bias, capacity_bias), hosts[], price_book{},
cache{ttl}.
- Auto-reload on mtime change.
- Admin UI (`/admin/config/page` → GET/POST `/admin/config`) reads and
  writes this file: POST overlays onto the current config, validates,
  persists atomically, and hot-swaps (tracker weights + networks re-derived;
  port + identity-plane file paths are load-time only). The write-back means
  UI edits survive restarts.
- Env overrides: `LLM_GATEWAY_CONF` (path), `PORT` (listen port).

## 11. Observability endpoints
- `/backends`: `{"backends":{url:{"engine","score","lanes","running","waiting","tps","energy_daily_kwh","cache_hit_pct","last_updated"}}}`
- `/usage`: pass-through merge of backend /usage payloads keyed by backend.

## 12. At-rest encryption (per-user vault)

Chat history (messages + titles) and document-search data (doc names +
chunk text) are stored **encrypted per user** once the user signs in once.
A stolen copy of `data.db` (and `users.json`, and even PB's password
hash) cannot be opened — only the user's password can.

Key schedule:
- Each user has a random **DEK** (AES-256-GCM) that exists **only in
  gateway process memory** (never written to disk). Sealed values are
  `"vault:v1:" + base64(nonce ‖ ciphertext)`; values without the prefix
  are plaintext (pre-vault rows) and pass through untouched.
- The DEK is wrapped with a **wrap key = PBKDF2-SHA256(login password,
  32-byte salt, 100 000 iterations)** derived **in the browser**
  (`/static/vault.js`). The gateway receives the derived 32-byte key, not
  the password. The salt is public by design; the wrapped DEK + salt are
  stored in the `vault` table (one row per user).
- The only server-side KDF run is a **rekey** during a password change
  (the old and new passwords are already in that request for PocketBase
  auth; the rekey re-wraps the cached DEK under the new password's wrap
  key **before** the password is committed, and re-keys back if the commit
  fails — the vault and the password can never disagree on disk).

Lifecycle:
- **First login**: the login page always posts a browser-derived wrap key
  + salt; the gateway creates the vault, caches the DEK, and runs a
  one-shot background migration that re-seals the user's existing
  plaintext chats and doc chunks in place (idempotent; progress visible
  at `GET /api/vault`). Login never blocks on vault failure.
- **Locked** (restart, or before first login this process): sealed-data
  reads fail with HTTP 503 `{"error":…,"code":"vault_locked"}`; the chat
  list degrades (titles blanked, no ciphertext leaks) and the UI's
  global fetch interceptor turns the 503 into a "enter your password to
  unlock" modal that retries the original request. Document upload +
  `POST /v1/rag/search` fail fast with the same 503 (no plaintext is
  written under a live vault).
- **Admin password reset** re-keys from the cached DEK; if the user's DEK
  is not cached (no active session), the reset still succeeds and the
  response carries a loud WARNING — the encrypted data is then
  unreachable and must be treated as such.
- Users who never sign in (API-key-only) have no vault; their data stays
  plaintext (nothing to protect without a password-derived key).
