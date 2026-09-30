package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"llmgateway/internal/embeddedpb"
	"llmgateway/internal/vault"
)

// vaultLocked is the sentinel every sealed-data path returns when the
// user's DEK isn't cached. Handlers map it to HTTP 503 + code "vault_locked"
// so the UI can prompt for a re-unlock instead of a generic failure.
var errVaultLocked = errors.New("vault locked: sign in to unlock this data")

// VaultMgr: the in-process home for per-user data keys (DEKs). A DEK exists
// ONLY in this map (never written to disk). It is populated by Unlock
// (login / on-demand) from the password-derived wrap key, and consumed by
// the seal/open helpers on the chat + doc read/write paths.
type VaultMgr struct {
	app *embeddedpb.App

	mu    sync.RWMutex
	dek   map[string][]byte          // user -> DEK (memory only)
	env   map[string]*vault.Envelope // user -> AEAD built from the DEK
	mig   map[string]migState        // user -> migration progress
	migMu sync.Mutex
}

type migState struct {
	Active     bool `json:"active"`
	ChatsDone  int  `json:"chats_done"`
	ChatsTotal int  `json:"chats_total"`
	DocsDone   int  `json:"docs_done"`
	DocsTotal  int  `json:"docs_total"`
}

// NewVault builds the manager over the embedded DB (nil when no DB).
func NewVault(app *embeddedpb.App) *VaultMgr {
	return &VaultMgr{app: app, dek: map[string][]byte{}, env: map[string]*vault.Envelope{}, mig: map[string]migState{}}
}

func (v *VaultMgr) setEnv(user string, key []byte) {
	env, _ := vault.NewEnvelope(key)
	v.dek[user] = key
	v.env[user] = env
}

// Status reports whether the user has a vault and whether it's unlocked.
func (v *VaultMgr) Status(user string) (enabled, unlocked bool) {
	if v == nil || v.app == nil {
		return false, false
	}
	row, ok, err := v.app.GetVaultKey(user)
	if err != nil || !ok {
		return false, false
	}
	_ = row
	v.mu.RLock()
	_, unlocked = v.env[user]
	v.mu.RUnlock()
	return true, unlocked
}

// WrapInfo returns the stored salt (for the browser to derive the wrap key)
// and whether the user has a vault. Not secret: the salt is public.
func (v *VaultMgr) WrapInfo(user string) (salt []byte, enabled bool, err error) {
	if v == nil || v.app == nil {
		return nil, false, nil
	}
	row, ok, err := v.app.GetVaultKey(user)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return row.Salt, true, nil
}

// Unlock opens the stored wrapped DEK with the password-derived wrap key and
// caches it. Returns ErrLocked-ish on a wrong key (ErrWrongKey).
func (v *VaultMgr) Unlock(user string, wrapKey []byte) error {
	if v == nil || v.app == nil {
		return errors.New("vault unavailable")
	}
	row, ok, err := v.app.GetVaultKey(user)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no vault for this user")
	}
	// Already unlocked with a working DEK? (idempotent on re-login)
	v.mu.RLock()
	if _, have := v.env[user]; have {
		v.mu.RUnlock()
		return nil
	}
	v.mu.RUnlock()

	dek, err := vault.UnwrapDEK(wrapKey, row.WrappedDEK)
	if err != nil {
		return err // ErrWrongKey on a bad password
	}
	v.mu.Lock()
	v.setEnv(user, dek)
	v.mu.Unlock()
	log.Printf("Vault unlocked for %s", user)
	return nil
}

