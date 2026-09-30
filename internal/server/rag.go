package server

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"llmgateway/internal/embeddedpb"
)

// Personal document search (RAG). A user uploads a document; the gateway
// chunks it, embeds the chunks with the configured embedding model, and
// stores the vectors embedded (SQLite BLOB, in-process cosine). Retrieval is
// always scoped to the owning user. The "document_search" tool exposed to the
// chat (via the seed-agent sidecar) calls /v1/rag/search; this file owns the
// store interface, the ingestion, and the retrieval.

// DocStore is the document-search storage (the embedded database).
type DocStore interface {
	ListDocs(user string) ([]embeddedpb.DocMeta, error)
	GetDoc(user, id string) (*embeddedpb.DocMeta, error)
	AddDoc(user, id, name string, size int64, created int64) error
	DocCount(user string) (int, error)
	ReplaceDocChunks(user, docID string, chunks []embeddedpb.DocChunk) error
	SearchChunks(user string) ([]embeddedpb.DocChunk, error)
	DeleteDoc(user, id string) error
}

const (
	maxUploadBytes = 5 << 20 // one document (mirrors the default cfg.RAG.MaxDocBytes)
)

var docIDRE = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// acceptedDocExt: file types we can extract plain text from. PDF/DOCX and
// OCR/scanned images are follow-ups (they need a text extractor / vision
// route) and are rejected with a clear message.
var acceptedDocExt = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".rst": true,
	".csv": true, ".json": true, ".html": true, ".htm": true,
}

// ---- text extraction + chunking ----

// extractText turns an uploaded file's bytes into plain text.
func extractText(name string, b []byte) string {
	ext := lowerExt(name)
	s := strings.ToValidUTF8(string(b), " ")
	if ext == ".html" || ext == ".htm" {
		s = stripHTML(s)
	}
	s = collapseWS(s)
	return strings.TrimSpace(s)
}

func lowerExt(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i:])
}

var htmlScriptRE = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
var htmlStyleRE = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
var htmlTagRE = regexp.MustCompile(`<[^>]+>`)
var htmlEntityRE = regexp.MustCompile(`&(#\d+|#x[0-9a-f]+|\w+);`)

func stripHTML(s string) string {
	s = htmlScriptRE.ReplaceAllString(s, " ")
	s = htmlStyleRE.ReplaceAllString(s, " ")
	s = htmlTagRE.ReplaceAllString(s, " ")
	s = htmlEntityRE.ReplaceAllStringFunc(s, func(m string) string {
		switch m {
		case "&amp;":
			return "&"
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		case "&quot;":
			return `"`
		case "&#39;":
			return "'"
		case "&nbsp;":
			return " "
		default:
			return m
		}
	})
	return s
}

var spaceRE = regexp.MustCompile(`[ \t]+`)
var blankRE = regexp.MustCompile(`\n{3,}`)

func collapseWS(s string) string {
	s = spaceRE.ReplaceAllString(s, " ")
	s = blankRE.ReplaceAllString(s, "\n\n")
	return s
}

// chunkText splits text into overlapping chunks of ~chunkChars, preferring
// paragraph and sentence boundaries. Overlap keeps context across chunk seams
// so a matching sentence near a boundary still retrieves well.
func chunkText(text string, chunk, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if chunk <= 0 {
		chunk = 3200
	}
	if overlap < 0 || overlap >= chunk {
		overlap = 300
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if c := strings.TrimSpace(cur.String()); c != "" {
			out = append(out, c)
		}
		cur.Reset()
	}
	for _, p := range splitParagraphs(text) {
		// A single paragraph longer than a chunk: hard-split it, keeping an
		// overlap tail so a sentence across the seam is found on both sides.
		for len(p) > chunk {
			cut := sentenceCut(p, chunk)
			cur.WriteString(p[:cut])
			flush()
			p = strings.TrimLeft(p[cut-overlap:], " \t")
			if len(p) > chunk {
				p = p[len(p)-chunk:]
			}
		}
		if cur.Len()+len(p) > chunk && cur.Len() > 0 {
			flush()
		}
		cur.WriteString(p)
		if len(p) > 0 {
			cur.WriteString("\n")
		}
	}
	flush()
	// Drop a near-empty trailing chunk.
	if len(out) > 1 && len(strings.TrimSpace(out[len(out)-1])) < 80 {
		out = out[:len(out)-1]
	}
	return out
}

