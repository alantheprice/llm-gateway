package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"llmgateway/internal/embeddedpb"
)

// Personal memories: short, user-owned facts the chat can save and recall.
// A "memory tool" sits alongside the document_search and web tools in the
// agent loop: the model calls memory_save to persist a fact worth keeping
// (preferences, decisions, project context) and memory_search to recall the
// relevant ones. Retrieval is in-process cosine over the caller's own
// memories — the same embed-and-score path as document RAG, but over a
// small set of single-fact passages instead of chunked documents.
//
// This file owns the store interface, the write path (embed + seal + store),
// the session CRUD API (/api/memories), and the key-auth retrieval endpoints
// (/v1/memories/search, /v1/memories/save) that the seed-agent tools call
// with the caller's key.

// MemoryStore is the memory storage (the embedded database).
type MemoryStore interface {
	ListMemories(user string) ([]embeddedpb.Memory, error)
	GetMemory(user, id string) (*embeddedpb.Memory, error)
	AddMemory(user, id, text string, embedding []byte, dims int, created, updated int64) error
	UpdateMemory(user, id, text string, embedding []byte, dims int, updated int64) error
	DeleteMemory(user, id string) error
	MemoryCount(user string) (int, error)
	AllMemories(user string) ([]embeddedpb.MemoryWithVec, error)
}

const (
	maxMemoriesPerUser = 500
	maxMemoryChars     = 2000
)

var memoryIDRE = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// newMemoryID: an unguessable random slug (no user-controlled path parts).
func newMemoryID() string {
	id := newDocID() // reuse the same 16-char random slug generator
	return "m" + id
}

// cleanMemoryText normalises whitespace and enforces the length cap.
func cleanMemoryText(text string) (string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("empty memory")
	}
	if utf8.RuneCountInString(text) > maxMemoryChars {
		return "", fmt.Errorf("memory is too long (limit %d characters)", maxMemoryChars)
	}
	return text, nil
}

// saveMemory embeds + seals + stores one memory, returning its id. keyID is
// the caller's API key for usage attribution (empty on the session path).
func (s *Server) saveMemory(user, keyID, text string) (id string, err error) {
	text, err = cleanMemoryText(text)
	if err != nil {
		return "", err
	}
	vecs, err := s.embedTexts(context.Background(), user, keyID, []string{text})
	if err != nil {
		return "", err
	}
	// Vault: seal the stored text (the human-readable fact). The embedding
	// stays plaintext — it is the cosine index the query is scored against.
	stored := text
	if s.vault != nil {
		if t, serr := s.vault.Seal(user, stored); serr != nil {
			return "", serr
		} else {
			stored = t
		}
	}
	now := time.Now().UnixMilli()
	id = newMemoryID()
	if err := s.memories.AddMemory(user, id, stored, vecToBlob(vecs[0]), len(vecs[0]), now, now); err != nil {
		return "", err
	}
	return id, nil
}

// ---- /api/memories (session) ----

