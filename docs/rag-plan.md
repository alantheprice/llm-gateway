# Document Upload + RAG — plan

Personal document search, as a chat tool alongside MCP connectors. Three decisions locked:
1. **Vector store:** embedded SQLite + in-process cosine. No external vector DB — keeps the
   "one binary, embedded" identity.
2. **Retrieval:** explicit. A "Document search" toggle in the tool selection (same place as the
   MCP connectors). Off by default; when on, the `document_search` tool is available.
3. **Scope:** personal only. A user uploads and searches only their own documents.

## Where things live

Two processes, two modules:

- **gateway** (`llmgateway`, this repo) — owns the DB, the embedding backend, and the RAG
  search. New table + store + `/v1/rag/search` + `/api/documents` (upload/list/delete) + a
  `docs_tool` flag injected into the agent body.
- **seed-agent** (`llm-gateway/seed-agent`, the nested module in this repo, deployed to
  `/home/aprice/llm-gateway/seed-agent`) — the tool
  executor. New `document_search` tool that calls the gateway's `/v1/rag/search` with the
  caller's key. Deployed via `deploy-seed-agent.sh` (systemd `seed-agent.service`).
- **UI** (this repo, `internal/web/`) — Documents page + a "Documents" row in the Connectors
  (tool-selection) dialog + a chat.js tool chip.

## Data model (gateway)

New table(s) in `internal/embeddedpb/docs.go`, mirroring `chats.go`:

```
docs(
  user TEXT, id TEXT, name TEXT, size INTEGER,
  created INTEGER, deleted INTEGER, PRIMARY KEY(user,id)
)
doc_chunks(
  user TEXT, doc_id TEXT, seq INTEGER, text TEXT,
  embedding BLOB,          -- float32 little-endian vector
  dims INTEGER, PRIMARY KEY(user,doc_id,seq)
)
INDEX doc_chunks_user ON doc_chunks(user)
```

- `DocStore` interface in `internal/server/documents.go` (mirror `ChatStore`, `chats.go:25`);
  field on `Server` + one line in `SetOps` (`server.go:168`); `InitDocsSchema()` called in
  `main.go` next to `InitChatsSchema` (`main.go:138`).
- Embedding stored as a raw float32 BLOB. Search loads the caller's chunks and does cosine in
  Go. Fine at personal scale (≤ tens of thousands of chunks/user).

## Ingestion (gateway)

`POST /api/documents` (session auth, mirror `handleAPIChats`): multipart `file`.
- Accept text/markdown/html/csv/json (`.txt .md .markdown .html .csv .json .rst`). **PDF/DOCX/
  OCR are follow-ups** (need a text extractor / vision route) — reject them with a clear 415.
- Limits (config `rag`): max doc bytes (5 MB), max docs/user (100), max chunks/doc (4000).
- Parse → chunk (~3200-char chunks, ~300-char overlap, paragraph-aware) → embed in batches →
  upsert `docs` + `doc_chunks`. Ingest **synchronously**, return `{id, name, chunks}`.

## Embedding helper (gateway)

`s.embedTexts(ctx, []string) ([][]float32, error)` in `internal/server/documents.go`:
- Resolve the embedding backend the same way `handleEmbeddings` does (`handlers.go:517`):
  `poolFor(name)` first, else scan `s.backends` for `Embeds`. `name` = config
  `rag.embedding_model` (default: the `Qwen3-Embedding-0.6B` pool).
- Body `{"model": mid, "input": [texts]}` → `s.backendRequest(POST, url, "/v1/embeddings",
  body, 30s)` → parse `{data:[{embedding:[...], index}]}`. Batch ≤ 32 texts/call.
- Count embedding tokens in usage (`s.usage.Record...`) under the embedding model name.

## Retrieval (gateway)

`POST /v1/rag/search` (key auth via `checkAuth`, so the caller's own docs come back):
```
in:  {query, top_k?}      out: {results:[{doc_id,name,seq,text,score}]}
```
Embed the query, load the caller's non-deleted chunks, cosine rank, return top-k (default 5).
No matches → empty `results` (not an error).

## Tool selection (the explicit gate)

- Per-user pref `docs_enabled` (mirror `ChatWeb` in `UserPrefs`, `internal/auth/prefs.go`).
- UI: a "📄 Document search" row in the Connectors dialog (not a URL connector — just an
  enabled checkbox). Reads/writes `prefs.docs_enabled` via `/api/prefs`.
- `withMCPServers` (`mcp.go:343`) additionally sets `m["docs_tool"] = <bool>` (the user's
  enabled state). The sidecar is the only consumer; it is loopback-only, so the flag is safe.

## seed-agent tool

- `main.go`: add `DocsTool bool \`json:"docs_tool"\`` to the `in` struct (line ~371).
- `ToolsExecutor` (`mcp.go:541`): add `docs bool`, `apiKey string`. When `docs`:
  - `GetTools()` includes a `document_search` core.Tool (param `query`).
  - `Execute`: route `document_search` calls to `POST {gatewayBase}/v1/rag/search` with
    `{query, top_k:5}` + `Authorization: Bearer apiKey`; format results as
    `[n] <name> (document)\n<text>` blocks (same `[n]` citation shape as `web_search`).
- System prompt (`main.go:437`): when `in.DocsTool`, append a line: "You also have
  `document_search`, which searches the user's own uploaded documents (personal only). Use it
  when the question likely concerns their documents; cite sources as [n]."
- Set `docs`/`apiKey` on the executor in the handler (`main.go:429`) from `in.DocsTool` /
  `in.APIKey`.

## UI

- New page `/documents` (template `documents.html`): list (name, size, chunks, date), upload
  (file input + drag-drop), delete. Register the route in `Handler()` + a nav link.
- `connectors.js` / `_layout.html`: add the "📄 Document search" enabled row.
- `chat.js`: a `document_search` tool-chip case (📄 "searching your documents: <args>");
  answers already cite `[n]` (markdown), and the nerd panel shows the tool summary.

## Config (`internal/config/config.go`)

```
rag: {
  embedding_model: "Qwen3-Embedding-0.6B",  // "" = first discovered Embeds backend
  chunk_chars: 3200, overlap_chars: 300,
  max_docs_per_user: 100, max_doc_bytes: 5242880, max_chunks_per_doc: 4000
}
```
`RAGCfg` struct + defaults in `applyDefaults`.

## Security

- Per-user isolation end-to-end: uploads, storage, and retrieval are all scoped to the
  authenticated user; the sidecar calls `/v1/rag/search` with the caller's key, so the gateway
  returns only that user's chunks.
- Upload size/quota limits; filename not used for path traversal (id is a random slug,
  regex-validated).
- No new external network calls from the gateway.

## Build order

1. gateway backend (schema + store + embed + ingest + search + config + tests)
2. seed-agent tool (+ test) — depends on the stable `/v1/rag/search` contract
3. UI (Documents page + selection row + chip) — depends on the `/api/documents` + prefs contract
4. deploy both (gateway `bin/` swap + `systemctl restart llm-gateway`; sidecar
   `deploy-seed-agent.sh`), verify end-to-end live