func splitParagraphs(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out []string
	for _, p := range strings.Split(text, "\n\n") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// sentenceCut finds a cut point near `target` at a sentence/line boundary.
func sentenceCut(p string, target int) int {
	if target >= len(p) {
		return len(p)
	}
	lo := target
	for i := target; i > target-400; i-- {
		switch p[i] {
		case '.', '!', '?', '\n', ';':
			return i + 1
		}
		lo = i
	}
	return lo
}

// ---- embedding backend ----

// resolveEmbedBackend picks the (backend URL, model id) to embed with: the
// configured rag.embedding_model (a pool or a discovered model), else the
// first discovered embedding backend.
func (s *Server) resolveEmbedBackend() (url, modelID string) {
	var name string
	s.mu.Lock()
	if s.cfg != nil {
		name = s.cfg.RAG.EmbeddingModel
	}
	s.mu.Unlock()
	if name != "" {
		if _, pool, ok := s.poolFor(name); ok && len(pool.Members) > 0 {
			m := pool.Members[0]
			return m.Backend, m.ModelID
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for u, info := range s.backends {
			if !info.Embeds {
				continue
			}
			for _, m := range info.Models {
				if m == name {
					return u, m
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for u, info := range s.backends {
		if info.Embeds && len(info.Models) > 0 {
			return u, info.Models[0]
		}
	}
	return "", ""
}

// embedTexts embeds a batch of texts with the configured backend and returns
// the vectors. Records usage for the embedding tokens.
func (s *Server) embedTexts(ctx context.Context, user, keyID string, texts []string) ([][]float32, error) {
	url, modelID := s.resolveEmbedBackend()
	if url == "" {
		return nil, fmt.Errorf("no embedding backend is available (set rag.embedding_model)")
	}
	batch := s.cfg.RAG.MaxEmbedBatch
	if batch <= 0 {
		batch = 32
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batch {
		end := start + batch
		if end > len(texts) {
			end = len(texts)
		}
		blob, _ := json.Marshal(map[string]any{"model": modelID, "input": texts[start:end]})
		status, body, err := s.backendRequest(http.MethodPost, url, "/v1/embeddings", blob, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("embedding backend %s: %v", url, err)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("embedding backend %s: status %d", url, status)
		}
		var resp struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("embedding backend %s: bad reply", url)
		}
		if len(resp.Data) != end-start {
			return nil, fmt.Errorf("embedding backend %s: expected %d vectors, got %d", url, end-start, len(resp.Data))
		}
		vecs := make([][]float32, len(resp.Data))
		promptTokens := 0
		for i, d := range resp.Data {
			v := make([]float32, len(d.Embedding))
			for j, f := range d.Embedding {
				v[j] = float32(f)
			}
			vecs[i] = v
			promptTokens += len(texts[start+i]) / 4
		}
		s.usage.Record(user, keyID, modelID, promptTokens, 0)
		out = append(out, vecs...)
	}
	return out, nil
}

// ---- vector codec (little-endian float32 BLOB) + similarity ----

func vecToBlob(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func blobToVec(b []byte) []float32 {
	if len(b)%4 != 0 || len(b) == 0 {
		return nil
	}
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// cosineSimilarity of two equal-length vectors (0 when lengths differ).
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// ---- ingestion ----

// ingestDoc: extract text, chunk, embed, and store a new document.
func (s *Server) ingestDoc(user, name string, size int64, body []byte) (docID string, nChunks int, err error) {
	if !acceptedDocExt[lowerExt(name)] {
		return "", 0, fmt.Errorf("unsupported file type; upload .txt .md .html .csv or .json")
	}
	text := extractText(name, body)
	if strings.TrimSpace(text) == "" {
		return "", 0, fmt.Errorf("no extractable text in this file")
	}
	parts := chunkText(text, s.cfg.RAG.ChunkChars, s.cfg.RAG.OverlapChars)
	maxChunks := s.cfg.RAG.MaxChunksPerDoc
	if maxChunks > 0 && len(parts) > maxChunks {
		return "", 0, fmt.Errorf("document is too long to index (%d chunks; limit %d)", len(parts), maxChunks)
	}
	vecs, err := s.embedTexts(context.Background(), user, "", parts)
	if err != nil {
		return "", 0, err
	}
	chunks := make([]embeddedpb.DocChunk, len(parts))
	for i, p := range parts {
		chunks[i] = embeddedpb.DocChunk{Seq: i, Text: p, Embedding: vecToBlob(vecs[i]), Dims: len(vecs[i])}
	}
	// Vault: seal the stored text + doc name. Embeddings stay PLAINTEXT —
	// they are the cosine index (the query is embedded and scored against
	// them), so only the human-readable fields are encrypted.
	if s.vault != nil {
		for i := range chunks {
			if t, err := s.vault.Seal(user, chunks[i].Text); err != nil {
				return "", 0, err
			} else {
				chunks[i].Text = t
			}
		}
	}
	sname := name
	if s.vault != nil {
		sn, err := s.vault.Seal(user, name)
		if err != nil {
			return "", 0, err
		}
		sname = sn
	}
	id := newDocID()
	if err := s.docs.ReplaceDocChunks(user, id, chunks); err != nil {
		return "", 0, err
	}
	if err := s.docs.AddDoc(user, id, sname, size, time.Now().UnixMilli()); err != nil {
		return "", 0, err
	}
	return id, len(chunks), nil
}

var docIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newDocID: an unguessable random slug (no user-controlled path components).
func newDocID() string {
	const n = 16
	raw := make([]byte, n)
	if _, err := crand.Read(raw); err != nil {
		// crypto/rand failed: fall back to a time-unique id rather than a
		// predictable one.
		return "doc" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = docIDAlphabet[int(raw[i])%len(docIDAlphabet)]
	}
	return string(b)
}

// ---- /api/documents ----

// handleAPIDocs: the user's documents.
//
//	GET    /api/documents      list (metadata + chunk counts)
//	POST   /api/documents      upload (multipart field "file")
//	DELETE /api/documents/<id> delete
func (s *Server) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	if s.docs == nil {
		errBody(w, http.StatusServiceUnavailable, "document search is unavailable")
		return
	}
	user := sess.U
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/documents"), "/")
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}

	if id == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := s.docs.ListDocs(user)
			if err != nil {
				errBody(w, http.StatusInternalServerError, "could not list documents")
				return
			}
			if s.vault != nil {
				for i := range list {
					if n, oerr := s.vault.Open(user, list[i].Name); oerr == nil {
						list[i].Name = n
					} else {
						list[i].Name = "(locked — sign in to see the name)"
					}
				}
			}
			writeJSON(map[string]any{"docs": list})
		case http.MethodPost:
			// Vault: sealing happens in ingestDoc; a locked vault can't seal
			// (we refuse to write plaintext under a live vault), so fail fast
			// with the code the UI turns into a "sign in to unlock" prompt.
			if s.vault != nil && s.vault.IsLocked(user) {
				errBodyCode(w, http.StatusServiceUnavailable, "vault_locked",
					"your documents are encrypted; sign in to unlock them")
				return
			}
			ctype := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ctype, "multipart/") {
				errBody(w, http.StatusUnsupportedMediaType, "upload as multipart/form-data with a 'file' field")
				return
			}
			if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
				errBody(w, http.StatusBadRequest, "bad multipart upload")
				return
			}
			file, hdr, err := r.FormFile("file")
			if err != nil {
				errBody(w, http.StatusBadRequest, "missing 'file' field")
				return
			}
			defer file.Close()
			if hdr.Size > int64(s.cfg.RAG.MaxDocBytes) {
				errBody(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("file is too large (limit %d bytes)", s.cfg.RAG.MaxDocBytes))
				return
			}
			body, err := io.ReadAll(io.LimitReader(file, int64(s.cfg.RAG.MaxDocBytes)+64<<10))
			if err != nil {
				errBody(w, http.StatusBadRequest, "could not read the file")
				return
			}
			if n, _ := s.docs.DocCount(user); n >= s.cfg.RAG.MaxDocsPerUser {
				errBody(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("document limit reached (%d); delete some first", s.cfg.RAG.MaxDocsPerUser))
				return
			}
			docID, nChunks, err := s.ingestDoc(user, hdr.Filename, int64(len(body)), body)
			if err != nil {
				errBody(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(map[string]any{"ok": true, "id": docID, "name": hdr.Filename, "chunks": nChunks})
		default:
			errBody(w, http.StatusMethodNotAllowed, "GET or POST")
		}
		return
	}
	if !docIDRE.MatchString(id) {
		errBody(w, http.StatusBadRequest, "bad document id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		d, err := s.docs.GetDoc(user, id)
		if err != nil {
			errBody(w, http.StatusInternalServerError, "could not load the document")
			return
		}
		if d == nil {
			errBody(w, http.StatusNotFound, "no such document")
			return
		}
		if s.vault != nil {
			if n, oerr := s.vault.Open(user, d.Name); oerr == nil {
				d.Name = n
			} else {
				d.Name = "(locked — sign in to see the name)"
			}
		}
		writeJSON(map[string]any{"doc": d})
	case http.MethodDelete:
		if err := s.docs.DeleteDoc(user, id); err != nil {
			errBody(w, http.StatusInternalServerError, "could not delete the document")
			return
		}
		writeJSON(map[string]any{"ok": true})
	default:
		errBody(w, http.StatusMethodNotAllowed, "GET or DELETE")
	}
}

// ---- /v1/rag/search ----

type ragHit struct {
	DocID string  `json:"doc_id"`
	Seq   int     `json:"seq"`
	Text  string  `json:"text"`
	Score float32 `json:"score"`
}

// handleRAGSearch: POST /v1/rag/search — the "document_search" tool's
// retrieval endpoint. Key auth (the caller's own docs).
func (s *Server) handleRAGSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errBody(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	user, keyID, ok := s.checkAuth(w, r)
	if !ok {
		return
	}
	if s.docs == nil {
		errBody(w, http.StatusServiceUnavailable, "document search is unavailable")
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

	// Vault: sealed documents can only be read while the owner's DEK is
	// cached. Fail fast (before the embedding backend call) so a locked
	// vault surfaces a clean "sign in to unlock" rather than a wasted embed.
	if s.vault != nil && s.vault.IsLocked(user) {
		errBodyCode(w, http.StatusServiceUnavailable, "vault_locked",
			"your documents are encrypted; sign in to unlock them")
		return
	}

	queryVecs, err := s.embedTexts(r.Context(), user, keyID, []string{strings.TrimSpace(in.Query)})
	if err != nil {
		errBody(w, http.StatusBadGateway, "could not embed the query: "+err.Error())
		return
	}
	q := queryVecs[0]

	chunks, err := s.docs.SearchChunks(user)
	if err != nil {
		errBody(w, http.StatusInternalServerError, "could not read documents")
		return
	}
	// Vault: chunk text (and doc names) may be sealed; a locked vault can't
	// be opened — 503 + code so the UI can prompt for a re-unlock. (Embeddings
	// are the plaintext index and are searched regardless.)
	if s.vault != nil {
		for i := range chunks {
			if t, oerr := s.vault.Open(user, chunks[i].Text); oerr != nil {
				errBodyCode(w, http.StatusServiceUnavailable, "vault_locked",
					"your documents are encrypted; sign in to unlock them")
				return
			} else {
				chunks[i].Text = t
			}
		}
	}
	hits := make([]ragHit, 0, len(chunks))
	for _, c := range chunks {
		hits = append(hits, ragHit{DocID: c.DocID, Seq: c.Seq, Text: c.Text,
			Score: cosineSimilarity(q, blobToVec(c.Embedding))})
	}
	// Best first.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Score > hits[j-1].Score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if len(hits) > in.TopK {
		hits = hits[:in.TopK]
	}
	docs, _ := s.docs.ListDocs(user)
	names := map[string]string{}
	for _, d := range docs {
		nm := d.Name
		if s.vault != nil {
			if o, oerr := s.vault.Open(user, nm); oerr == nil {
				nm = o
			} else {
				nm = "(locked — sign in to see the name)"
			}
		}
		names[d.ID] = nm
	}
	results := make([]map[string]any, 0, len(hits))
	for i, h := range hits {
		results = append(results, map[string]any{
			"rank": i + 1, "doc_id": h.DocID, "name": names[h.DocID],
			"seq": h.Seq, "score": h.Score, "text": h.Text,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"query": in.Query, "results": results})
}

// withDocsTool sets the caller's "document search" toggle on an agent request
// body (overwriting anything the client sent — only the gateway decides which
// of the user's tools are enabled).
func (s *Server) withDocsTool(body []byte, user, keyID string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	var enabled bool
	if privateCaller(user, keyID) != "" {
		enabled = s.store.PrefsOf(user).DocsEnabled
	}
	m["docs_tool"] = enabled
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
