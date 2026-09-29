package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"llmgateway/internal/embeddedpb"
)

// Chat history API for the chat page: conversations are saved per user on
// the server, so they follow the user between browsers and devices.
//
//	GET    /api/chats?since=<ms>  list (metadata, tombstones included)
//	GET    /api/chats/<id>        one conversation, with messages
//	PUT    /api/chats/<id>        save (409 when the server copy is newer)
//	DELETE /api/chats/<id>        delete (tombstone)

// ChatStore is the chat-history storage (the embedded database).
type ChatStore interface {
	ListChats(user string, since int64) ([]embeddedpb.ChatMeta, error)
	GetChat(user, id string) (*embeddedpb.Chat, error)
	SaveChat(user string, c embeddedpb.Chat) error
	DeleteChat(user, id string, at int64) error
	ChatUsage(user string) (int, int64, error)
}

const (
	maxChatBytes     = 8 << 20   // one conversation (images are data URLs)
	maxChatsPerUser  = 2000      // live conversations
	maxChatUserBytes = 512 << 20 // all of a user's conversations
)

var chatIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Server) handleAPIChats(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	store := s.chats
	if store == nil {
		errBody(w, http.StatusServiceUnavailable, "chat history is unavailable")
		return
	}
	user := sess.U
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/chats"), "/")
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}

	if id == "" {
		if r.Method != http.MethodGet {
			errBody(w, http.StatusMethodNotAllowed, "GET")
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		list, err := store.ListChats(user, since)
		if err != nil {
			errBody(w, http.StatusInternalServerError, "could not list conversations")
			return
		}
		if list == nil {
			list = []embeddedpb.ChatMeta{}
		}
		writeJSON(map[string]any{"chats": list, "now": time.Now().UnixMilli()})
		return
	}
	if !chatIDRE.MatchString(id) {
		errBody(w, http.StatusBadRequest, "bad conversation id")
		return
	}

	switch r.Method {
	case http.MethodGet:
		c, err := store.GetChat(user, id)
		if err != nil {
			errBody(w, http.StatusInternalServerError, "could not load the conversation")
			return
		}
		if c == nil {
			errBody(w, http.StatusNotFound, "no such conversation")
			return
		}
		writeJSON(map[string]any{"id": c.ID, "title": c.Title, "model": c.Model, "created": c.Created,
			"updated": c.Updated, "messages": json.RawMessage(c.Messages)})
	case http.MethodPut:
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatBytes+64<<10))
		if err != nil {
			errBody(w, http.StatusRequestEntityTooLarge, "this conversation is too large to save (8 MB limit)")
			return
		}
		var in struct {
			Title    string          `json:"title"`
			Model    string          `json:"model"`
			Created  int64           `json:"created"`
			Updated  int64           `json:"updated"`
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			errBody(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		var msgs []json.RawMessage
		if json.Unmarshal(in.Messages, &msgs) != nil {
			errBody(w, http.StatusBadRequest, "messages must be an array")
			return
		}
		if len(in.Messages) > maxChatBytes {
			errBody(w, http.StatusRequestEntityTooLarge, "this conversation is too large to save (8 MB limit)")
			return
		}
		if in.Updated <= 0 {
			in.Updated = time.Now().UnixMilli()
		}
		if n, b, err := store.ChatUsage(user); err == nil {
			existing, _ := store.GetChat(user, id)
			if existing == nil && n >= maxChatsPerUser {
				errBody(w, http.StatusRequestEntityTooLarge, "conversation limit reached; delete some old ones")
				return
			}
			if existing != nil {
				b -= existing.Bytes
			}
			if b+int64(len(in.Messages)) > maxChatUserBytes {
				errBody(w, http.StatusRequestEntityTooLarge, "chat history storage is full; delete some old conversations")
				return
			}
		}
		title := in.Title
		if len(title) > 200 {
			title = title[:200]
		}
		err = store.SaveChat(user, embeddedpb.Chat{
			ChatMeta: embeddedpb.ChatMeta{ID: id, Title: title, Model: in.Model, Created: in.Created,
				Updated: in.Updated, MsgCount: len(msgs)},
			Messages: string(in.Messages),
		})
		if errors.Is(err, embeddedpb.ErrChatStale) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "stale": true})
			return
		}
		if err != nil {
			errBody(w, http.StatusInternalServerError, "could not save the conversation")
			return
		}
		writeJSON(map[string]any{"ok": true, "updated": in.Updated})
	case http.MethodDelete:
		at := time.Now().UnixMilli()
		if v, err := strconv.ParseInt(r.URL.Query().Get("at"), 10, 64); err == nil && v > 0 {
			at = max(at, v)
		}
		if err := store.DeleteChat(user, id, at); err != nil {
			errBody(w, http.StatusInternalServerError, "could not delete the conversation")
			return
		}
		writeJSON(map[string]any{"ok": true})
	default:
		errBody(w, http.StatusMethodNotAllowed, "GET, PUT or DELETE")
	}
}
