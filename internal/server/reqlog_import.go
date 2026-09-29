package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/embeddedpb"
)

// Engine request-log import: NInfer engines can write every request to a
// JSONL log (--request-log-jsonl). For the time before the gateway's own
// analytics began, those logs are the only per-request record. This loads
// them into analytics:
//
//	POST /admin/analytics/import?backend=<service URL>[&replace=1]
//	body: the engine's JSONL log (optionally gzip, Content-Encoding: gzip)
//
// Only requests from before the gateway's first logged request for that
// service are imported (nothing is counted twice), and only within the
// analytics retention window. The engine doesn't know who sent a request,
// so imported rows are attributed to engineLogUser.

const engineLogUser = "(engine log)"

// RequestImporter: the analytics store's bulk-import surface.
type RequestImporter interface {
	InsertRequestsTx(recs []embeddedpb.RequestRecord) error
	EarliestRequestTS(backends []string) (time.Time, error)
	DeleteRequestsBy(user string, backends []string) (int64, error)
}

// AnalyticsRetentionDays: how long per-request analytics rows are kept.
const AnalyticsRetentionDays = 14

// engineLogRecord: the fields of a NInfer request_done event we use.
type engineLogRecord struct {
	Event   string `json:"event"`
	TS      int64  `json:"timestamp_unix_ms"`
	Request struct {
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
	} `json:"request"`
	Result struct {
		Prompt     int64  `json:"prompt_tokens"`
		Completion int64  `json:"completion_tokens"`
		CacheHit   int64  `json:"prefix_cache_hit_tokens"`
		ReusePath  string `json:"prefix_reuse_path"`
	} `json:"result"`
	Timings struct {
		TTFT    float64 `json:"ttft"`
		Prefill float64 `json:"prefill"`
		Decode  float64 `json:"decode"`
		Total   float64 `json:"total"`
	} `json:"timings_seconds"`
	Speculative struct {
		Drafted  int64 `json:"drafted_tokens"`
		Accepted int64 `json:"accepted_tokens"`
	} `json:"speculative"`
	EngineTiming struct {
		QueueWait float64 `json:"queue_wait_seconds"`
	} `json:"engine_timing"`
}

func (e engineLogRecord) toRow(backend string) embeddedpb.RequestRecord {
	kind := "chat"
	if p := e.Request.Protocol; strings.Contains(p, "completions") && !strings.Contains(p, "chat") {
		kind = "completions"
	}
	var tps float64
	if e.Timings.Decode > 0 {
		tps = float64(e.Result.Completion) / e.Timings.Decode
	}
	return embeddedpb.RequestRecord{
		TS: time.UnixMilli(e.TS), User: engineLogUser, Model: e.Request.Model, Requested: e.Request.Model,
		Backend: backend, Kind: kind, Status: 200,
		Prompt: e.Result.Prompt, Cached: e.Result.CacheHit, Output: e.Result.Completion,
		TTFTms: e.Timings.TTFT * 1000, Prefillms: e.Timings.Prefill * 1000, Decodems: e.Timings.Decode * 1000,
		Totalms: e.Timings.Total * 1000, TokPerSec: tps, ReusePath: e.Result.ReusePath,
		DraftN: e.Speculative.Drafted, DraftAccepted: e.Speculative.Accepted, QueueWaitms: e.EngineTiming.QueueWait * 1000,
	}
}

