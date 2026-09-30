# seed-agent — agentic web-search loop for LLM Gateway

Go sidecar implementing tool-calling agents (web search + page fetch) on top
of the LLM Gateway, using [sprout-foundry/seed](https://github.com/sprout-foundry/seed).

## Architecture

```
Chat UI ──> LLM Gateway :8033 (auth, usage billing, pool routing)
                 │
                 └──> seed-agent :8095 (this sidecar)
                        ├── seed agent loop (query → LLM → tools → answer)
                        ├── web_search   → Jina search API
                        ├── fetch_page   → Jina Reader API
                        └── LLM calls    ──> back through the Gateway
                                             (so usage stays attributed)
```

- Agent LLM calls carry the **caller's API key** — usage lands on the right user
- Search uses **Jina** (`JINA_API_KEY` in the environment)
- Emits SSE events: `start`, `tool_start`, `tool_end`, `content`, `error`, `done`

## Run

```bash
export GATEWAY_API_KEY=sk-...        # any valid gateway key
export JINA_API_KEY=jina_...         # from your environment
./seed-agent                         # listens on 127.0.0.1:8095
```

## API

`POST /v1/agent/chat`
```json
{"messages": [{"role": "user", "content": "..."}]}
```
Optional header `X-Session-Id` — pins the conversation to one GPU through
the gateway's session-affinity routing.

Response is SSE. Events:
- `start` `{model}`
- `tool_start` `{tool, args, status}` / `tool_end` `{tool, status, summary}`
- `done` `{content}` — the final answer

## Gateway integration

The gateway exposes `POST /v1/agent/chat` (auth-gated like all `/v1/*`) which
proxies to this sidecar. The chat UI has a tools toggle that switches between
plain chat and agent mode.

## Deployment

`seed-agent.service.template` — copy to `/etc/systemd/system/seed-agent.service`,
fill in the two env values, `systemctl enable --now seed-agent`.
