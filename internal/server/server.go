// Package server wires the HTTP surface (SPEC §2, §4, §7, §8, §12):
// auth, throttles, discovery, pool routing with reactive failover, usage.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"llmgateway/internal/embeddedpb"

	"github.com/danielgtaylor/huma/v2"

	"llmgateway/internal/auth"
	"llmgateway/internal/config"
	"llmgateway/internal/pb"
	"llmgateway/internal/routing"
)

const (
	rateWindow    = 60 * time.Second
	rateMax       = 300
	probeMax      = 60
	sessionCookie = "llmgw_session"
)

type BackendInfo struct {
	Models []string `json:"models"`
	Engine string   `json:"engine,omitempty"`
	Chat   bool     `json:"chat"`
	Embeds bool     `json:"embeddings"`
}

type Server struct {
	cfg     *config.Config
	store   *auth.Store
	tracker *routing.Tracker
	client  *http.Client
	pb      *pb.Client

	mu          sync.Mutex
	backends    map[string]*BackendInfo // url -> info
	rate        map[string][]time.Time
	probe       map[string][]time.Time
	leader      map[string]string // pool -> leader url
	usage       *UsageStore
	lastMetrics map[string]map[string]any // url -> raw /usage payload for merge
	// last sane (clearly-busy) fleet throughput reading — pricing's
	// GPU-time split needs a stable pp/tg ratio; defaults are a typical
	// prefill/decode pair until the engines are seen working.
	lastGoodPP float64
	lastGoodTG float64

	peaks *PeakStore // per-backend high-water throughput (pricing basis)

	costHistory *CostHistory // per-day actual cost + value (chart)

	cacheTable *routing.CacheTable // conversation-hash → GPU (content affinity)

	// ops: SQLite ops tables (usage_daily, cost_history) in the embedded
	// PocketBase's database. Nil in tests that don't embed PB — every
	// call site must nil-check.
	ops   OpsStore
	muOps sync.RWMutex

	// embeddedPB: the in-process PocketBase app (bootstrap flows).
	embeddedPB PBAppStore

	huma         huma.API
	docHandler   http.Handler
	uiKeys       map[string]string // username -> plaintext ui key (session lifetime)
	agentURL     string            // seed-agent sidecar base URL
	streamClient *http.Client      // no overall timeout — SSE/long streams
}

// OpsStore is the SQLite ops surface (embeddedpb.App satisfies it).
// Interface keeps the server testable without a live PB.
type OpsStore interface {
	UpsertUsageDaily(day, user, kind string, requests, prompt, cached, output int) error
	UpsertCostDay(day string, energy, overhead, capital, value float64, tokens int64) error
	CostSeries(days int) ([]embeddedpb.CostRow, error)
	PruneOlderThan(days int) (int64, error)
}

// PBAppStore: the embedded PocketBase app surface the server needs beyond
// the HTTP client (bootstrap-time user count, direct record access).
type PBAppStore interface {
	CountUsers() (int64, error)
	CreateAdminUser(username, password string) error
	EnsureSuperuserEnv(dataDir string, ident, pass string) (string, error)
	SuperuserPass() (string, bool)
} // SetEmbeddedPB attaches the embedded PB app (nil in tests that don't
// embed PocketBase).
func (s *Server) SetEmbeddedPB(app PBAppStore) {
	s.muOps.Lock()
	s.embeddedPB = app
	s.muOps.Unlock()
}

// EmbeddedPB returns the attached app (or nil).
func (s *Server) EmbeddedPB() PBAppStore {
	s.muOps.RLock()
	defer s.muOps.RUnlock()
	return s.embeddedPB
}

// SetOps attaches the embedded PB ops store (called from main after
// bootstrap). Nil clears it.
func (s *Server) SetOps(ops OpsStore) {
	s.muOps.Lock()
	s.ops = ops
	s.muOps.Unlock()
}

// SyncUsageToOps mirrors the in-memory usage tallies (lifetime users map
// + per-day buckets + per-key breakdowns) into the SQLite ops tables.
// MAX()-merge makes it idempotent — call it every minute and at shutdown;
// restarts then lose nothing. Best-effort: errors are returned, logged by
// the caller.
func (s *Server) SyncUsageToOps() error {
	ops := s.Ops()
	if ops == nil {
		return nil
	}
	s.usage.Flush()
	for _, r := range s.usage.OpsRows() {
		if err := ops.UpsertUsageDaily(r.Day, r.User, r.Kind, r.Requests, r.Prompt, r.Cached, r.Output); err != nil {
			return err
		}
	}
	return nil
}