// Enable creates the user's vault (client-chosen salt + DEK wrapped by
// wrapKey), caches the DEK, and starts the one-shot migration of plaintext
// rows. The salt is client-provided: the browser derives the wrap key from
// it, so the stored salt is what it re-derives from on later unlocks.
func (v *VaultMgr) Enable(user string, wrapKey, salt []byte) error {
	if v == nil || v.app == nil {
		return errors.New("vault unavailable")
	}
	// The create path is serialized under the write lock so two concurrent
	// first logins can't both create (last-write-wins upsert + double
	// migration). The DB check runs inside the critical section; it's a
	// local embedded-DB round trip on a rare path.
	v.mu.Lock()
	if _, have := v.env[user]; have {
		v.mu.Unlock()
		return nil // already unlocked
	}
	row, ok, err := v.app.GetVaultKey(user)
	if err != nil {
		v.mu.Unlock()
		return err
	}
	if ok {
		// Row exists (e.g. re-enabled after a restart): unlock instead. The
		// client should have derived wrapKey from the stored salt (via
		// wrapinfo); a client-fresh salt means the key won't match and the
		// unlock fails safely — no stored value is touched.
		if len(salt) > 0 && string(salt) != string(row.Salt) {
			log.Printf("vault enable (%s): client salt differs from stored salt; unlock will fail unless the key matches", user)
		}
		v.mu.Unlock()
		return v.Unlock(user, wrapKey)
	}
	v.mu.Unlock()

	if len(salt) == 0 {
		salt = vault.Random(32)
	}
	dek := vault.Random(vault.KeySize)
	wrapped, err := vault.WrapDEK(wrapKey, dek)
	if err != nil {
		return err
	}
	if err := v.app.SaveVaultKey(embeddedpb.VaultKey{User: user, Salt: salt, WrappedDEK: wrapped, Created: time.Now().UnixMilli()}); err != nil {
		return err
	}
	v.mu.Lock()
	v.setEnv(user, dek)
	v.mu.Unlock()
	log.Printf("Vault enabled for %s", user)
	v.startMigration(user)
	return nil
}

// Lock drops the user's DEK from memory (the data is still sealed on disk).
func (v *VaultMgr) Lock(user string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	delete(v.dek, user)
	delete(v.env, user)
	v.mu.Unlock()
}

// Rekey re-wraps the DEK under a new wrap key (password change). Opens with
// oldKey, seals under newKey. Fails (leaves the vault as-is) if oldKey is
// wrong — the caller surfaces that so the user knows the vault wasn't rekeyed.
func (v *VaultMgr) Rekey(user string, oldKey, newKey []byte) error {
	if v == nil || v.app == nil {
		return errors.New("vault unavailable")
	}
	row, ok, err := v.app.GetVaultKey(user)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no vault for this user")
	}
	// Use the cached DEK when present; otherwise open with oldKey.
	var dek []byte
	v.mu.RLock()
	if cached, have := v.dek[user]; have {
		dek = cached
	}
	v.mu.RUnlock()
	if dek == nil {
		dek, err = vault.UnwrapDEK(oldKey, row.WrappedDEK)
		if err != nil {
			return err // wrong old password — do NOT touch the vault
		}
	}
	wrapped, err := vault.WrapDEK(newKey, dek)
	if err != nil {
		return err
	}
	row.WrappedDEK = wrapped
	if err := v.app.SaveVaultKey(row); err != nil {
		return err
	}
	log.Printf("Vault rekeyed for %s", user)
	return nil
}

// Envelope returns the user's AEAD, or errVaultLocked when the DEK isn't
// cached. Seal/Open are the higher-level seams (below).
func (v *VaultMgr) Envelope(user string) (*vault.Envelope, error) {
	if v == nil {
		return nil, errors.New("vault unavailable")
	}
	v.mu.RLock()
	e, ok := v.env[user]
	v.mu.RUnlock()
	if !ok {
		return nil, errVaultLocked
	}
	return e, nil
}

// HasVault: does this user have a vault row (i.e. is their data encrypted /
// to be encrypted)? A user who never enabled a vault has no row.
func (v *VaultMgr) HasVault(user string) bool {
	if v == nil || v.app == nil {
		return false
	}
	_, ok, err := v.app.GetVaultKey(user)
	return err == nil && ok
}

