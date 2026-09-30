package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

// ---------- pure unit tests: extraction + chunking ----------

func TestExtractTextPlainAndHTML(t *testing.T) {
	if got := extractText("a.txt", []byte("  hello   world\n\n\nagain ")); !strings.Contains(got, "hello") || !strings.Contains(got, "again") {
		t.Fatalf("txt extract = %q", got)
	}
	if got := extractText("a.html", []byte(`<html><head><style>.x{}</style></head><body><p>Hello &amp; world</p><script>var a=1;</script></body></html>`)); !strings.Contains(got, "Hello & world") || strings.Contains(got, "var a") || strings.Contains(got, ".x") {
		t.Fatalf("html extract = %q", got)
	}
}

func TestChunkTextOverlappingAndBounded(t *testing.T) {
	// Many paragraphs → several chunks, none wildly oversized, first chunk
	// carries the opening words.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "Paragraph %d talks about a distinct subject %d. ", i, i)
		if i%3 == 0 {
			b.WriteString("\n\n")
		} else {
			b.WriteString(" ")
		}
	}
	parts := chunkText(b.String(), 400, 60)
	if len(parts) < 3 {
		t.Fatalf("too few chunks: %d", len(parts))
	}
	for i, p := range parts {
		if len(p) > 700 {
			t.Fatalf("chunk %d is %d chars (too big)", i, len(p))
		}
	}
	if !strings.Contains(parts[0], "Paragraph 0") {
		t.Fatalf("first chunk missing the opening: %q", parts[0][:60])
	}
	// Empty / whitespace-only input yields no chunks.
	if got := chunkText("   \n  ", 400, 60); len(got) != 0 {
		t.Fatalf("empty input should yield no chunks, got %v", got)
	}
}

func TestCosineSimilarity(t *testing.T) {
	if got := cosineSimilarity([]float32{1, 0}, []float32{1, 0}); got <= 0.99 || got > 1.01 {
		t.Fatalf("identical = %v, want ~1", got)
	}
	if got := cosineSimilarity([]float32{1, 0}, []float32{0, 1}); got < -0.01 || got > 0.01 {
		t.Fatalf("orthogonal = %v, want ~0", got)
	}
	if got := cosineSimilarity([]float32{1, 0, 0}, []float32{1, 0}); got != 0 {
		t.Fatalf("length mismatch = %v, want 0", got)
	}
}

// vecRoundTrip: the float32 BLOB codec is lossless for our vectors.
func TestVecRoundTrip(t *testing.T) {
	in := []float32{0, 1, -2.5, 3e-4, 1e5}
	out := blobToVec(vecToBlob(in))
	if len(out) != len(in) {
		t.Fatalf("len %d != %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("roundtrip[%d] = %v, want %v", i, out[i], in[i])
		}
	}
	if got := blobToVec(nil); got != nil {
		t.Fatalf("nil blob → %v", got)
	}
}

// ---------- integration: upload, rank, per-user isolation ----------

