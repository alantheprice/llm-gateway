# Connectors (remote MCP servers)

Connect **remote MCP servers** to the chat, and the model can use their tools: look up issues, query a database, read docs, whatever the server offers. You add a server by its URL; nothing runs on your computer.

[MCP](https://modelcontextprotocol.io) (Model Context Protocol) is the open standard many services use to expose tools to AI models.

## Add a server

1. In **Chat**, click **🔌 Connectors**.
2. Enter:
   - **Name:** a short name, e.g. `github` (lowercase letters, digits, `-`, `_`). Tools show up as `<name>__<tool>`.
   - **URL:** the server's MCP endpoint, e.g. `https://mcp.example.com/mcp`.
   - **Sign-in**, one of:
     - **API key or token:** a header, usually `Authorization` with the value `Bearer <your token>`. Some servers use another header, such as `X-API-Key`.
     - **Sign in with the service (OAuth):** for servers that have you approve access in the browser. After **Add**, a window opens at the service; approve there and it closes by itself. The connector then shows **✓ Signed in**.
     - **None:** for public servers.
3. Click **Test** to connect and list its tools (for a token), then **Add**.

Turn on **🌐 Web** in the composer to use connectors. The toggle reads **Web + N connectors** while any are on. Each connector has its own on/off switch.

In a reply, each tool call shows as a chip, e.g. `🔌 github · list_issues ✓`. Hover it for the result. If a server can't be reached, the reply says so and the chat carries on without it.

## Finding servers

**Quick add** in the Connectors dialog fills in a few well-known remote servers:

| Server | What it does | Sign-in |
|---|---|---|
| DeepWiki | Docs and Q&A for any public GitHub repo | none |
| Context7 | Up-to-date documentation for libraries | none |
| Hugging Face | Search models, datasets and Spaces | none |
| Cloudflare Docs | Cloudflare's documentation | none |
| Notion, Linear, Sentry, Atlassian (Jira & Confluence) | Work with your own workspace | sign in with the service |
| GitHub | Repos, issues, pull requests | a personal access token |
| Stripe | Your Stripe account | a secret or restricted key |

For more, browse a directory and pick servers marked **remote** or **hosted** (their address is an `https://…/mcp` or `…/sse` URL):

- [Official MCP Registry](https://registry.modelcontextprotocol.io): the protocol's own catalog
- [Smithery](https://smithery.ai): many hosted servers, each with a remote URL
- [Glama](https://glama.ai/mcp/servers): a large directory you can filter by hosting
- [mcp.so](https://mcp.so): a community directory
- [modelcontextprotocol/servers](https://github.com/modelcontextprotocol/servers): reference servers and a long list of official integrations

## What works

| | |
|---|---|
| **Transports** | Streamable HTTP (current MCP) and the older HTTP + SSE transport (`/sse` endpoints). The gateway tries the current one first. |
| **Auth** | A header (API key or personal access token), or OAuth sign-in following the MCP authorization spec: the gateway finds the service's sign-in endpoints, registers itself as a client, signs you in with PKCE, and refreshes the token before it expires. Services that need a client registered by hand (no dynamic client registration) aren't supported for sign-in; use a token for those. |
| **Tools** | All of a server's tools. Text results go to the model; images in results are left out. |
| **Limits** | Up to 10 connectors per user. A tool call times out after 2 minutes. |

Stdio servers (the kind you run locally with `npx …` or `uvx …`) aren't connectors. Use a hosted version of the server if there is one.

## Security

- **Your header values and sign-in tokens are secret.** The gateway stores them server-side, encrypted, and never shows them again; the list shows only the last four characters of a header. They're sent only to that server, with each chat request that uses connectors. **Sign out** (or **Remove**) deletes the tokens; to revoke the gateway's access entirely, also remove it in the service's own settings.
- If a sign-in expires and can't be refreshed, the chat says so; click **Sign in** again.
- **A connector's tools act with your token's permissions.** The model decides when to call them, based on your conversation. Give connectors tokens with only the access they need (read-only where possible).
- **Tool results go into the conversation**, and a server's text can try to steer the model. Add servers you trust.
- Connectors must be on the public internet and use `https://`. Only admins can add servers on the local network or over plain `http://`.

## For admins

Connectors run in the agent service (`seed-agent`) that powers the chat's Web mode. The gateway stores each user's servers with their account and passes the enabled ones to the agent with each request; the agent connects, lists the tools, and calls them during the conversation. The agent refuses connections to loopback, private and link-local addresses for non-admins, checked after DNS resolution.

API: `GET /api/mcp` lists the signed-in user's servers (header values masked); `POST /api/mcp` with `{"action": "add" | "update" | "delete" | "test" | "sign_out", ...}` manages them (`"auth": "oauth"` on add for sign-in). Sign-in runs through `GET /api/mcp/oauth/start?id=<id>` and returns to `/api/mcp/oauth/callback`, on the address the browser used; the service must allow that address as a redirect (dynamic registration registers it).