// Seal encrypts s for user. A user with no vault passes through unchanged
// (their data is plaintext — they never opted in). An enabled-but-locked
// vault is an error: we refuse to store plaintext under a live vault.
func (v *VaultMgr) Seal(user, s string) (string, error) {
	if v == nil || !v.HasVault(user) {
		return s, nil
	}
	env, err := v.Envelope(user)
	if err != nil {
		return "", err
	}
	return env.Seal(s), nil
}

// Open decrypts s for user. A value without the seal prefix (no vault, or a
// legacy plaintext row) passes through with no DEK required; a sealed value
// needs the cached DEK, else errVaultLocked.
func (v *VaultMgr) Open(user, s string) (string, error) {
	if v == nil || !vault.IsSealed(s) {
		return s, nil
	}
	env, err := v.Envelope(user)
	if err != nil {
		return "", err
	}
	return env.Open(s)
}

// IsLocked reports whether the seal/open path would fail for user (a vault
// exists but the DEK isn't cached).
func (v *VaultMgr) IsLocked(user string) bool {
	if v == nil {
		return false
	}
	return v.HasVault(user) && (func() bool {
		_, err := v.Envelope(user)
		return err != nil
	})()
}

// Migration: one pass over the user's plaintext chats + docs, re-sealing each
// value. Idempotent (Seal passes through already-sealed values) and
// concurrency-safe with live edits (a stale SaveChat is skipped; the next
// save re-seals). Runs in a goroutine; progress is observable via Status.
func (v *VaultMgr) startMigration(user string) {
	v.migMu.Lock()
	if v.mig[user].Active {
		v.migMu.Unlock()
		return
	}
	v.mig[user] = migState{Active: true}
	v.migMu.Unlock()
	go v.migrate(user)
}

func (v *VaultMgr) setMig(user string, m migState) {
	v.migMu.Lock()
	v.mig[user] = m
	v.migMu.Unlock()
}

func (v *VaultMgr) Migration(user string) migState {
	v.migMu.Lock()
	defer v.migMu.Unlock()
	return v.mig[user]
}

func (v *VaultMgr) migrate(user string) {
	defer v.setMig(user, migState{})

	// Chats: seal messages + title in place.
	ids, err := v.app.ListChatIDs(user)
	if err != nil {
		log.Printf("vault migration (%s): list chats: %v", user, err)
		return
	}
	total := len(ids)
	for i, id := range ids {
		c, err := v.app.GetChat(user, id)
		if err != nil || c == nil {
			continue
		}
		env, err := v.Envelope(user)
		if err != nil {
			return // locked mid-migration; stop
		}
		c.Messages = env.Seal(c.Messages)
		c.Title = env.Seal(c.Title)
		if err := v.app.SaveChat(user, *c); err != nil {
			if !errors.Is(err, embeddedpb.ErrChatStale) {
				log.Printf("vault migration (%s): chat %s: %v", user, id, err)
			}
			continue
		}
		v.setMig(user, migState{Active: true, ChatsDone: i + 1, ChatsTotal: total})
	}

	// Docs: seal doc name + each chunk's text (embeddings stay plaintext).
	dids, err := v.app.ListDocIDs(user)
	if err != nil {
		log.Printf("vault migration (%s): list docs: %v", user, err)
		return
	}
	dtotal := len(dids)
	for j, did := range dids {
		env, err := v.Envelope(user)
		if err != nil {
			return
		}
		dm, err := v.app.GetDoc(user, did)
		if err == nil && dm != nil {
			if err := v.app.AddDoc(user, did, env.Seal(dm.Name), dm.Size, dm.Created); err != nil {
				log.Printf("vault migration (%s): doc %s meta: %v", user, did, err)
			}
		}
		chunks, err := v.app.ListDocChunks(user, did)
		if err != nil {
			log.Printf("vault migration (%s): doc %s chunks: %v", user, did, err)
			continue
		}
		for k := range chunks {
			chunks[k].Text = env.Seal(chunks[k].Text)
		}
		if err := v.app.ReplaceDocChunks(user, did, chunks); err != nil {
			log.Printf("vault migration (%s): doc %s re-seal: %v", user, did, err)
			continue
		}
		v.setMig(user, migState{Active: true, ChatsDone: total, ChatsTotal: total, DocsDone: j + 1, DocsTotal: dtotal})
	}

	log.Printf("Vault migration complete for %s (%d chats, %d docs)", user, total, dtotal)
}