// Ops returns the current ops store (or nil).
func (s *Server) Ops() OpsStore {
	s.muOps.RLock()
	defer s.muOps.RUnlock()
	return s.ops
}
func New(cfg *config.Config, store *auth.Store) *Server {
	store.LegacyKeysFile = cfg.Gateway.APIKeysFile
	s := &Server{
		cfg:   cfg,
		store: store,
		tracker: routing.NewTracker(routing.Weights{
			NinferLane:     cfg.Metrics.NinferLaneWeight,
			NinferQueue:    cfg.Metrics.NinferQueueWeight,
			NinferPressure: cfg.Metrics.NinferPressureWt,
			StaleSeconds:   float64(cfg.Metrics.StaleThreshold),
		}),
		client:       &http.Client{Timeout: 5 * time.Second},
		streamClient: &http.Client{Timeout: 0}, // streams: no overall deadline
		backends:     map[string]*BackendInfo{},
		rate:         map[string][]time.Time{},
		probe:        map[string][]time.Time{},
		leader:       map[string]string{},
		usage:        NewUsageStore(UsagePath(cfg)),
		peaks:        NewPeakStore(peaksPath(UsagePath(cfg))),
		costHistory:  NewCostHistory(CostHistoryPath(UsagePath(cfg))),
		cacheTable:   routing.NewCacheTable(2*time.Hour, 8192),
		lastMetrics:  map[string]map[string]any{},
		lastGoodPP:   3000, lastGoodTG: 300,
		uiKeys: map[string]string{},
	}
	s.pb = pb.New(pbURL(cfg))
	s.pb.SetCredsRefresh(func() (string, string) {
		if app := s.EmbeddedPB(); app != nil {
			if p, ok := app.SuperuserPass(); ok {
				return pbSuperuserIdent(), p
			}
		}
		if p := pbSuperuserPass(cfg); p != "" {
			return pbSuperuserIdent(), p
		}
		return "", ""
	})
	s.pb.SetSuperuser(pbSuperuserIdent(), pbSuperuserPass(cfg))
	if a := os.Getenv("AGENT_URL"); a != "" {
		s.agentURL = a
	} else {
		s.agentURL = "http://127.0.0.1:8095"
	}
	return s
}

// pbSuperuserPass mirrors the Python gateway: PB_SUPERUSER_PASS env wins,
// else read SUPERUSER_PASS from <users_file_dir>/../pb/.superuser-env with
// identity from PB_SUPERUSER_ID (default "admin@llm.local").
func pbSuperuserPass(cfg *config.Config) string {
	if p := os.Getenv("PB_SUPERUSER_PASS"); p != "" {
		return p
	}
	// Embedded-PB layout: <PB_DATA_DIR>/.superuser-env (written by
	// `user create-admin` / install.sh).
	if dd := os.Getenv("PB_DATA_DIR"); dd != "" {
		data, err := os.ReadFile(filepath.Join(dd, ".superuser-env"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "SUPERUSER_PASS=") {
					return strings.TrimSpace(strings.TrimPrefix(line, "SUPERUSER_PASS="))
				}
			}
		}
	}
	usersPath := cfg.Gateway.UsersFile
	if usersPath == "" {
		usersPath = "users.json"
	}
	abs, err := filepath.Abs(usersPath)
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(filepath.Dir(filepath.Dir(abs)), "pb", ".superuser-env"),
		filepath.Join(filepath.Dir(abs), ".superuser-env"), // <conf dir>/.superuser-env
	}
	for _, envPath := range candidates {
		data, err := os.ReadFile(envPath)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "SUPERUSER_PASS=") {
				return strings.TrimSpace(strings.TrimPrefix(line, "SUPERUSER_PASS="))
			}
		}
	}
	return ""
}

func pbSuperuserIdent() string {
	if v := os.Getenv("PB_SUPERUSER_ID"); v != "" {
		return v
	}
	return "admin@llm.local"
}

