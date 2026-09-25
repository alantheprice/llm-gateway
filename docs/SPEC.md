# LLM Gateway — Behavioral Specification (Go port)

Source of truth for the Go single-binary port. Written from the behavior of
the Python gateway (llm_gateway.py @ 523feab) as observed in production, NOT
by copying its code. Tests are written against THIS document; the Go
implementation is written to pass the tests.

Goal: a **drop-in replacement** for the Python gateway's *inference plane*:
same endpoints, same config file, same users.json/usage.json formats, same
routing decisions. The web UI/identity plane (PocketBase login, invites,
admin pages) is explicitly OUT of scope for v1 — see §11.

## 1. Identity & Formats (compat-critical)

### 1.1 Key hashing (PBKDF2)
- Algorithm: PBKDF2-HMAC-SHA256, **60000 iterations**, salt = the key
  record's `salt` string (ASCII), output lowercase hex.
- `hash(sk, salt) = hex(pbkdf2_hmac_sha256(sk, salt, 60000))`
- Comparison is constant-time.

### 1.2 users.json schema (read/write compatible)
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

### 1.3 Session cookies (compat-critical)
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

## 2. HTTP surface (inference plane)

| Method | Path | Auth | Behavior |
|---|---|---|---|
| GET | `/health` | none | `200 "OK"` text/plain |
| GET | `/v1/models` | key or LAN-trust | catalog; pools collapse to virtual name; public_models filter if configured |
| POST | `/v1/chat/completions` | key or LAN-trust | route (pool or direct); stream-aware proxy |
| POST | `/v1/completions` | key or LAN-trust | direct to resolved backend |
| POST | `/v1/embeddings` | key or LAN-trust | direct to embedding backend |
| GET | `/usage` | key or LAN-trust | engine aggregate usage JSON |
| GET | `/metrics` | key or LAN-trust | Prometheus text |
| GET | `/slots` | key or LAN-trust | engine slots JSON |
| GET | `/backends` | key or LAN-trust | per-backend engine/score/lanes snapshot |
| ANY | other `/v1/*` | stricter | 404; if unknown subpath: log + 60/min/IP probe throttle |

- **Auth modes** (`_auth_required` semantics): if `gateway.trust_local_networks`
  is true AND source IP ∈ `local_networks` → no key needed. Else Bearer key
  required. **127.0.0.1 is NEVER trusted** (it's the tunnel path).
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
5. **Size affinity**: `wantsLarge = estTokens >= pool.large_prompt_tokens`
   (if configured >0). Members flagged `large_context` get +0.50 penalty
   when NOT wanted; small prompts get +0.50 on large_context members.
   If wantsLarge and best large-context member has score < 0.90 → pick it.
6. **Threshold filter**: eligible = score < pool.overflow_threshold; if none
   eligible → least-loaded single member.
7. **Choice**: min by `(score + sizePenalty - (member==leader ? sticky_bias:0),
   original index)`. Set leader = chosen.

### 5.5 Reactive failover
Try chosen member; on connect error OR HTTP 5xx/408 → mark tried, pick next
from remaining candidates (same selection), retry. Failover must happen
before first streamed byte. All members failing → last error status/body.
Loop guard: each member tried at most once.

## 6. Metrics polling
- Every `metrics.poll_interval` (10s): for each discovered backend, GET
  /slots + /usage (1s timeout), detect engine.
  - ninfer: lanes/running/waiting from /slots; spills_total/evictions_total
    from /usage; engine="ninfer".
  - vLLM: running/waiting/kv_usage from /metrics Prometheus text
    (`vllm:num_requests_running` etc.); engine="vllm".
- Scoring staleness: metrics older than `metrics.stale_threshold` (30s)
  → score 0.0.

## 7. Model discovery & catalog
- Scan `discovery.local_ports` on 127.0.0.1 and `discovery.remote_host`
  ports: GET /v1/models (1s timeout). Response model ids recorded with
  backend URL + capability hints (chat/embeddings).
- Catalog: pool models collapse to the virtual pool name; ids in
  `public_models` (if set) filter the output. Output shape mirrors OpenAI:
  `{"object":"list","data":[{"id":...,"object":"model",...}]}`.

## 8. Usage accounting
`usage.json` (0600, atomic writes, flush ~60s or on mutation count):
```json
{"users": {"<username>": {"requests": N, "prompt_tokens": N,
  "output_tokens": N, "keys": {"<key_id>": {...same3...}},
  "kinds": {"chat"/"embeddings"/"fim": {...same3...}}}}}
```
- kind from model name: contains "embed" → embeddings, "fim" → fim, else chat.
- Recorded once per request on the serving member, streaming included
  (tokens from backend usage or final SSE chunk).
- Legacy-key requests attribute to user "legacy" (or configured name).

## 9. Overflow (non-pool legacy path)
`overflow_pairs`: when primary model's serving backend score ≥ threshold,
retry on fallback backend with `fallback_model_id`. (Pool path supersedes
this when model_pools configured; kept for compat.)

## 10. Config file (same format as llm_gateway.conf)
Single JSON file, keys: gateway{port,trust_local_networks,api_keys_file,
internal_api_key_file}, discovery{local_ports,remote_host,remote_ports},
local_networks[], metrics{...}, backend_max_seqs{}, model_pools{},
overflow_pairs{}, public_models[], cache{ttl}.
- Auto-reload on mtime change (like the Python conf watcher).
- Env overrides: `LLM_GATEWAY_CONF` (path), `PORT` (listen port).

## 11. Explicitly out of scope for Go v1 (stays on Python instance)
- Web UI (templates/static), PocketBase identity, login/logout/sessions-as-
  auth-for-pages, invites, password flows, admin pages, `/chat` `/keys`
  `/account` `/admin` routes.
- seed-agent proxying (`/v1/agent/chat`) — add post-parity.
The Go binary serves the inference plane only; UI/identity remains on the
Python service until the Go port proves itself, then ports incrementally.

## 12. Observability endpoints (parity)
- `/backends`: `{"backends":{url:{"engine","score","lanes","running","waiting","tps","energy_daily_kwh","cache_hit_pct","last_updated"}}}`
- `/usage`: pass-through merge of backend /usage payloads keyed by backend.