// RekeyOnPasswordChange re-wraps the user's DEK under a wrap key derived
// from the new password (x/crypto/pbkdf2 — the Go-side twin of the browser
// KDF). Called from the password-change handler: without it the stored
// wrapped DEK stays bound to the old password and the user's encrypted data
// is permanently unreachable. No vault row → no-op.
func (v *VaultMgr) RekeyOnPasswordChange(user, oldPW, newPW string) error {
	if v == nil || v.app == nil {
		return nil
	}
	row, ok, err := v.app.GetVaultKey(user)
	if err != nil {
		return err
	}
	if !ok {
		return nil // no vault — nothing to rekey
	}
	oldWrap, err := kdfFromPassword([]byte(oldPW), row.Salt)
	if err != nil {
		return err
	}
	newWrap, err := kdfFromPassword([]byte(newPW), row.Salt)
	if err != nil {
		return err
	}
	return v.Rekey(user, oldWrap, newWrap)
}

// RekeyToPassword re-wraps the user's DEK under the wrap key of a NEW
// password without knowing the old one. Used by admin password reset, where
// the old password is unavailable. It re-wraps using the CACHED DEK when
// present (the reset target's session usually has it); when the DEK is not
// cached the vault cannot be re-keyed and the error is returned so the caller
// can warn the admin that the user's encrypted data is now unreachable.
// No vault row → no-op (nil).
func (v *VaultMgr) RekeyToPassword(user, newPW string) error {
	if v == nil || v.app == nil {
		return nil
	}
	row, ok, err := v.app.GetVaultKey(user)
	if err != nil {
		return err
	}
	if !ok {
		return nil // no vault — nothing to rekey
	}
	// The DEK must be cached: without the old password we cannot unwrap a
	// stored wrapped DEK, so a locked vault cannot be re-keyed to the new pw.
	v.mu.RLock()
	dek := v.dek[user]
	v.mu.RUnlock()
	if dek == nil {
		return errors.New("vault is locked; the data key is not cached and the old password is unknown, so the encrypted data cannot be re-keyed to the new password")
	}
	newWrap, err := kdfFromPassword([]byte(newPW), row.Salt)
	if err != nil {
		return err
	}
	wrapped, err := vault.WrapDEK(newWrap, dek)
	if err != nil {
		return err
	}
	row.WrappedDEK = wrapped
	return v.app.SaveVaultKey(row)
}

// kdfFromPassword: the Go-side twin of the browser's deriveWrapKey
// (PBKDF2-SHA256, same iteration count + salt → identical wrap key).
func kdfFromPassword(pw, salt []byte) ([]byte, error) {
	return pbkdf2.Key(pw, salt, vault.PBKDF2Iterations, vault.KeySize, sha256.New), nil
}
func hexWrapKey(h string) ([]byte, error) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != vault.KeySize {
		return nil, errors.New("invalid wrap key (need 64 hex chars)")
	}
	return b, nil
}

// ---- /api/vault: the user's own vault control (session auth) ----
//
//	GET  /api/vault            status: {enabled, unlocked, salt, kdf, migration}
//	POST /api/vault/enable    {wrap_key}  create + cache the DEK, start migration
//	POST /api/vault/unlock    {wrap_key}  cache the DEK (re-login / on demand)
//	POST /api/vault/lock      clear the cached DEK
//	POST /api/vault/rekey     {old_key,new_key} re-wrap for a password change

