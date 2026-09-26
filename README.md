# llm-gateway

**A single-binary gateway for self-hosted LLM fleets.** Route, authenticate,
quota, and account for inference across heterogeneous engines — with the
one thing proxies usually miss: awareness of what each GPU is actually
doing.

```
                 ┌───────────────────────────────┐
  clients ──────▶│  llm-gateway (one binary)      │──────▶ NInfer fork  (energy, lanes)
  (sk- keys,     │  · pool routing + failover     │──────▶ vLLM         (partial signal)
   LAN trust)    │  · cache-affinity routing      │──────▶ llama.cpp    (any OpenAI API)
                 │  · embedded PocketBase + SQLite│
                 └───────────────────────────────┘
```

**New here? The [setup guide](https://github.com/sprout-foundry/llm-gateway/blob/main/docs/start.md)
is the shortest path from install to first request** — or run the installer
and open `/guide` on your gateway.

## Why it's different

- **Cache-affinity routing.** Conversations are hashed (prefix chain) and
  routed to the GPU that already holds their KV cache — measured 99%+
  prefix-cache hits. Independent of client key/session discipline; new
  conversations spread by live load.
- **Engine-aware scoring.** With the [NInfer fork](#the-ninfer-fork--energy-metrics):
  lane occupancy, queue depth, KV pressure, per-engine tok/s. Reactive
  failover happens before the first streamed byte.
- **Honest cost accounting.** Real GPU energy (NVML via the fork), host
  overhead, capex amortization — charted against a **price book you set**
  (the gateway never derives your prices) on `/admin/costs`.
- **One binary.** Identity (PocketBase), history (SQLite), UI, OpenAPI
  docs — embedded. The systemd service is the whole deployment.

## Install (Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/sprout-foundry/llm-gateway/main/scripts/install.sh | bash
```

Installs a `llmgateway` systemd service, provisions the first admin
(password printed once), and verifies health. Pinned versions
(`--version vX.Y.Z`), source builds (`--build`), custom port, and
`--uninstall` are documented in the [Installation Runbook](docs/install.md).

Then: log in at `http://<host>:8033/`, mint a key on the Keys page, and

```bash
curl -s http://<host>:8033/v1/chat/completions \
  -H "Authorization: Bearer sk-..." \
  -d '{"model":"my-pool","messages":[{"role":"user","content":"hello"}]}'
```

## First run: bootstrap an engine

A gateway needs something to route to. The recommended engine is the
**NInfer fork** (below) — one script:

```bash
curl -fsSL https://raw.githubusercontent.com/sprout-foundry/llm-gateway/main/scripts/install-ninfer-engine.sh | bash -s -- --arch 120a
```

Already running vLLM or llama.cpp? Set `model_pools` in
`/opt/llm-gateway/llm_gateway.conf` (or the admin config page) to point at
it and restart the service. Pool knobs — thresholds, cache affinity,
capacity weights, size affinity — are all live-editable.

## The NInfer fork — energy metrics

Routing quality, energy/cost dashboards, and `/backends` all depend on
engine telemetry. The fork
([`alantheprice/ninfer-4090`](https://github.com/alantheprice/ninfer-4090),
branch `nvfp4-upstream-master`) adds what stock engines lack:

| Endpoint | What it provides |
|---|---|
| `GET /slots` | lane capacity, running + waiting requests |
| `GET /usage` | decode/prefill tok/s, KV pressure, cache-hit %, **energy kWh + $** (NVML, `--electricity-rate`) |
| `GET /metrics` | Prometheus text (vLLM-compatible parser) |

Counters persist across restarts (`--metrics-state`). Per-engine behavior:

| Engine | Load scoring | Energy | Cache affinity |
|---|---|---|---|
| **NInfer (fork)** | lane-aware (0.75·lanes + 0.15·queue + 0.10·KV-pressure) | ✅ NVML | ✅ full |
| vLLM | partial (Prometheus `running/waiting/KV`) | ❌ | by conversation content |
| llama.cpp / stock | none (scores 0.0, no overflow) | ❌ | by conversation content |

Build + serve details: [NInfer Engine Runbook](docs/ninfer-engine.md).

## Day-2 operations

- **Costs**: declare hosts + electricity rate, set your price book, watch
  value-vs-cost accumulate on `/admin/costs`. History in SQLite, restart-proof.
- **Quotas**: per-user daily token limits with automatic `429`s.
- **Keys**: 10 active per user, rotation with grace windows, instant
  revocation (kills live sessions via epoch bump).
- **Backups**: three paths to copy — see the
  [Operations Runbook](docs/operations.md).

## Docs

| Document | Contents |
|---|---|
| [docs/start.md](docs/start.md) | zero → first request |
| [docs/install.md](docs/install.md) | install/upgrade/uninstall runbook |
| [docs/ninfer-engine.md](docs/ninfer-engine.md) | building + serving the telemetry fork |
| [docs/operations.md](docs/operations.md) | costs, quotas, backups, troubleshooting |
| [docs/SPEC.md](docs/SPEC.md) | full behavioral specification |
| `/guide` on any running gateway | the same docs, embedded |

The guide pages are rendered into the release binary — an installed
gateway ships its own manual at `http://<host>:8033/guide`.

## API

OpenAPI 3.1, generated from the code: `/openapi.json`, `/openapi.yaml`,
`/docs` (interactive). Auth-gated like everything else (session, key, or
LAN trust).

## Status

Production-hardened on a two-GPU (RTX 5090 + RTX 6000 Pro) heterogeneous
pool serving multi-million-token daily traffic. Apache-2.0 — see
[LICENSE](LICENSE).
