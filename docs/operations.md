# Operations Runbook

Day-2 operations: costs, quotas, backups, users, troubleshooting.

## Cost accounting & the price book

The gateway tracks what your fleet **costs** and what its traffic is
**worth** — the worth numbers are yours, never derived:

1. **Config → Costs & hosts**: declare each physical box (IPs, overhead
   watts, hardware cost, purchase date, amortization years) and your
   electricity rate.
2. **Set your price book**: `price_book` — prompt / cached / generated
   $/1M. These are the numbers the "value" line uses. There is no
   recommended pricing; pricing is policy, and policy is yours.
3. **`/admin/costs`** shows the value-vs-cost daily chart: green (value at
   your prices) above red (actual cost = GPU energy + overhead + capex
   accrual) means the system pays for itself. History persists in SQLite
   (`cost_history` table) and survives restarts.

Engine energy comes from the NInfer fork's NVML accounting — see the
[NInfer runbook](/guide/ninfer-engine). Hosts without fork telemetry show
overhead + capex only.

## Per-user quotas

Admins → Users → set a daily token limit per user. Exceeding it returns
`429` with a retry hint until midnight in the gateway host's local
time zone (the same day boundary usage history uses). Admins and LAN-trusted
unkeyed traffic are exempt by design.

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
| `/opt/llm-gateway/llm_gateway.conf` | Painfully | yes |
| `usage.json`, `cost_history.json` | Mirrors of SQLite | optional |

```bash
sudo systemctl stop llm-gateway
tar czf llm-gateway-backup.tgz /var/lib/llm-gateway/pb /opt/llm-gateway/{users.json,llm_gateway.conf}
sudo systemctl start llm-gateway
```

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
| User hit 429 | Daily quota — raise it in Admins → Users, or wait for midnight (gateway host local time) |
| Cache hit rates dropped after a config change | New pool name or member set = new affinity table; warms up over subsequent turns (see `cache-affinity depth=` in the journal) |

## Upgrading

Releases are tagged `vX.Y.Z`; `install.sh` re-run handles it (see
[Installation Runbook](/guide/install)). Skim the release notes for
config-schema notes — unknown fields survive (config overlay keeps old
values), but behavior changes are called out per release.