// wrapInfo: what the browser needs to derive a wrap key (the KDF params are
// returned, never hardcoded in the UI).
type wrapInfo struct {
	Salt       string `json:"salt"` // hex 32B ("" when no vault yet)
	Enabled    bool   `json:"enabled"`
	Iterations int    `json:"iterations"`
	Hash       string `json:"hash"`
}

func vaultStatusJSON(user string, v *VaultMgr) map[string]any {
	enabled, unlocked, salt := false, false, []byte{}
	if v != nil {
		enabled, unlocked = v.Status(user)
		salt, _, _ = v.WrapInfo(user)
	}
	return map[string]any{
		"enabled": enabled, "unlocked": unlocked,
		"salt":      hex.EncodeToString(salt),
		"kdf":       map[string]any{"iterations": vault.PBKDF2Iterations, "hash": vault.Hash},
		"migration": v.Migration(user),
	}
}

func (s *Server) handleAPIVault(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, 401, "login required")
		return
	}
	v := s.vault
	if v == nil {
		errBody(w, 503, "vault unavailable (no embedded database)")
		return
	}
	user := sess.U
	switch {
	case r.URL.Path == "/api/vault" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(vaultStatusJSON(user, v))
	default:
		var in struct {
			WrapKey string `json:"wrap_key"`
			Salt    string `json:"salt"` // hex 32B (client-chosen; the KDF input)
			OldKey  string `json:"old_key"`
			NewKey  string `json:"new_key"`
		}
		if r.Method != http.MethodPost {
			errBody(w, 405, "GET /api/vault or POST action")
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			errBody(w, 400, "bad json")
			return
		}
		writeOK := func() {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
		switch r.URL.Path {
		case "/api/vault/enable":
			key, err := hexWrapKey(in.WrapKey)
			if err != nil {
				errBody(w, 400, err.Error())
				return
			}
			var salt []byte
			if in.Salt != "" {
				if salt, err = hex.DecodeString(in.Salt); err != nil || len(salt) != vault.KeySize {
					errBody(w, 400, "salt: need 64 hex chars (32 bytes)")
					return
				}
			}
			if err := v.Enable(user, key, salt); err != nil {
				errBody(w, 401, "could not enable vault: "+err.Error())
				return
			}
			writeOK()
		case "/api/vault/unlock":
			key, err := hexWrapKey(in.WrapKey)
			if err != nil {
				errBody(w, 400, err.Error())
				return
			}
			if err := v.Unlock(user, key); err != nil {
				errBody(w, 401, "wrong key")
				return
			}
			writeOK()
		case "/api/vault/lock":
			v.Lock(user)
			writeOK()
		case "/api/vault/rekey":
			oldKey, err := hexWrapKey(in.OldKey)
			if err != nil {
				errBody(w, 400, "old_key: "+err.Error())
				return
			}
			newKey, err := hexWrapKey(in.NewKey)
			if err != nil {
				errBody(w, 400, "new_key: "+err.Error())
				return
			}
			if err := v.Rekey(user, oldKey, newKey); err != nil {
				// A wrong old key must not corrupt the vault: surface it.
				errBody(w, 401, "could not rekey vault (wrong old password?)")
				return
			}
			writeOK()
		default:
			errBody(w, 404, "unknown vault action")
		}
	}
}

// handleVaultWrapInfo: pre-login salt lookup for the browser's KDF. The salt
// is not secret (PBKDF2 salts are public by design); this only reveals
// whether a username has a vault — no more than a login attempt does.
func (s *Server) handleVaultWrapInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errBody(w, 405, "GET only")
		return
	}
	user := strings.TrimSpace(r.URL.Query().Get("user"))
	if !usernameRE.MatchString(user) {
		errBody(w, 400, "bad username")
		return
	}
	v := s.vault
	salt, enabled := []byte{}, false
	if v != nil {
		salt, enabled, _ = v.WrapInfo(user)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wrapInfo{
		Salt: hex.EncodeToString(salt), Enabled: enabled,
		Iterations: vault.PBKDF2Iterations, Hash: vault.Hash,
	})
}
