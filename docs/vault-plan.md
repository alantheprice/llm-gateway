# Chat/document vault (per-user at-rest encryption) — plan

**Decision (locked 2026-09-30):** per-user vault (option B). The **login password is
the passphrase** — no second secret. A *password change* re-keys cleanly (old+new
known); a *password reset* orphans the data (documented, explicit).

## Threat model (honest)

- ✅ Stops file/backup theft: a stolen `data.db` reveals only ciphertext (no operator
  key file exists — the key is password-derived, held nowhere).
- ✅ Superuser isolation: the gateway stores only the password *hash*; even a
  superuser with `data.db` + `users.json` + the hash cannot unwrap another user's
  DEK. Per-user secrecy.
- ❌ Not: a fully compromised gateway process (DEKs live in process memory while
  unlocked) or a fully compromised OS user with /proc access.
- Warning: Documents: `text` + `name` are encrypted; **`embedding` BLOBs stay plaintext**
  (the in-process cosine search runs over them). Partial guarantee for docs, full
  for chats.

## Key hierarchy

```
password (browser-only, at login/enable/change)
  └─ wrap_key = PBKDF2-SHA256(password, vault_salt, 100_000)   [32B]
        └─ DEK = AES-256-GCM-open(wrapped_dek, wrap_key)        [32B, random at setup]
              └─ data: chats.messages, chats.title, docs.name,
                  doc_chunks.text  = "vault:v1:" + b64(nonce||GCM(DEK, pt))
```

- `vault` table (same SQLite): `user PK, salt BLOB(32), wrapped_dek BLOB, created`.
  Plaintext (legacy) values pass through unchanged — prefix `vault:v1:` detects
  ciphertext (same pattern as `enc:v1:` in `internal/auth/secrets.go`).
- DEK cache: in-memory `map[user][]byte` (mutex, bounded). **Never persisted.**
  Missing → "vault locked" (code `vault_locked`, HTTP 503).

## Unlock paths

1. **Login** (browser derives, form carries): `login.html` JS fetches
   `GET /api/vault/wrapinfo?user=<name>` (salt; unauthenticated — the salt is not
   secret) *before* submitting; derives wrap_key via WebCrypto; adds hidden field
   `vault_key` (hex). `handleLogin` → after PB auth OK → `vault.Unlock(user,
   wrapKey)`. No row (not enabled yet) → no-op; unlock failure → login succeeds,
   vault stays locked (UI can re-unlock).
2. **Re-unlock** (session, on demand): `POST /api/vault/unlock {wrap_key}` —
   triggered by the global `vault_locked` interceptor modal (type password again).
3. **Lock now**: `POST /api/vault/lock` — clears the in-memory DEK.

## Enable + migration (explicit, user-initiated)

Settings → Security card: "Enable vault" → dialog asks for the login password (the
passphrase) → browser derives wrap_key (salt from wrapinfo) → `POST
/api/vault/enable {wrap_key}`:
- gen salt + random DEK, store wrapped row, cache DEK
- **one-shot background migration**: for the user's non-deleted chats (messages +
  title) and docs (name) + chunks (text): encrypt each plaintext value, re-write.
  Idempotent (skip `vault:v1:` values), batched, progress in
  `GET /api/vault` (`{enabled, migration:{chats_done,chats_total,docs_done,
  docs_total,active}}`). After migration, everything the gateway writes is
  encrypted automatically.

## Write/read seams (server layer stays crypto-aware; embeddedpb stays agnostic)

- `chats.go handleAPIChats`: PUT → seal `in.Messages` (+title) before `SaveChat`;
  GET → open `c.Messages`; list → open titles (locked → title `""`, never leak
  ciphertext).
- `rag.go`: upload → seal chunk `text` + doc `name` before `AddDoc`/`ReplaceDocChunks`
  (embeddings untouched); `SearchChunks` → open `text`; docs list/get → open `name`;
  `/v1/rag/search` locked → 503 `vault_locked`.
- Chat export / agent: go through the same seams (session-unlocked for the UI;
  agent doc search only while the owner's DEK is cached).

## Rekey on password change

Settings → Account password-change success → browser derives old+new wrap keys →
`POST /api/vault/rekey {old_key,new_key}` → open with old, wrap with new. Failure
(old unknown) → alert: vault left as-is.

## UI

- Settings → **Security** card: vault status (off / on+unlocked / on+locked),
  Enable (password dialog + migration progress), Lock now, re-unlock affordance.
- Global `vault_locked` interceptor in `app.js`: modal, re-type password, unlock,
  retry the failed request.
- Documents/Chat pages: no changes beyond the interceptor (they surface 503).

## Deploy + ops

- Gateway swap (bin/) + restart; **no new key file to back up** — a `data.db`
  restore is self-contained (salt + wrapped_dek travel in the same DB).
- `docs/operations.md`: note the reset-orphaning caveat + "restore works with no
  extra files".
- `data.db` tightened to 0600 (currently world-readable) as part of the change.

## Build order

1. `internal/vault` (KDF + envelope + table + Unlock/Rekey/Enable + tests)
2. login wiring + `/api/vault` endpoints (wrapinfo/unlock/lock/enable/status/rekey)
3. chat + rag seams (seal on write, open on read, locked handling)
4. UI (Settings Security card, enable dialog + progress, interceptor modal)
5. deploy, live end-to-end verify, commit; ops doc note
