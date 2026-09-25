# llm-gateway

A single-binary, engine-aware gateway for self-hosted LLM backends.
Route, authenticate, and account for inference across **heterogeneous
engines** (NInfer, vLLM, llama.cpp — anything speaking OpenAI-compatible
HTTP) with routing that understands what each engine is actually doing.

**The differentiator:** session pinning with KV-cache affinity. Repeat
requests from the same conversation are routed to the same backend so the
engine's prefix cache stays warm — measured 99%+ cache hit rates on
sequential chat. Plus engine-aware load scoring (lane occupancy, queue
depth, KV pressure/spills), reactive failover before the first streamed
byte, invite-only identity with no email requirement, and per-user/per-key
usage accounting.

```
                 ┌──────────────────────────┐
  clients ──────▶│  llm-gateway (one binary) │──────▶ NInfer   (local GPU)
  (keys/OAuth-   │  · pool routing           │──────▶ vLLM     (remote GPU)
   style keys)   │  · session pinning        │──────▶ llama.cpp
                 │  · failover + usage       │
                 └──────────────────────────┘
```

## Status

Work in progress. The inference plane (routing, pools, auth, usage,
metrics, admin/keys APIs, OpenAPI docs) is complete and tested; the web UI
port is in flight. See [`docs/SPEC.md`](docs/SPEC.md) for the full
behavioral spec.

## Quick start

```bash
go build -o llm-gateway ./cmd/gateway

LLM_GATEWAY_CONF=llm_gateway.conf ./llm-gateway
```

Config (`llm_gateway.conf`) declares backends, pools, thresholds, and
networks — see the commented example in this repo. Point `model_pools` at
your engines; the gateway discovers them, polls their metrics, and routes.

```bash
curl -s localhost:8033/health          # -> OK
curl -s localhost:8033/v1/models -H "Authorization: Bearer sk-..."
curl -s localhost:8033/v1/chat/completions -H "Authorization: Bearer sk-..." \
  -d '{"model":"my-pool","messages":[{"role":"user","content":"hi"}]}'
```

## Auth model

- **LAN trust (configurable):** hosts on `local_networks` may call inference
  without a key. `127.0.0.1` is treated as tunnel traffic and is *never*
  trusted (that's the Cloudflare tunnel path).
- **Keys:** `sk-...` bearer keys, PBKDF2-hashed at rest, per-user with
  rotation grace windows. Mint via the web UI or `POST /keys`.
- **Identity:** PocketBase (optional) backs login, invites, and account
  lifecycle; the gateway keeps its own key store.
- **Sessions:** HMAC cookies carrying a per-user epoch — disabling or
  deleting a user kills their live sessions instantly.
- **Rate limits:** per-client-IP (tunnel-aware via `CF-Connecting-IP`),
  login backoff, probe throttling on unknown `/v1/*` paths.

## API docs

Generated OpenAPI 3.1 via [huma](https://huma.rocks):

- `/openapi.json`, `/openapi.yaml`
- `/docs` — interactive UI

Docs endpoints are **auth-gated** like everything else: session cookie,
bearer key, or LAN trust.

## Design notes

- **Pool routing** (SPEC §5): score each member from engine metrics, pin
  sessions via `md5(pool|session) % members` with a headroom check, blend
  gateway-side in-flight counts (engine `running` lags bursts), scale by
  capacity, then size-affinity (large prompts → large-context members) and
  sticky-bias the current leader.
- **Failover**: on connect error/5xx/408, retry the next member *before any
  bytes reach the client* — safe for SSE.
- **Usage**: recorded once per request on the serving member (streaming
  included), per user/key/kind (chat / embeddings / fim), JSON store with
  atomic 0600 writes.

## License

TBD (Apache-2.0 or MIT before first public release).
