# Connectors (remote MCP servers)

Connect **remote MCP servers** to the chat, and the model can use their tools: look up issues, query a database, read docs, whatever the server offers. You add a server by its URL; nothing runs on your computer.

[MCP](https://modelcontextprotocol.io) (Model Context Protocol) is the open standard many services use to expose tools to AI models.

## Add a server

1. In **Chat**, click **🔌 Connectors**.
2. Enter:
   - **Name:** a short name, e.g. `github` (lowercase letters, digits, `-`, `_`). Tools show up as `<name>__<tool>`.
   - **URL:** the server's MCP endpoint, e.g. `https://mcp.example.com/mcp`.
   - **Auth header** (if the server needs one): usually `Authorization` with the value `Bearer <your token>`. Some servers use another header, such as `X-API-Key`.
3. Click **Test** to connect and list its tools, then **Add**.

Turn on **🌐 Web** in the composer to use connectors. The toggle reads **Web + N connectors** while any are on. Each connector has its own on/off switch.

In a reply, each tool call shows as a chip, e.g. `🔌 github · list_issues ✓`. Hover it for the result. If a server can't be reached, the reply says so and the chat carries on without it.

## What works

| | |
|---|---|
| **Transports** | Streamable HTTP (current MCP) and the older HTTP + SSE transport (`/sse` endpoints). The gateway tries the current one first. |
| **Auth** | A static header: an API key or personal access token. Servers that only offer an OAuth sign-in flow aren't supported yet; use a token if the service offers one. |
| **Tools** | All of a server's tools. Text results go to the model; images in results are left out. |
| **Limits** | Up to 10 connectors per user. A tool call times out after 2 minutes. |

Stdio servers (the kind you run locally with `npx …` or `uvx …`) aren't connectors. Use a hosted version of the server if there is one.

## Security

- **Your header values are secret.** The gateway stores them server-side and never shows them again; the list shows only the last four characters. They're sent only to that server, with each chat request that uses connectors.
- **A connector's tools act with your token's permissions.** The model decides when to call them, based on your conversation. Give connectors tokens with only the access they need (read-only where possible).
- **Tool results go into the conversation**, and a server's text can try to steer the model. Add servers you trust.
- Connectors must be on the public internet and use `https://`. Only admins can add servers on the local network or over plain `http://`.

## For admins

Connectors run in the agent service (`seed-agent`) that powers the chat's Web mode. The gateway stores each user's servers with their account and passes the enabled ones to the agent with each request; the agent connects, lists the tools, and calls them during the conversation. The agent refuses connections to loopback, private and link-local addresses for non-admins, checked after DNS resolution.

API: `GET /api/mcp` lists the signed-in user's servers (header values masked); `POST /api/mcp` with `{"action": "add" | "update" | "delete" | "test", ...}` manages them.