// newFakeEmbedding: a deterministic bag-of-words embedder over a fixed vocab,
// so cosine ranking is stable and testable without a real embedding model.
func newFakeEmbedding(t *testing.T, vocab []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			w.WriteHeader(404)
			return
		}
		var in struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(400)
			return
		}
		vec := func(s string) []float64 {
			v := make([]float64, len(vocab))
			for _, word := range strings.Fields(strings.ToLower(s)) {
				word = strings.Trim(word, ".,!?;:\"'()")
				for i, k := range vocab {
					if word == k {
						v[i]++
					}
				}
			}
			return v
		}
		data := make([]map[string]any, 0, len(in.Input))
		for i, txt := range in.Input {
			data = append(data, map[string]any{"embedding": vec(txt), "index": i})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ragConf(embedURL string) string {
	return `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "rag":{"embedding_model":"test-embed"}}`
}

func uploadDoc(t *testing.T, s *Server, h http.Handler, user, filename, text string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write([]byte(text))
	mw.Close()
	r := httptest.NewRequest("POST", "/api/documents", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: user, Role: "user"}, time.Hour)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func ragSearch(t *testing.T, s *Server, h http.Handler, key, query string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/rag/search", strings.NewReader(fmt.Sprintf(`{"query":%q,"top_k":5}`, query)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func TestRAGUploadAndPerUserIsolation(t *testing.T) {
	app, _, _ := testPB(t)
	vocab := []string{"apple", "pie", "cinnamon", "recipe", "vllm", "gpu", "scheduling", "engine", "token", "quantization"}
	emb := newFakeEmbedding(t, vocab)

	s := testServer(t, ragConf(emb.URL), nil)
	s.backends[emb.URL] = &BackendInfo{Models: []string{"test-embed"}, Embeds: true, Chat: false}
	s.SetOps(app)
	h := s.Handler()

	danaKey, _, _ := s.store.CreateKey("dana", "dkey", "user", false)
	erinKey, _, _ := s.store.CreateKey("erin", "ekey", "user", false)

	// Both users upload a document.
	if c, m := uploadDoc(t, s, h, "dana", "recipe.txt", "An apple pie recipe with cinnamon. A classic apple pie with butter."); c != 200 {
		t.Fatalf("dana upload = %d %v", c, m)
	}
	if c, m := uploadDoc(t, s, h, "erin", "notes.txt", "vllm gpu scheduling engine token quantization notes."); c != 200 {
		t.Fatalf("erin upload = %d %v", c, m)
	}

	// Dana's search finds her own doc and never erin's.
	c, m := ragSearch(t, s, h, danaKey, "apple pie")
	if c != 200 {
		t.Fatalf("dana search = %d %v", c, m)
	}
	results := m["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("dana: no results: %v", m)
	}
	top := results[0].(map[string]any)
	if !strings.Contains(strings.ToLower(top["text"].(string)), "apple") {
		t.Fatalf("top result not dana's apple doc: %v", top)
	}
	for _, rr := range results {
		if strings.Contains(strings.ToLower(rr.(map[string]any)["text"].(string)), "vllm") {
			t.Fatalf("dana's search leaked erin's doc: %v", rr)
		}
	}

	// Erin's search finds her own doc.
	c, m = ragSearch(t, s, h, erinKey, "vllm scheduling")
	if c != 200 {
		t.Fatalf("erin search = %d %v", c, m)
	}
	results = m["results"].([]any)
	if len(results) == 0 || !strings.Contains(strings.ToLower(results[0].(map[string]any)["text"].(string)), "vllm") {
		t.Fatalf("erin: top not her doc: %v", m)
	}

	// Store-level isolation: each user's chunk set excludes the other's text.
	danaChunks, _ := app.SearchChunks("dana")
	for _, c := range danaChunks {
		if strings.Contains(strings.ToLower(c.Text), "vllm") {
			t.Fatalf("store leaked erin text to dana: %q", c.Text)
		}
	}

	// Dana's doc list shows her doc, not erin's.
	r := httptest.NewRequest("GET", "/api/documents", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "dana", Role: "user"}, time.Hour)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var dm map[string]any
	json.Unmarshal(w.Body.Bytes(), &dm)
	if docs := dm["docs"].([]any); len(docs) != 1 || docs[0].(map[string]any)["name"] != "recipe.txt" {
		t.Fatalf("dana doc list = %v", dm)
	}

	// Unsupported extension is rejected.
	if c, _ := uploadDoc(t, s, h, "dana", "malware.exe", "bad"); c != 400 {
		t.Fatalf("exe upload = %d, want 400", c)
	}

	// Delete: gone from the list and no longer searchable.
	id := dm["docs"].([]any)[0].(map[string]any)["id"].(string)
	r = httptest.NewRequest("DELETE", "/api/documents/"+id, nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "dana", Role: "user"}, time.Hour)})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
	c, m = ragSearch(t, s, h, danaKey, "apple pie")
	if c != 200 {
		t.Fatalf("search after delete = %d %v", c, m)
	}
	for _, rr := range m["results"].([]any) {
		if strings.Contains(strings.ToLower(rr.(map[string]any)["text"].(string)), "apple") {
			t.Fatalf("deleted doc still searchable: %v", rr)
		}
	}
}

// withDocsTool injects the caller's document-search toggle into an agent
// body, overwriting any value the client tried to smuggle.
func TestWithDocsToolInjection(t *testing.T) {
	s := testServer(t, `{}`, nil)
	s.store.CreateKey("carol", "k", "user", false)
	_ = s.store.SetPrefs("carol", auth.UserPrefs{DocsEnabled: true})

	// Enabled user: the tool is offered even if the client sent false.
	got := s.withDocsTool([]byte(`{"messages":[],"docs_tool":false}`), "carol", "k1")
	var m map[string]any
	json.Unmarshal(got, &m)
	if m["docs_tool"] != true {
		t.Fatalf("docs_tool = %v, want true", m["docs_tool"])
	}
	// Anonymous (no API key) caller: the tool is off even if the client
	// sent true — privateCaller("") is empty, so no user's docs are used.
	g := s.withDocsTool([]byte(`{"messages":[],"docs_tool":true}`), "carol", "")
	var m2 map[string]any
	json.Unmarshal(g, &m2)
	if m2["docs_tool"] != false {
		t.Fatalf("anonymous docs_tool = %v, want false", m2["docs_tool"])
	}
}