func pbURL(cfg *config.Config) string {
	if u := os.Getenv("POCKETBASE_URL"); u != "" {
		return u
	}
	if u := os.Getenv("PB_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:8090"
}

func UsagePath(cfg *config.Config) string {
	if cfg.Gateway.UsageFile != "" {
		return cfg.Gateway.UsageFile
	}
	return "usage.json"
}

// --- networking helpers ---

var localNets []*net.IPNet

func (s *Server) ParseNetworks() {
	localNets = nil
	for _, cidr := range s.cfg.LocalNetworks {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			localNets = append(localNets, n)
		}
	}
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopback(r *http.Request) bool { return peerIP(r) == "127.0.0.1" }

// isLocalRequest: SPEC §2 — LAN trust; 127.0.0.1 is the tunnel, never local.
func isLocalRequest(r *http.Request) bool {
	if isLoopback(r) {
		return false
	}
	ip := net.ParseIP(peerIP(r))
	if ip == nil {
		return false
	}
	for _, n := range localNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP: SPEC §3 — rate-limit key only.
func clientIP(r *http.Request) string {
	if isLoopback(r) {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			return "tunnel:" + cf
		}
		return "tunnel:unknown"
	}
	return peerIP(r)
}

// --- auth ---

func bearerKey(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	// query param fallback (mirrors Python's /metrics?api_key= usage)
	if k := r.URL.Query().Get("api_key"); k != "" {
		return k
	}
	return ""
}

func (s *Server) authRequired(r *http.Request) bool {
	return !(s.cfg.Gateway.TrustLocalNetworks && isLocalRequest(r))
}

// authorized: (username, keyID, ok). Order matters:
//  1. A presented, VALID key always resolves to its user — even from
//     trusted networks (LAN trust is for UNKEYED convenience; it must not
//     swallow keyed requests or per-key usage/quota attribution breaks).
//  2. Trusted network without (or with an invalid) key → LAN trust,
//     unattributed ("local" upstream).
//  3. Untrusted + missing/invalid key → 401.
func (s *Server) authorized(r *http.Request) (string, string, bool) {
	key := bearerKey(r)
	if key != "" {
		if user, rec, ok := s.store.LookupKey(key); ok {
			return user, rec.KeyID, true
		}
		for _, lk := range s.store.LegacyKeys() {
			if lk == key {
				return "operator", "operator", true
			}
		}
	}
	if !s.authRequired(r) {
		return "", "", true
	}
	return "", "", false
}

// --- throttles (SPEC §4) ---

func sliding(m map[string][]time.Time, key string, max int, window time.Duration) bool {
	now := time.Now()
	hits := m[key]
	kept := hits[:0]
	for _, ts := range hits {
		if now.Sub(ts) < window {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= max {
		m[key] = kept
		return false
	}
	m[key] = append(kept, now)
	if len(m) > 5000 {
		for k := range m {
			delete(m, k)
			if len(m) <= 4000 {
				break
			}
		}
	}
	return true
}

func (s *Server) allow(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sliding(s.rate, clientIP(r), rateMax, rateWindow)
}

var knownV1 = regexp.MustCompile(`^(chat/completions|completions|embeddings|models|responses|messages|count_tokens)(/|$)`)

func (s *Server) allowProbe(path string, r *http.Request) bool {
	rest := strings.TrimPrefix(path, "/v1/")
	if knownV1.MatchString(rest) || rest == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := sliding(s.probe, clientIP(r), probeMax, rateWindow)
	if !ok {
		log.Printf("Unknown /v1/%s probed from %s (throttled)", rest, clientIP(r))
	} else {
		log.Printf("Unknown /v1/%s probed from %s", rest, clientIP(r))
	}
	return ok
}

func rateLimited(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"message": "rate limited", "type": "rate_limited"}})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"message": "Invalid or missing API key", "type": "unauthorized"}})
}

// --- discovery (SPEC §7) ---

func (s *Server) Discover() {
	urls := []string{}
	for _, p := range s.cfg.Discovery.LocalPorts {
		urls = append(urls, fmt.Sprintf("http://127.0.0.1:%d", p))
	}
	for _, p := range s.cfg.Discovery.RemotePorts {
		urls = append(urls, fmt.Sprintf("http://%s:%d", s.cfg.Discovery.RemoteHost, p))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			info := probeBackend(s.client, u, s.cfg)
			if info == nil {
				return
			}
			mu.Lock()
			s.backends[u] = info
			mu.Unlock()
		}(u)
	}
	wg.Wait()
}