// handleAPIMemories: the user's own memories.
//
//	GET    /api/memories      list
//	POST   /api/memories      add  {"text": "..."}
//	DELETE /api/memories/<id> delete
func (s *Server) handleAPIMemories(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	if s.memories == nil {
		errBody(w, http.StatusServiceUnavailable, "memory storage is unavailable")
		return
	}
	user := sess.U
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/memories"), "/")
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	unlock := func() {
		// A sealed memory can only be read while the owner's DEK is cached.
		// Fail fast with the code the UI turns into a "sign in to unlock".
		errBodyCode(w, http.StatusServiceUnavailable, "vault_locked",
			"your memories are encrypted; sign in to unlock them")
	}

	if id == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := s.memories.ListMemories(user)
			if err != nil {
				errBody(w, http.StatusInternalServerError, "could not list memories")
				return
			}
			if s.vault != nil {
				for i := range list {
					if t, oerr := s.vault.Open(user, list[i].Text); oerr == nil {
						list[i].Text = t
					} else {
						list[i].Text = "(locked — sign in to see this memory)"
					}
				}
			}
			writeJSON(map[string]any{"memories": list})
		case http.MethodPost:
			if s.vault != nil && s.vault.IsLocked(user) {
				unlock()
				return
			}
			var in struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
				errBody(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			if n, _ := s.memories.MemoryCount(user); n >= maxMemoriesPerUser {
				errBody(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("memory limit reached (%d); delete some first", maxMemoriesPerUser))
				return
			}
			mid, err := s.saveMemory(user, "", in.Text)
			if err != nil {
				errBody(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(map[string]any{"ok": true, "id": mid})
		default:
			errBody(w, http.StatusMethodNotAllowed, "GET or POST")
		}
		return
	}
	if !memoryIDRE.MatchString(id) {
		errBody(w, http.StatusBadRequest, "bad memory id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		m, err := s.memories.GetMemory(user, id)
		if err != nil {
			errBody(w, http.StatusInternalServerError, "could not load the memory")
			return
		}
		if m == nil {
			errBody(w, http.StatusNotFound, "no such memory")
			return
		}
		if s.vault != nil {
			if t, oerr := s.vault.Open(user, m.Text); oerr == nil {
				m.Text = t
			} else {
				m.Text = "(locked — sign in to see this memory)"
			}
		}
		writeJSON(map[string]any{"memory": m})
	case http.MethodDelete:
		if err := s.memories.DeleteMemory(user, id); err != nil {
			errBody(w, http.StatusInternalServerError, "could not delete the memory")
			return
		}
		writeJSON(map[string]any{"ok": true})
	default:
		errBody(w, http.StatusMethodNotAllowed, "GET or DELETE")
	}
}

// ---- /v1/memories/search + /v1/memories/save (key auth, the tools) ----

// handleMemorySearch: POST /v1/memories/search — the "memory_search" tool's
// retrieval endpoint. Key auth (the caller's own memories).
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errBody(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return
	}
	if s.memories == nil {
		errBody(w, http.StatusServiceUnavailable, "memory storage is unavailable")
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var in struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Query) == "" {
		errBody(w, http.StatusBadRequest, "invalid search (need {\"query\": \"...\"})")
		return
	}
	if in.TopK <= 0 || in.TopK > 20 {
		in.TopK = 5
	}
	// Vault: a locked vault can't be opened — fail fast so the UI can prompt.
	if s.vault != nil && s.vault.IsLocked(user) {
		errBodyCode(w, http.StatusServiceUnavailable, "vault_locked",
			"your memories are encrypted; sign in to unlock them")
		return
	}
	queryVecs, err := s.embedTexts(r.Context(), user, keyID, []string{strings.TrimSpace(in.Query)})
	if err != nil {
		errBody(w, http.StatusBadGateway, "could not embed the query: "+err.Error())
		return
	}
	q := queryVecs[0]
	mems, err := s.memories.AllMemories(user)
	if err != nil {
		errBody(w, http.StatusInternalServerError, "could not read memories")
		return
	}
	type hit struct {
		m     embeddedpb.Memory
		score float32
	}
	hits := make([]hit, 0, len(mems))
	for _, m := range mems {
		text := m.Text
		if s.vault != nil {
			if t, oerr := s.vault.Open(user, text); oerr != nil {
				// A sealed memory we can't open: skip it rather than fail the
				// whole search (matches the doc path's per-chunk handling).
				continue
			} else {
				text = t
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		hits = append(hits, hit{m: embeddedpb.Memory{ID: m.ID, Text: text, Created: m.Created, Updated: m.Updated},
			score: cosineSimilarity(q, blobToVec(m.Embedding))})
	}
	// Best first.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].score > hits[j-1].score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if len(hits) > in.TopK {
		hits = hits[:in.TopK]
	}
	results := make([]map[string]any, 0, len(hits))
	for i, h := range hits {
		results = append(results, map[string]any{
			"rank": i + 1, "id": h.m.ID, "text": h.m.Text, "score": h.score,
			"updated": h.m.Updated,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"query": in.Query, "results": results})
}

// handleMemorySave: POST /v1/memories/save — the "memory_save" tool's write
// endpoint. Key auth (the caller's own memories).
func (s *Server) handleMemorySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errBody(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return
	}
	if s.memories == nil {
		errBody(w, http.StatusServiceUnavailable, "memory storage is unavailable")
		return
	}
	if s.vault != nil && s.vault.IsLocked(user) {
		errBodyCode(w, http.StatusServiceUnavailable, "vault_locked",
			"your memories are encrypted; sign in to unlock them")
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		errBody(w, http.StatusBadRequest, "invalid body (need {\"text\": \"...\"})")
		return
	}
	if n, _ := s.memories.MemoryCount(user); n >= maxMemoriesPerUser {
		errBody(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("memory limit reached (%d)", maxMemoriesPerUser))
		return
	}
	id, err := s.saveMemory(user, keyID, in.Text)
	if err != nil {
		errBody(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
}

// withMemoryTool sets the caller's "memory" toggle on an agent request body
// (overwriting anything the client sent — only the gateway decides which of
// the user's tools are enabled).
func (s *Server) withMemoryTool(body []byte, user, keyID string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	var enabled bool
	if privateCaller(user, keyID) != "" {
		enabled = s.store.PrefsOf(user).MemoryEnabled
	}
	m["memory_tool"] = enabled
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
