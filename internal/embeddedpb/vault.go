package embeddedpb

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
)

// Per-user vault keys (at-rest encryption for chats + documents). One row
// per enabled user:
//
//	salt        32B random; the BROWSER derives the wrap key from the login
//	            password as PBKDF2-SHA256(password, salt, 100_000) — the
//	            gateway never runs the KDF and never holds the password.
//	wrapped_dek  AES-256-GCM(random DEK, wrap_key) — the data key, stored
//	            wrapped so it can only be opened with the password-derived
//	            wrap key. The DEK itself lives only in process memory.
//
// A copy of the database (or of users.json + the PB password hash) cannot
// open wrapped_dek: only someone who KNOWS the password can.

const vaultSchema = `
CREATE TABLE IF NOT EXISTS vault (
  user        TEXT PRIMARY KEY,
  salt        BLOB NOT NULL,
  wrapped_dek BLOB NOT NULL,
  created     INTEGER NOT NULL DEFAULT 0
);
`

// VaultKey is one user's stored vault row.
type VaultKey struct {
	User       string `db:"user"`
	Salt       []byte `db:"salt"`
	WrappedDEK []byte `db:"wrapped_dek"`
	Created    int64  `db:"created"`
}

// InitVaultSchema creates the vault table (idempotent).
func (a *App) InitVaultSchema() error {
	if a.pb.DB() == nil {
		return fmt.Errorf("vault: DB not open")
	}
	_, err := a.pb.DB().NewQuery(vaultSchema).Execute()
	return err
}

// SaveVaultKey upserts the user's vault row.
func (a *App) SaveVaultKey(k VaultKey) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("vault: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO vault (user, salt, wrapped_dek, created)
		VALUES ({:user}, {:salt}, {:dek}, {:created})
		ON CONFLICT(user) DO UPDATE SET salt = excluded.salt,
		  wrapped_dek = excluded.wrapped_dek, created = excluded.created`).
		Bind(dbx.Params{"user": k.User, "salt": k.Salt, "dek": k.WrappedDEK, "created": k.Created}).Execute()
	return err
}

// GetVaultKey returns the user's vault row (ok=false when the user has no
// vault yet).
func (a *App) GetVaultKey(user string) (VaultKey, bool, error) {
	if a.pb.DB() == nil {
		return VaultKey{}, false, fmt.Errorf("vault: DB not open")
	}
	var k VaultKey
	err := a.pb.DB().NewQuery(`SELECT user, salt, wrapped_dek, created FROM vault WHERE user = {:user}`).
		Bind(dbx.Params{"user": user}).One(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return VaultKey{}, false, nil
	}
	if err != nil {
		return VaultKey{}, false, err
	}
	return k, true, nil
}

// DeleteVaultKey removes the user's vault row (disables the vault).
func (a *App) DeleteVaultKey(user string) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("vault: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`DELETE FROM vault WHERE user = {:user}`).
		Bind(dbx.Params{"user": user}).Execute()
	return err
}