func probeBackend(client *http.Client, url string, cfg *config.Config) *BackendInfo {
	c := &http.Client{Timeout: 1 * time.Second}
	resp, err := c.Get(url + "/v1/models")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	info := &BackendInfo{Chat: true}
	for _, m := range body.Data {
		info.Models = append(info.Models, m.ID)
		if strings.Contains(strings.ToLower(m.ID), "embed") {
			info.Embeds = true
			info.Chat = false
		}
	}
	if len(info.Models) == 0 {
		return nil
	}
	return info
}

// --- metrics polling (SPEC §6) ---

func (s *Server) PollOnce() {
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	for u := range s.backends {
		urls = append(urls, u)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if l, raw := pollNinferFull(s.client, u); l != nil {
				s.tracker.Set(u, l)
				s.mu.Lock()
				s.lastMetrics[u] = raw // raw /usage payload for /backends extras
				s.mu.Unlock()
				return
			}
			if l := pollVLLM(s.client, u, s.cfg); l != nil {
				s.tracker.Set(u, l)
			}
		}(u)
	}
	wg.Wait()
}

func getJSON(client *http.Client, url string, timeout time.Duration) (map[string]any, bool) {
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(url)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}

func pollNinfer(client *http.Client, backend string) *routing.Load {
	l, _ := pollNinferFull(client, backend)
	return l
}

// pollNinferFull fetches /slots + /usage and returns both the routing Load
// and the raw /usage payload (uptime, KV windows, lane breakdown — the
// /backends view needs them; see Python's _backend_load field list).
func pollNinferFull(client *http.Client, backend string) (*routing.Load, map[string]any) {
	slots, ok := getJSON(client, backend+"/slots", 3*time.Second)
	if !ok {
		return nil, nil
	}
	mc, _ := slots["max_concurrency"].(float64)
	if mc == 0 {
		return nil, nil // not a ninfer /slots shape
	}
	proc, _ := slots["requests_processing"].(float64)
	wait, _ := slots["requests_waiting"].(float64)
	lanes := int(mc)
	l := &routing.Load{
		Engine: "ninfer", Running: int(proc), Waiting: int(wait), Lanes: lanes,
		MaxSeqs: lanes, LastUpdated: time.Now(),
	}
	usage, ok := getJSON(client, backend+"/usage", 3*time.Second)
	if !ok {
		return l, nil
	}
	if h, ok := usage["health"].(map[string]any); ok {
		l.SpillsTotal, _ = toInt(h["kv_pressure_spills"])
		l.EvictionsTotal, _ = toInt(h["sessions_evicted"])
	}
	if thr, ok := usage["throughput"].(map[string]any); ok {
		l.DecodeTPS, _ = toF(thr["decode_tok_per_s"])
		l.PrefillTPS, _ = toF(thr["prefill_tok_per_s"])
	}
	if en, ok := usage["energy"].(map[string]any); ok {
		if today, ok := en["today"].(map[string]any); ok {
			l.EnergyKWH, _ = toF(today["kwh"])
		}
	}
	if tok, ok := usage["tokens"].(map[string]any); ok {
		if in, ok := tok["input"].(map[string]any); ok {
			l.CacheHitPct, _ = toF(in["cache_hit_rate_pct"])
		}
	}
	return l, usage
}

func pollVLLM(client *http.Client, backend string, cfg *config.Config) *routing.Load {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(backend + "/metrics")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	text, _ := io.ReadAll(resp.Body)
	vals := parsePrometheus(string(text))
	running, okR := vals["vllm:num_requests_running"]
	if !okR {
		return nil // ninfer /metrics or other — reject (SPEC §6)
	}
	waiting := vals["vllm:num_requests_waiting"]
	kv := vals["vllm:kv_cache_usage_perc"]
	l := &routing.Load{
		Engine:  "vllm",
		Running: int(running), Waiting: int(waiting), KVUsage: kv,
		MaxSeqs: cfg.MaxSeqsFor(backend), LastUpdated: time.Now(),
	}
	return l
}

func parsePrometheus(text string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		name := parts[0]
		if i := strings.Index(name, "{"); i >= 0 {
			name = name[:i]
		}
		var f float64
		if _, err := fmt.Sscanf(parts[len(parts)-1], "%g", &f); err == nil {
			out[name] = f
		}
	}
	return out
}

func toInt(v any) (int, bool) {
	if f, ok := v.(float64); ok {
		return int(f), true
	}
	return 0, false
}

func toF(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	return 0, false
}

// --- usage accounting (SPEC §8) ---
