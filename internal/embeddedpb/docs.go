package embeddedpb

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
)

// Personal document search (RAG). A user uploads documents; the gateway
// chunks + embeds them (the embedding vector is stored as a raw float32
// BLOB) and retrieves over them in-process (cosine). Everything is scoped
// to the owning user — there is no cross-user table or query.
//
// Two tables:
//   - docs:       one row per uploaded document (metadata)
//   - doc_chunks: one row per chunk (text + embedding)

const docsSchema = `
CREATE TABLE IF NOT EXISTS docs (
  user      TEXT NOT NULL,
  id        TEXT NOT NULL,
  name      TEXT NOT NULL DEFAULT '',
  size      INTEGER NOT NULL DEFAULT 0,
  created   INTEGER NOT NULL DEFAULT 0,   -- ms since epoch
  deleted   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user, id)
);
CREATE INDEX IF NOT EXISTS docs_user ON docs(user, created);

CREATE TABLE IF NOT EXISTS doc_chunks (
  user      TEXT NOT NULL,
  doc_id    TEXT NOT NULL,
  seq       INTEGER NOT NULL,
  text      TEXT NOT NULL DEFAULT '',
  embedding BLOB,                          -- float32 little-endian vector
  dims      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user, doc_id, seq)
);
CREATE INDEX IF NOT EXISTS doc_chunks_user ON doc_chunks(user);
`

// DocMeta is an uploaded document's metadata (no chunks).
type DocMeta struct {
	ID      string `db:"id" json:"id"`
	Name    string `db:"name" json:"name"`
	Size    int64  `db:"size" json:"size"`
	Created int64  `db:"created" json:"created"`
	Chunks  int    `db:"chunks" json:"chunks"`
	Deleted bool   `db:"deleted" json:"deleted,omitempty"`
}

// DocChunk is one chunk of a document with its embedding.
type DocChunk struct {
	DocID     string   `db:"doc_id"`
	Seq       int      `db:"seq"`
	Text      string   `db:"text"`
	Embedding []byte   `db:"embedding"`
	Dims      int      `db:"dims"`
}

// InitDocsSchema creates the docs + doc_chunks tables (idempotent).
func (a *App) InitDocsSchema() error {
	if a.pb.DB() == nil {
		return fmt.Errorf("docs: DB not open")
	}
	_, err := a.pb.DB().NewQuery(docsSchema).Execute()
	return err
}

// ListDocs: the user's documents, newest first (tombstones included so
// clients can drop deleted ones), with a per-doc chunk count.
func (a *App) ListDocs(user string) ([]DocMeta, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("docs: DB not open")
	}
	var out []DocMeta
	err := a.pb.DB().NewQuery(`
		SELECT d.id, d.name, d.size, d.created, d.deleted,
		       (SELECT COUNT(*) FROM doc_chunks c WHERE c.user = d.user AND c.doc_id = d.id) AS chunks
		FROM docs d
		WHERE d.user = {:user} ORDER BY d.created DESC`).
		Bind(dbx.Params{"user": user}).All(&out)
	return out, err
}

// GetDoc: one document (nil if missing or deleted).
func (a *App) GetDoc(user, id string) (*DocMeta, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("docs: DB not open")
	}
	var c DocMeta
	err := a.pb.DB().NewQuery(`SELECT id, name, size, created, deleted FROM docs
		WHERE user = {:user} AND id = {:id}`).Bind(dbx.Params{"user": user, "id": id}).One(&c)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && c.Deleted) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// AddDoc stores a document's metadata (idempotent upsert by user+id).
func (a *App) AddDoc(user, id, name string, size int64, created int64) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("docs: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO docs (user, id, name, size, created, deleted)
		VALUES ({:user}, {:id}, {:name}, {:size}, {:created}, 0)
		ON CONFLICT(user, id) DO UPDATE SET
		  name = excluded.name, size = excluded.size, created = excluded.created, deleted = 0`).
		Bind(dbx.Params{"user": user, "id": id, "name": name, "size": size, "created": created}).Execute()
	return err
}

// DocCount: how many live documents the user has.
func (a *App) DocCount(user string) (int, error) {
	if a.pb.DB() == nil {
		return 0, fmt.Errorf("docs: DB not open")
	}
	var n int
	err := a.pb.DB().NewQuery(`SELECT COUNT(*) FROM docs WHERE user = {:user} AND deleted = 0`).
		Bind(dbx.Params{"user": user}).One(&n)
	return n, err
}

// ReplaceDocChunks: delete then re-insert the chunks for one document.
// Called after re-embedding (upsert of the whole document).
func (a *App) ReplaceDocChunks(user, docID string, chunks []DocChunk) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("docs: DB not open")
	}
	if _, err := a.pb.DB().NewQuery(`DELETE FROM doc_chunks WHERE user = {:user} AND doc_id = {:doc}`).
		Bind(dbx.Params{"user": user, "doc": docID}).Execute(); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := a.pb.DB().NewQuery(`
			INSERT INTO doc_chunks (user, doc_id, seq, text, embedding, dims)
			VALUES ({:user}, {:doc}, {:seq}, {:text}, {:emb}, {:dims})`).
			Bind(dbx.Params{"user": user, "doc": docID, "seq": c.Seq, "text": c.Text,
				"emb": c.Embedding, "dims": c.Dims}).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// SearchChunks: every chunk for the user's live documents (text + embedding),
// for in-process retrieval. A user with no documents gets an empty slice.
func (a *App) SearchChunks(user string) ([]DocChunk, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("docs: DB not open")
	}
	var out []DocChunk
	err := a.pb.DB().NewQuery(`
		SELECT c.doc_id, c.seq, c.text, c.embedding, c.dims
		FROM doc_chunks c
		JOIN docs d ON d.user = c.user AND d.id = c.doc_id
		WHERE c.user = {:user} AND d.deleted = 0`).
		Bind(dbx.Params{"user": user}).All(&out)
	return out, err
}

// DeleteDoc leaves a tombstone and removes the document's chunks.
func (a *App) DeleteDoc(user, id string) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("docs: DB not open")
	}
	if _, err := a.pb.DB().NewQuery(`DELETE FROM doc_chunks WHERE user = {:user} AND doc_id = {:id}`).
		Bind(dbx.Params{"user": user, "id": id}).Execute(); err != nil {
		return err
	}
	if _, err := a.pb.DB().NewQuery(`DELETE FROM docs WHERE user = {:user} AND id = {:id}`).
		Bind(dbx.Params{"user": user, "id": id}).Execute(); err != nil {
		return err
	}
	return nil
}
