package embeddedpb

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
)

// Personal memories: short, user-owned facts the chat agent can save and
// later recall ("remember that I prefer dark mode"). Unlike uploaded
// documents, a memory is a single short passage with its own embedding — no
// chunking. Retrieval is in-process cosine over the caller's live memories,
// scoped to the owning user (there is no cross-user table or query).
//
// One table: memories (text + embedding BLOB + timestamps). The text is the
// human-readable fact (sealed by the vault when the user has one); the
// embedding stays plaintext — it is the cosine index the query is scored
// against (same rationale as documents).

const memoriesSchema = `
CREATE TABLE IF NOT EXISTS memories (
  user      TEXT NOT NULL,
  id        TEXT NOT NULL,
  text      TEXT NOT NULL DEFAULT '',
  embedding BLOB,                          -- float32 little-endian vector
  dims      INTEGER NOT NULL DEFAULT 0,
  created   INTEGER NOT NULL DEFAULT 0,   -- ms since epoch
  updated   INTEGER NOT NULL DEFAULT 0,   -- ms since epoch
  deleted   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user, id)
);
CREATE INDEX IF NOT EXISTS memories_user ON memories(user, updated);
`

// Memory is one saved memory.
type Memory struct {
	ID      string `db:"id" json:"id"`
	Text    string `db:"text" json:"text"`
	Created int64  `db:"created" json:"created"`
	Updated int64  `db:"updated" json:"updated"`
	Deleted bool   `db:"deleted" json:"deleted,omitempty"`
}

// MemoryWithVec is a memory plus its embedding (for in-process retrieval).
type MemoryWithVec struct {
	Memory
	Embedding []byte `db:"embedding" json:"-"`
	Dims      int    `db:"dims" json:"dims"`
}

// InitMemoriesSchema creates the memories table (idempotent).
func (a *App) InitMemoriesSchema() error {
	if a.pb.DB() == nil {
		return fmt.Errorf("memories: DB not open")
	}
	_, err := a.pb.DB().NewQuery(memoriesSchema).Execute()
	return err
}

// ListMemories: the user's live memories, most recently updated first.
func (a *App) ListMemories(user string) ([]Memory, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("memories: DB not open")
	}
	var out []Memory
	err := a.pb.DB().NewQuery(`
		SELECT id, text, created, updated, deleted
		FROM memories
		WHERE user = {:user} AND deleted = 0
		ORDER BY updated DESC`).
		Bind(dbx.Params{"user": user}).All(&out)
	return out, err
}

// GetMemory: one memory (nil if missing or deleted).
func (a *App) GetMemory(user, id string) (*Memory, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("memories: DB not open")
	}
	var m Memory
	err := a.pb.DB().NewQuery(`SELECT id, text, created, updated, deleted
		FROM memories WHERE user = {:user} AND id = {:id}`).
		Bind(dbx.Params{"user": user, "id": id}).One(&m)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && m.Deleted) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// AddMemory inserts a new memory (idempotent upsert by user+id).
func (a *App) AddMemory(user, id, text string, embedding []byte, dims int, created, updated int64) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("memories: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO memories (user, id, text, embedding, dims, created, updated, deleted)
		VALUES ({:user}, {:id}, {:text}, {:emb}, {:dims}, {:created}, {:updated}, 0)
		ON CONFLICT(user, id) DO UPDATE SET
		  text = excluded.text, embedding = excluded.embedding, dims = excluded.dims,
		  updated = excluded.updated, deleted = 0`).
		Bind(dbx.Params{"user": user, "id": id, "text": text, "emb": embedding,
			"dims": dims, "created": created, "updated": updated}).Execute()
	return err
}

// UpdateMemory rewrites a memory's text + embedding (re-embed) and bumps
// updated.
func (a *App) UpdateMemory(user, id, text string, embedding []byte, dims int, updated int64) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("memories: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		UPDATE memories SET text = {:text}, embedding = {:emb}, dims = {:dims},
		  updated = {:updated}, deleted = 0
		WHERE user = {:user} AND id = {:id}`).
		Bind(dbx.Params{"user": user, "id": id, "text": text, "emb": embedding,
			"dims": dims, "updated": updated}).Execute()
	return err
}

// DeleteMemory leaves a tombstone.
func (a *App) DeleteMemory(user, id string) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("memories: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`UPDATE memories SET deleted = 1 WHERE user = {:user} AND id = {:id}`).
		Bind(dbx.Params{"user": user, "id": id}).Execute()
	return err
}

// MemoryCount: how many live memories the user has.
func (a *App) MemoryCount(user string) (int, error) {
	if a.pb.DB() == nil {
		return 0, fmt.Errorf("memories: DB not open")
	}
	var c struct {
		N int `db:"n"`
	}
	if err := a.pb.DB().NewQuery(`SELECT COUNT(*) AS n FROM memories WHERE user = {:user} AND deleted = 0`).
		Bind(dbx.Params{"user": user}).One(&c); err != nil {
		return 0, err
	}
	return c.N, nil
}

// AllMemories: every live memory for the user (text + embedding), for
// in-process retrieval. A user with no memories gets an empty slice.
func (a *App) AllMemories(user string) ([]MemoryWithVec, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("memories: DB not open")
	}
	var out []MemoryWithVec
	err := a.pb.DB().NewQuery(`
		SELECT id, text, embedding, dims, created, updated, deleted
		FROM memories
		WHERE user = {:user} AND deleted = 0`).
		Bind(dbx.Params{"user": user}).All(&out)
	return out, err
}
