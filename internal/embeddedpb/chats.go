package embeddedpb

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
)

// Chat history: each user's conversations from the chat page, so they
// follow the user between browsers and devices. Messages are stored as the
// chat page's own JSON; the gateway doesn't interpret them. Deleting leaves
// a tombstone (deleted=1, no messages) so other devices drop their copy.

const chatsSchema = `
CREATE TABLE IF NOT EXISTS chats (
  user      TEXT NOT NULL,
  id        TEXT NOT NULL,
  title     TEXT NOT NULL DEFAULT '',
  model     TEXT NOT NULL DEFAULT '',
  created   INTEGER NOT NULL DEFAULT 0,  -- ms since epoch (client clock)
  updated   INTEGER NOT NULL DEFAULT 0,  -- ms; last writer wins
  msg_count INTEGER NOT NULL DEFAULT 0,
  bytes     INTEGER NOT NULL DEFAULT 0,
  deleted   INTEGER NOT NULL DEFAULT 0,
  messages  TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (user, id)
);
CREATE INDEX IF NOT EXISTS chats_user_updated ON chats (user, updated);
`

// ChatMeta is a conversation without its messages (the list view).
type ChatMeta struct {
	ID       string `db:"id" json:"id"`
	Title    string `db:"title" json:"title"`
	Model    string `db:"model" json:"model"`
	Created  int64  `db:"created" json:"created"`
	Updated  int64  `db:"updated" json:"updated"`
	MsgCount int    `db:"msg_count" json:"msg_count"`
	Deleted  bool   `db:"deleted" json:"deleted,omitempty"`
}

// Chat is a full conversation.
type Chat struct {
	ChatMeta
	Messages string `db:"messages" json:"-"` // raw JSON array
	Bytes    int64  `db:"bytes" json:"-"`
}

// ErrChatStale: the stored copy is newer than the one being saved.
var ErrChatStale = errors.New("a newer copy of this conversation is already saved")

// InitChatsSchema creates the chats table (idempotent).
func (a *App) InitChatsSchema() error {
	if a.pb.DB() == nil {
		return fmt.Errorf("chats: DB not open")
	}
	_, err := a.pb.DB().NewQuery(chatsSchema).Execute()
	return err
}

// ListChats: the user's conversations changed after `since` (ms), newest
// first, tombstones included (so clients can drop deleted ones).
func (a *App) ListChats(user string, since int64) ([]ChatMeta, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("chats: DB not open")
	}
	var out []ChatMeta
	err := a.pb.DB().NewQuery(`
		SELECT id, title, model, created, updated, msg_count, deleted FROM chats
		WHERE user = {:user} AND updated > {:since} ORDER BY updated DESC`).
		Bind(dbx.Params{"user": user, "since": since}).All(&out)
	return out, err
}

// GetChat returns one conversation (nil if missing or deleted).
func (a *App) GetChat(user, id string) (*Chat, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("chats: DB not open")
	}
	var c Chat
	err := a.pb.DB().NewQuery(`
		SELECT id, title, model, created, updated, msg_count, deleted, messages, bytes FROM chats
		WHERE user = {:user} AND id = {:id}`).Bind(dbx.Params{"user": user, "id": id}).One(&c)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && c.Deleted) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ChatUsage: how many live conversations the user has, and their bytes.
func (a *App) ChatUsage(user string) (count int, bytes int64, err error) {
	if a.pb.DB() == nil {
		return 0, 0, fmt.Errorf("chats: DB not open")
	}
	var row struct {
		N int   `db:"n"`
		B int64 `db:"b"`
	}
	err = a.pb.DB().NewQuery(`SELECT COUNT(*) AS n, COALESCE(SUM(bytes), 0) AS b FROM chats WHERE user = {:user} AND deleted = 0`).
		Bind(dbx.Params{"user": user}).One(&row)
	return row.N, row.B, err
}

// SaveChat stores c unless the stored copy is newer (ErrChatStale) or was
// deleted at or after c.Updated (also ErrChatStale).
func (a *App) SaveChat(user string, c Chat) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("chats: DB not open")
	}
	res, err := a.pb.DB().NewQuery(`
		INSERT INTO chats (user, id, title, model, created, updated, msg_count, bytes, deleted, messages)
		VALUES ({:user}, {:id}, {:title}, {:model}, {:created}, {:updated}, {:count}, {:bytes}, 0, {:messages})
		ON CONFLICT(user, id) DO UPDATE SET
		  title = excluded.title, model = excluded.model, created = excluded.created,
		  updated = excluded.updated, msg_count = excluded.msg_count, bytes = excluded.bytes,
		  deleted = 0, messages = excluded.messages
		WHERE chats.updated <= excluded.updated AND NOT (chats.deleted = 1 AND chats.updated >= excluded.updated)`).
		Bind(dbx.Params{"user": user, "id": c.ID, "title": c.Title, "model": c.Model, "created": c.Created,
			"updated": c.Updated, "count": c.MsgCount, "bytes": int64(len(c.Messages)), "messages": c.Messages}).Execute()
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrChatStale
	}
	return nil
}

// DeleteChat leaves a tombstone stamped `at` (ms).
func (a *App) DeleteChat(user, id string, at int64) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("chats: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO chats (user, id, updated, deleted, messages) VALUES ({:user}, {:id}, {:at}, 1, '[]')
		ON CONFLICT(user, id) DO UPDATE SET deleted = 1, messages = '[]', bytes = 0, msg_count = 0,
		  title = '', updated = MAX(chats.updated, {:at})`).
		Bind(dbx.Params{"user": user, "id": id, "at": at}).Execute()
	return err
}
