# Operations Runbook

Day-2 operations: costs, quotas, backups, users, troubleshooting.

## Cost accounting & the price book

The gateway tracks what your fleet **costs** and what its traffic is
**worth** — the worth numbers are yours, never derived:

1. **Config → Costs & hosts**: declare each physical box (IPs, overhead
   watts, hardware cost, purchase date, amortization years) and your
   electricity rate.
2. **Set your price book** on **Usage & costs → Your price book → Edit prices** (`price_book`): prompt / cached / generated
   $/1M. These are the numbers the "value" line uses. There is no
   recommended pricing; pricing is policy, and policy is yours.
3. **Usage & costs** (`/admin/costs`) shows the value-vs-cost daily chart: green (value at
   your prices) above red (actual cost = GPU energy + overhead + capex
   accrual) means the system pays for itself. History persists in SQLite
   (`cost_history` table) and survives restarts.

Engine energy comes from the NInfer fork's NVML accounting — see the
[NInfer runbook](/guide/ninfer-engine). Hosts without fork telemetry show
overhead + capex only.

**Services and GPUs.** A *service* is one engine (host + port); a *GPU* is
a physical card. Several services can share a card, and one service can
span several. Every engine on a card reports that card's whole draw, so
list each box's cards under **gpus** (one per line, `name = ports`):

```
RTX PRO 6000 = 8006
RTX 4090 = 8001, 8009
```

A card with no ports runs every service on the box; a port on two cards
spans both. The gateway then counts each card (or group of cards joined
by a spanning service) once, and splits its energy between the services
on it by tokens served. Without a gpus list, each service is assumed to
have its own card, and the Usage & costs page warns when a box has several
services reporting energy.

## Alerts

The gateway checks every 30 seconds and notifies you when something breaks. Each problem is sent once when it starts and once when it clears.

| Alert | When | Severity |
|---|---|---|
| **Service down** | An engine hasn't answered for 2 minutes (set with *alert after*). One that disappears from discovery counts as down too. | critical |
| **GPU link disconnected** | The agent of an admin-owned link, or of a link serving a shared model, has been gone for 2 minutes. | critical |
| **Engine restarted** | An engine's uptime went back: a crash, or a deliberate restart. | warning |
| **Engine keeps restarting** | 3+ restarts within 10 minutes. A GPU fault can need a reboot. | critical |
| **Disk nearly full** | Under 5% or 2 GB free where the gateway keeps its database and usage records. | critical |
| **Daily limit reached** | A user hits their token limit (once per user per day). | info |

Set it up in **Config → Alerts**: add a webhook, **Save**, then **Send a test alert**.

- **Phone push:** install [ntfy](https://ntfy.sh), subscribe to a long, unguessable topic, and add `https://ntfy.sh/<topic>` with format *ntfy*. Self-hosted ntfy works the same way.
- **Slack / Discord:** an incoming-webhook URL with format *Slack* or *Discord*.
- **Anything else:** format *JSON* posts `{kind, severity, title, message, resolved, at}`.

Each webhook chooses what it gets: everything, warnings and up (default), or critical only. You can switch individual alert kinds off.

Active alerts and the last notifications are on **Overview** (`/admin/overview`). When a service is gone for good, **Stop watching** it there; otherwise it keeps counting as down for a week. Every alert is also written to the gateway log (`journalctl -u llm-gateway | grep ALERT`).

## Per-user quotas

Admins → Users → set a daily token limit per user. Exceeding it returns
`429` with a retry hint until midnight UTC — the same UTC-day boundary
usage and cost accounting bucket by (the admin UI shows the rollover in
your local time). Admins and LAN-trusted unkeyed traffic are exempt by
design.

## Users, keys, sessions

- **Invites**: Admins → Users → invite (credential hand-off; the user
  must change the password on first login).
- **Keys**: max 10 active per user; rotation keeps the old key alive for
  a grace window; revocation kills sessions instantly (epoch bump).
- **PB dashboard**: `http://127.0.0.1:8090/_/` — superuser credentials in
  `/var/lib/llm-gateway/pb/.superuser-env` (or re-seed with
  `llm-gateway superuser upsert <email> <pass>`).

## Backups

What matters, in order:

| File | Recreate-able? | Backup |
|---|---|---|
| `/var/lib/llm-gateway/pb/` | No (accounts) | **yes — stop the gateway or use PB's backup API** |
| `/opt/llm-gateway/users.json` | No (key hashes, epochs) | yes |
| `/opt/llm-gateway/users.json.key` | No: without it, the encrypted secrets in `users.json` are lost | **yes, stored apart from users.json** |
| `/opt/llm-gateway/llm_gateway.conf` | Painfully | yes |
| `usage.json`, `cost_history.json` | Mirrors of SQLite | optional |

```bash
sudo systemctl stop llm-gateway
tar czf llm-gateway-backup.tgz /var/lib/llm-gateway/pb /opt/llm-gateway/{users.json,llm_gateway.conf}
# the secrets key goes somewhere else (another disk, a password manager)
cp -p /opt/llm-gateway/users.json.key /secure/place/
sudo systemctl start llm-gateway
```

### Secrets in users.json

The session signing secret, each user's UI key and connector (MCP) auth headers are stored **encrypted** in `users.json` (AES-256-GCM). The key is in `users.json.key` next to it (mode 600; override the path with `LLM_GATEWAY_SECRETS_KEY_FILE`), created on first start. Older files with plain values are encrypted automatically on the next start.

- Back the key up, but not in the same place as `users.json`: together they reveal the secrets.
- Lose the key and the gateway refuses to start, saying so. Restore the key, or move `users.json`'s encrypted values aside: users then sign in again, UI keys are re-issued and connectors need their tokens re-entered.
- A gateway older than this change can't read the encrypted values: rolling back signs everyone out and breaks connectors until they're re-entered.

## Data flow (why restarts are safe)

Usage is recorded in memory → **synced to SQLite every 60s and at
shutdown** → `usage.json`/`cost_history.json` are mirrors. Every boot
re-merges the mirrors into SQLite (idempotent max-merge). Losing at most
one minute of tallies requires killing the process twice inside the same
minute.

## Public exposure (Cloudflare tunnel)

The tunnel targets port 8033. `127.0.0.1` is never LAN-trusted (that's
the tunnel path), so tunnel traffic needs keys. Security posture: HMAC
session cookies with per-user epochs, per-IP rate limits, login backoff,
2 MiB body cap, no version banner.

## Troubleshooting

| Symptom | Diagnosis |
|---|---|
| `/backends` shows a member with score 0 always | Engine has no metrics endpoints (stock/llama.cpp) — see [NInfer runbook](/guide/ninfer-engine) |
| `energy` columns are 0 | Engine lacks `--electricity-rate` or is vLLM (no NVML reporting) |
| `bind: address already in use` (8090) at gateway boot | A standalone PocketBase is running — `sudo systemctl disable --now pocketbase` |
| `/usage/costs` shows `source: json` | SQLite ops not attached; check boot logs for `ops tables` errors |
| User hit 429 | Daily quota — raise it in Admins → Users, or wait for UTC midnight (UI shows the local-time equivalent) |
| Cache hit rates dropped after a config change | New pool name or member set = new affinity table; warms up over subsequent turns (see `cache-affinity depth=` in the journal) |

## Upgrading

Releases are tagged `vX.Y.Z`; `install.sh` re-run handles it (see
[Installation Runbook](/guide/install)). Skim the release notes for
config-schema notes — unknown fields survive (config overlay keeps old
values), but behavior changes are called out per release.