func (s *Server) handleRequestLogImport(w http.ResponseWriter, r *http.Request) {
	// Admins only: a browser session, or any API key of an admin (a script
	// runs this, so not only the UI's own key).
	if key := bearerKey(r); key != "" {
		if user, rec, ok := s.store.LookupKey(key); !ok || rec.Role == auth.RoleLink || s.store.RoleOf(user) != "admin" {
			errBody(w, http.StatusForbidden, "admin API key required")
			return
		}
	} else if !s.adminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		errBody(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	imp, ok := s.Ops().(RequestImporter)
	if !ok || imp == nil {
		errBody(w, http.StatusServiceUnavailable, "analytics store not attached")
		return
	}
	backend := strings.TrimSpace(r.URL.Query().Get("backend"))
	if backend == "" {
		errBody(w, http.StatusBadRequest, "backend=<service URL> is required, e.g. http://link/ws6000:8006")
		return
	}
	// Every URL the same service has had (direct and link) shares its identity.
	id := s.gpuIdentity(backend)
	aliases := []string{backend}
	s.mu.Lock()
	for u := range s.backends {
		if u != backend && identifyGPU(s.cfg.Hosts, u).Key == id.Key {
			aliases = append(aliases, u)
		}
	}
	hosts := s.cfg.Hosts
	s.mu.Unlock()
	for _, u := range s.linkReg.LiveURLs() {
		if u != backend && identifyGPU(hosts, u).Key == id.Key {
			aliases = append(aliases, u)
		}
	}
	// Also URLs only seen in past analytics rows (e.g. a direct URL before a
	// link cutover): the identity key is host+port, so match the port.
	if port := backendPort(backend); port != "" && id.Host != "" {
		for _, h := range hosts {
			if h.Label != id.Host {
				continue
			}
			for _, ip := range h.IPs {
				aliases = append(aliases, "http://"+ip+":"+port)
			}
			for _, l := range h.Links {
				aliases = append(aliases, "http://link/"+strings.ToLower(l)+":"+port)
			}
		}
	}
	aliases = uniqStrings(aliases)

	if r.URL.Query().Get("replace") == "1" {
		n, err := imp.DeleteRequestsBy(engineLogUser, aliases)
		if err != nil {
			errBody(w, http.StatusInternalServerError, "clearing earlier import: "+err.Error())
			return
		}
		log.Printf("request-log import: removed %d earlier imported rows for %s", n, backend)
	}
	cutoff, err := imp.EarliestRequestTS(aliases)
	if err != nil {
		errBody(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cutoff.IsZero() {
		cutoff = time.Now()
	}
	oldest := time.Now().AddDate(0, 0, -AnalyticsRetentionDays).Add(time.Hour)

	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			errBody(w, http.StatusBadRequest, "bad gzip body")
			return
		}
		defer gz.Close()
		body = gz
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 256*1024), 16<<20)
	var batch []embeddedpb.RequestRecord
	var imported, skippedAfter, skippedOld, lines int
	var first, last time.Time
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := imp.InsertRequestsTx(batch)
		batch = batch[:0]
		return err
	}
	for sc.Scan() {
		lines++
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"request_done"`)) {
			continue // server_start, throughput, request_start, …
		}
		var e engineLogRecord
		if json.Unmarshal(line, &e) != nil || e.Event != "request_done" || e.TS == 0 {
			continue
		}
		ts := time.UnixMilli(e.TS)
		switch {
		case !ts.Before(cutoff):
			skippedAfter++
			continue
		case ts.Before(oldest):
			skippedOld++
			continue
		}
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if ts.After(last) {
			last = ts
		}
		batch = append(batch, e.toRow(backend))
		imported++
		if len(batch) >= 2000 {
			if err := flush(); err != nil {
				errBody(w, http.StatusInternalServerError, "insert: "+err.Error())
				return
			}
		}
	}
	if err := sc.Err(); err != nil {
		errBody(w, http.StatusBadRequest, "reading the log: "+err.Error())
		return
	}
	if err := flush(); err != nil {
		errBody(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	log.Printf("request-log import for %s: %d requests (%s … %s), %d skipped as already logged, %d older than retention",
		backend, imported, first.Format(time.RFC3339), last.Format(time.RFC3339), skippedAfter, skippedOld)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"imported": imported, "lines": lines, "from": first, "to": last,
		"skipped_already_logged": skippedAfter, "skipped_older_than_retention": skippedOld,
		"cutoff": cutoff, "attributed_to": engineLogUser, "service": id.Label,
		"note": fmt.Sprintf("Analytics keeps request rows for %d days; imported rows age out with the rest.", AnalyticsRetentionDays),
	})
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
