# Installation Runbook (Linux)

## Requirements

- Linux (amd64 or arm64) with systemd; root via sudo
- One or more OpenAI-compatible LLM backends reachable over HTTP
  (for the full-featured path: the [NInfer fork](/guide/ninfer-engine))
- Go 1.27+ only if building from source (release installs download a binary)

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/sprout-foundry/llm-gateway/main/scripts/install.sh | bash
```

Useful flags (pass after `--`):

| Flag | Meaning |
|---|---|
| `--version vX.Y.Z` | pin a release (default: latest) |
| `--build` | build from source instead of downloading |
| `--port N` | gateway port (default 8033) |
| `--uninstall` | remove service + files (data kept unless `--purge`) |

What it does:

1. Creates the `llmgateway` system user and `/opt/llm-gateway`
2. Installs the binary + default `llm_gateway.conf`
3. Installs the systemd unit (`llm-gateway.service`)
4. Boots, then provisions the **first admin** and the **PB dashboard
   superuser** — the admin password is printed once, store it
5. Verifies `GET /health`

## Layout

| Path | What |
|---|---|
| `/opt/llm-gateway/llm-gateway` | binary |
| `/opt/llm-gateway/llm_gateway.conf` | config (form-editable at `/admin/config/page`) |
| `/opt/llm-gateway/users.json` | API keys + session state (0600) |
| `/opt/llm-gateway/usage.json` | usage mirror (SQLite is the store) |
| `/var/lib/llm-gateway/pb/` | embedded PocketBase + SQLite history |

## First login

1. `http://<host>:8033/` → log in as `admin`
2. Change the password (**Account** page)
3. Follow [Start Here §First 5 minutes](/guide/start)

## Upgrade

```bash
sudo systemctl stop llm-gateway
curl -fsSL .../install.sh | bash        # same command; replaces the binary
sudo systemctl start llm-gateway
```

Usage history survives: SQLite is the store and every boot re-merges the
JSON mirrors into it.

## Uninstall

```bash
curl -fsSL .../install.sh | bash -s -- --uninstall
# add --purge to also delete /var/lib/llm-gateway and /opt/llm-gateway
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| Service restarts with `bind: address already in use (8090)` | Another PocketBase owns 8090 — it must be stopped: `sudo systemctl disable --now pocketbase` |
| `DB not open after health-wait` at boot | Same root cause: port 8090 is taken |
| Login says invalid credentials for the admin | Re-run `sudo -u llmgateway /opt/llm-gateway/llm-gateway user create-admin admin` and use the printed password |
| Everything works locally, 401 from outside | That's `models_require_auth` / LAN trust — see [Operations Runbook](/guide/operations) |
