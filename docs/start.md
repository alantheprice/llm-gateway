# Start Here

Welcome to llm-gateway — a single-binary gateway for self-hosted LLM
backends. This page gets you from zero to a working system, then hands off
to the runbooks.

## What this is

One binary that sits between your AI clients and your GPU servers:

- **Routes** requests across pools of engines (NInfer, vLLM, llama.cpp —
  anything OpenAI-compatible), with per-engine load scoring, session/ KV-cache
  affinity, and failover that happens before the first streamed byte.
- **Authenticates**: per-user `sk-...` keys (PBKDF2-hashed), optional LAN
  trust, per-user daily quotas, login rate-limiting.
- **Accounts**: per-user / per-key / per-day usage in SQLite, plus full cost
  and energy accounting (real NVML energy from the NInfer fork).
- **Identity**: PocketBase embedded in the same binary — accounts, invites,
  sessions, admin UI. One process, one database.

## Install (Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/sprout-foundry/llm-gateway/main/scripts/install.sh | bash
```

or, pinned to a release:

```bash
curl -fsSL https://raw.githubusercontent.com/sprout-foundry/llm-gateway/main/scripts/install.sh | bash -s -- --version v0.1.0
```

The script installs a `llmgateway` systemd service and prints the
**first-admin password once** — store it. Details and flags:
[Installation Runbook](/guide/install).

## First 5 minutes

1. Open `http://<host>:8033/` and log in as `admin` (password from the
   installer — change it under **Account**).
2. **Point it at an engine.** No backends yet? Run the NInfer engine
   installer on your GPU box (see [NInfer Engine Runbook](/guide/ninfer-engine))
   or set `model_pools` in `/opt/llm-gateway/llm_gateway.conf` to any
   OpenAI-compatible server, then:
   ```bash
   sudo systemctl restart llm-gateway
   ```
3. **Mint a key**: Keys page → new key → copy (`sk-...`).
4. **First request**:
   ```bash
   curl -s http://<host>:8033/v1/chat/completions \
     -H "Authorization: Bearer sk-..." \
     -d '{"model":"qwen3.8-27b","messages":[{"role":"user","content":"hello"}]}'
   ```
5. **Watch it work**: `/admin/costs` (cost + energy), `/usage/me`
   (your tokens), `/backends` (live per-engine load).

## Where to go next

| I want to… | Read |
|---|---|
| Install/upgrade the gateway | [Installation Runbook](/guide/install) |
| Serve models with the NInfer fork (energy metrics) | [NInfer Engine Runbook](/guide/ninfer-engine) |
| Price it, set quotas, back up, debug | [Operations Runbook](/guide/operations) |
| Full behavioral spec | [docs/SPEC.md](https://github.com/sprout-foundry/llm-gateway/blob/main/docs/SPEC.md) |

## Ten thousand feet

```
clients (sk- keys) ──▶ llm-gateway :8033
                        │  auth · quota · routing · usage · costs
                        │  embedded PocketBase (accounts) + SQLite (history)
                        ▼
        NInfer fork ──────────────── vLLM ────────── llama.cpp
        /slots /usage /metrics      /metrics         (no metrics:
        energy via NVML             partial signal   scored 0.0)
```
