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
metrics, admin/keys/config APIs, OpenAPI docs) is complete and tested; the
web UI (chat, keys, usage graphs, admin users/system/config) is functional
and tracking the Python original. See [`docs/SPEC.md`](docs/SPEC.md) for the
full behavioral spec.

## Quick start

```bash
go build -o llm-gateway ./cmd/gateway

LLM_GATEWAY_CONF=llm_gateway.conf ./llm-gateway
```

Config (`llm_gateway.conf`) declares backends, pools, thresholds, and
networks — see the commented example in this repo. Point `model_pools` at
your engines; the gateway discovers them, polls their metrics, and routes.
Admins can edit every knob live at **/admin/config/page** (saves apply
immediately *and* persist to the conf file).

```bash
curl -s localhost:8033/health          # -> OK
curl -s localhost:8033/v1/models       # public by default (see models_require_auth)
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

## Engine metrics: the NInfer fork

Routing quality, the admin energy/cost views, and `/backends` all depend on
what each engine *reports*. This project was built against a specialized
**NInfer fork** — [`alantheprice/ninfer-4090`](https://github.com/alantheprice/ninfer-4090)
(branch `nvfp4-upstream-master`) — which adds three observability endpoints
stock engines don't have:

| Endpoint | What it provides |
|---|---|
| `/slots` | lane capacity (`max_concurrency`), running + waiting requests |
| `/usage` | decode/prefill tok/s, KV pressure (spills/evictions), cache-hit %, **energy** (kWh + $ via NVML, `--electricity-rate`) |
| `/metrics` | Prometheus text (also parsed, vLLM-compatible) |

Counters persist across restarts (`--metrics-state`), so daily/30-day energy
and cost rollups survive engine restarts.

**Per-engine, what actually happens:**

- **NInfer (the fork)** — full fidelity: lane-aware scoring
  (`0.75·lanes + 0.15·queue + 0.10·KV-pressure`, weights in `metrics.ninfer_*`),
  session pinning with cache-affinity, per-backend tok/s, cache-hit %, and the
  admin energy/cost dashboards.
- **vLLM** — partial: `running/waiting/KV-usage` from its Prometheus
  `/metrics`; scored with the vLLM heuristic (`metrics.default_max_seqs`,
  per-backend overrides in `metrics.backend_max_seqs`). No energy (vLLM
  doesn't report it).
- **llama.cpp or stock NInfer (no fork endpoints)** — degrades gracefully:
  discovery and routing still work (model catalog, pool selection, failover,
  usage accounting by tokens), but there's no load signal, so the gateway
  can't tell a busy backend from an idle one — scores stay 0.0 and overflow
  never triggers. Pin sessions if you run one of these in a pool; treat it
  as capacity-planning-by-hand.

Everything else (auth, keys, invites, chat UI, usage accounting, OpenAPI
docs) is engine-agnostic — any OpenAI-compatible backend works.

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

[Apache-2.0](LICENSE)
