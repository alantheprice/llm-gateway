// Package server wires the HTTP surface (SPEC §2, §4, §7, §8, §12):
// auth, throttles, discovery, pool routing with reactive failover, usage.
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmgateway/internal/analytics"
	"llmgateway/internal/embeddedpb"
	"llmgateway/internal/link"

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

	mu                   sync.Mutex
	backends             map[string]*BackendInfo // url -> info
	discoverMiss         map[string]int          // url -> consecutive failed discovery probes
	rate                 map[string][]time.Time
	probe                map[string][]time.Time
	leader               map[string]string // pool -> leader url
	usage                *UsageStore
	lastMetrics          map[string]map[string]any // url -> raw /usage payload for merge
	lastPollAt           map[string]time.Time      // url -> last metrics poll attempt
	maxCtx               map[string]ctxInfo        // url -> engine context window (max_model_len)
	lastCostRecord       atomic.Int64              // unix-nano of last poll-loop cost write
	costRecorderDisabled atomic.Bool               // tests: disable the async cost recorder
	costRecorderOff      atomic.Bool               // tests: disable the background cost recorder
	// last sane (clearly-busy) fleet throughput reading — pricing's
	// GPU-time split needs a stable pp/tg ratio; defaults are a typical
	// prefill/decode pair until the engines are seen working.
	lastGoodPP float64
	lastGoodTG float64

	peaks *PeakStore // per-backend high-water throughput (pricing basis)

	costHistory *CostHistory // per-day actual cost + value (chart)

	cacheTable *routing.CacheTable // conversation-hash → GPU (content affinity)
	dayShare   *routing.DayShare   // new-conversations-routed-today per member
	linkReg    *link.Registry      // outbound link agents (remote GPUs)
	reqLog     *analytics.Store    // per-request telemetry (async writer)

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
	UpsertGPUDaily(day, backend string, tokens, cacheHits, engineInput int64, kwh float64) error
	GPUDaily(day string) ([]embeddedpb.GPUDailyRow, error)
	ReplaceCostDay(day string, energy, overhead, capital, value float64, tokens int64) error
	QueryUsageDay(day string, dest any) error
	QueryAnalyticsHourly(days int, dest *[]embeddedpb.HourlyRow) error
	QueryAnalyticsPerGPU(days int, dest *[]embeddedpb.GPURow) error
	QueryTTFTPercentile(days int, backends []string, q float64) (float64, error)
	QueryAnalyticsPerUser(days int, dest *[]embeddedpb.UserRow) error
	QueryAnalyticsReuse(days int, dest *[]embeddedpb.ReuseRow) error
	QueryBackendUsers(days int, backends []string, dest *[]embeddedpb.BackendUserRow) error
	QueryBackendTokensDay(day string, dest *[]embeddedpb.BackendTokens) error
	QueryRequestedDay(day string, dest *[]embeddedpb.NameCount) error
	PruneOlderThan(days int) (int64, error)
}

// PBAppStore: the embedded PocketBase app surface the server needs beyond
// the HTTP client (bootstrap-time user count, direct record access).
type PBAppStore interface {
	OpsReady() bool
	InitRequestsSchema() error
	CountUsers() (int64, error)
	CreateAdminUser(username, password string) error
	EnsureSuperuserEnv(dataDir string, ident, pass string) (string, error)
	SuperuserPass() (string, bool)
} // SetEmbeddedPB attaches the embedded PB app (nil in tests that don't
// embed PocketBase).
// CloseAnalytics: flush + stop the per-request telemetry writer
// (graceful shutdown).
func (s *Server) CloseAnalytics() {
	s.reqLog.Close()
}

func (s *Server) SetEmbeddedPB(app PBAppStore) {
	s.muOps.Lock()
	s.embeddedPB = app
	s.muOps.Unlock()
	// Per-request telemetry: schema + async writer over the embedded PB.
	if app != nil {
		if err := app.InitRequestsSchema(); err != nil {
			log.Printf("analytics schema: %v", err)
		}
		if appConcrete, ok := app.(*embeddedpb.App); ok {
			s.reqLog = analytics.NewStore(appConcrete)
		}
	}
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
		streamClient: &http.Client{Timeout: 0, Transport: streamTransport()}, // streams: no overall deadline
		backends:     map[string]*BackendInfo{},
		rate:         map[string][]time.Time{},
		probe:        map[string][]time.Time{},
		leader:       map[string]string{},
		usage:        NewUsageStore(UsagePath(cfg)),
		peaks:        NewPeakStore(peaksPath(UsagePath(cfg))),
		costHistory:  NewCostHistory(CostHistoryPath(UsagePath(cfg))),
		cacheTable:   routing.NewCacheTable(2*time.Hour, 8192),
		dayShare:     routing.NewDayShare(),
		linkReg:      link.NewRegistry(),
		reqLog:       analytics.NewStore(nil),
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
	// A disconnecting agent's engines leave rotation immediately rather
	// than on the next request that fails against them.
	s.linkReg.OnDrop = func(urls []string) {
		for _, u := range urls {
			s.tracker.MarkDown(u)
		}
		log.Printf("link: %v left rotation (agent disconnected)", urls)
	}
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

// publicBaseURL: externally-reachable gateway URL for user-facing
// snippets (invites, key setup). config wins; env SPROUT_PUBLIC_URL
// second; last resort http://<hostname>:<port>.
func (s *Server) publicBaseURL() string {
	if u := strings.TrimSpace(s.cfg.Gateway.PublicBaseURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	if u := os.Getenv("SPROUT_PUBLIC_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("http://%s:%d", host, s.cfg.Gateway.Port)
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
		if user, rec, ok := s.store.LookupKey(key); ok && rec.Role != auth.RoleLink {
			// Link tokens only open agent connections (/link/agent); they
			// are not API keys.
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

// Discover: probe discovery port ranges for OpenAI-compatible backends.
// The gateway's own listen port is always skipped — otherwise a misconfig
// (gateway port inside a discovery range) makes the gateway discover and
// route to itself. Discovery is a convenience; explicit pool members need
// no scanning and are the recommended setup.
func (s *Server) Discover() {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	urls := []string{}
	own := fmt.Sprintf("http://127.0.0.1:%d", cfg.Gateway.Port)
	for _, p := range cfg.Discovery.LocalPorts {
		u := fmt.Sprintf("http://127.0.0.1:%d", p)
		if u == own {
			continue
		}
		urls = append(urls, u)
	}
	if cfg.Discovery.RemoteHost != "" {
		for _, p := range cfg.Discovery.RemotePorts {
			urls = append(urls, fmt.Sprintf("http://%s:%d", cfg.Discovery.RemoteHost, p))
		}
	}
	found := map[string]*BackendInfo{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if info := probeBackend(s.client, u, cfg); info != nil {
				mu.Lock()
				found[u] = info
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()

	// Swap in the new set. An engine that is still scanned but failed this
	// probe keeps its entry until discoveryMaxMisses probes in a row fail
	// (one slow answer must not hide a model); one no longer scanned goes
	// at once.
	scanned := map[string]bool{}
	for _, u := range urls {
		scanned[u] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discoverMiss == nil {
		s.discoverMiss = map[string]int{}
	}
	next := map[string]*BackendInfo{}
	for u, info := range found {
		next[u] = info
		delete(s.discoverMiss, u)
	}
	for u, info := range s.backends {
		if _, ok := next[u]; ok || !scanned[u] {
			continue
		}
		s.discoverMiss[u]++
		if s.discoverMiss[u] < discoveryMaxMisses {
			next[u] = info
		} else {
			log.Printf("discovery: %s stopped answering; removed", u)
			delete(s.discoverMiss, u)
		}
	}
	for u := range s.backends {
		if !scanned[u] {
			log.Printf("discovery: %s no longer scanned; removed", u)
		}
	}
	s.backends = next
}

// discoveryMaxMisses: consecutive failed probes before an engine is dropped.
const discoveryMaxMisses = 3

func probeBackend(client *http.Client, url string, cfg *config.Config) *BackendInfo {
	c := &http.Client{Timeout: 3 * time.Second}
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
	// Keep today's cost-history row fresh even when nobody views the page
	// (throttled to once a minute; MAX-merge converges through the day).
	// Tests disable this: the async write races t.TempDir cleanup.
	if !s.costRecorderDisabled.Load() && time.Since(time.Unix(0, s.lastCostRecord.Load())) > time.Minute {
		now := time.Now().UnixNano()
		if s.lastCostRecord.CompareAndSwap(s.lastCostRecord.Load(), now) {
			go func() {
				defer func() { _ = recover() }()
				s.usageCostsPayload()
			}()
		}
	}
	s.mu.Lock()
	urls := make([]string, 0, len(s.backends))
	for u := range s.backends {
		urls = append(urls, u)
	}
	s.mu.Unlock()
	// Live link engines are polled over their agent socket like any LAN
	// backend (load score, down detection, energy). They are deliberately
	// NOT in s.backends: that map feeds raw model-id resolution, and a
	// linked engine must only be reachable through the pools that name it.
	urls = append(urls, s.linkReg.LiveURLs()...)
	// Pool members are always polled, discovered or not: an unpolled
	// member has no load score (reads as idle), no down detection until a
	// request fails, and no known context window.
	s.mu.Lock()
	for _, pool := range s.cfg.ModelPools {
		for _, m := range pool.Members {
			if m.Backend != "" && !isLinkURL(m.Backend) { // live links added above
				urls = append(urls, m.Backend)
			}
		}
	}
	s.mu.Unlock()
	urls = dedupeStrings(urls)
	// A member already known to be down is re-probed at most every
	// downReprobe: PollOnce runs on every pool request, and polling an
	// unreachable host costs a full timeout per request.
	now2 := time.Now()
	s.mu.Lock()
	if s.lastPollAt == nil {
		s.lastPollAt = map[string]time.Time{}
	}
	due := urls[:0]
	for _, u := range urls {
		if s.tracker.IsDown(u) && now2.Sub(s.lastPollAt[u]) < downReprobe {
			continue
		}
		s.lastPollAt[u] = now2
		due = append(due, u)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, u := range due {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if l, raw := s.pollNinferFull(u); l != nil {
				s.tracker.Set(u, l)
				s.mu.Lock()
				s.lastMetrics[u] = raw // raw /usage payload for /backends extras
				s.mu.Unlock()
				s.recordGPUDaily(u, raw)
				s.refreshMaxContext(u)
				return
			}
			if l := s.pollVLLM(u); l != nil {
				s.tracker.Set(u, l)
				s.refreshMaxContext(u)
				return
			}
			// Both shapes failed once — retry before marking down. Under
			// load (race builds, GC, port contention) a single probe can
			// exceed the 1s timeout; a genuinely dead engine fails twice.
			time.Sleep(50 * time.Millisecond)
			if l, raw := s.pollNinferFull(u); l != nil {
				s.tracker.Set(u, l)
				s.mu.Lock()
				s.lastMetrics[u] = raw
				s.mu.Unlock()
				s.recordGPUDaily(u, raw)
				return
			}
			if l := s.pollVLLM(u); l != nil {
				s.tracker.Set(u, l)
				return
			}
			// Neither metrics shape answered: a previously healthy member
			// is down (engine stopped or host unreachable).
			wasDown := s.tracker.IsDown(u)
			s.tracker.MarkDownIfTracked(u)
			if !wasDown && s.tracker.IsDown(u) {
				log.Printf("backend %s: metrics poll failed, marking down", u)
			}
		}(u)
	}
	wg.Wait()
}

// downReprobe: minimum spacing between metrics polls of a down member.
const downReprobe = 5 * time.Second

// ctxInfo: a backend's context window and when it was read.
type ctxInfo struct {
	tokens   int
	engineID string         // model id the engine serves (first /v1/models entry)
	card     map[string]any // engine model card (nil if the engine serves none)
	at       time.Time
}

// maxContextRefresh: context windows only change when an engine restarts
// with new flags, so they are re-read rarely.
const maxContextRefresh = 10 * time.Minute

// maxContext: the engine-reported context window for a backend (0 when
// unknown — the router then treats the member as able to fit anything).
func (s *Server) maxContext(url string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxCtx[url].tokens
}

// refreshMaxContext re-reads max_model_len from the backend's /v1/models
// (directly or over its link) when the cached value is missing or old.
// Called from the poller after a successful metrics poll.
func (s *Server) refreshMaxContext(url string) {
	s.mu.Lock()
	cur, ok := s.maxCtx[url]
	s.mu.Unlock()
	if ok && time.Since(cur.at) < maxContextRefresh {
		return
	}
	models, ok := s.backendJSON(url, "/v1/models", 3*time.Second)
	if !ok {
		return
	}
	best, engineID := 0, ""
	if data, ok := models["data"].([]any); ok {
		for _, d := range data {
			if m, ok := d.(map[string]any); ok {
				if n, ok := toInt(m["max_model_len"]); ok && n > best {
					best = n
				}
				if id, _ := m["id"].(string); engineID == "" && id != "" {
					engineID = id
				}
			}
		}
	}
	// Model card (NInfer serving standard): GET /v1/models/<id> carries a
	// "model_card" object when the engine was started with --model-card.
	// Engines without cards simply leave it nil.
	var card map[string]any
	if engineID != "" {
		if one, ok := s.backendJSON(url, "/v1/models/"+engineID, 3*time.Second); ok {
			card, _ = one["model_card"].(map[string]any)
		}
	}
	s.mu.Lock()
	if s.maxCtx == nil {
		s.maxCtx = map[string]ctxInfo{}
	}
	s.maxCtx[url] = ctxInfo{tokens: best, engineID: engineID, card: card, at: time.Now()}
	s.mu.Unlock()
}

// recordGPUDaily: persist a backend's engine-reported daily tokens + kWh
// into the gpu_daily ops table (MAX-merge; engines are the source of
// truth). Called on every poll — the upsert is idempotent, so the 10s
// cadence just keeps the day's row current.
func (s *Server) recordGPUDaily(backend string, raw map[string]any) {
	ops := s.Ops()
	if ops == nil {
		return
	}
	// During shutdown the DB closes before the poller stops — skip
	// rather than spam "DB not open" every second.
	type readier interface{ OpsReady() bool }
	if rr, ok := ops.(readier); !ok || !rr.OpsReady() {
		return
	}
	tok := usageNum(raw, "energy", "today", "tokens")
	kwh := usageNum(raw, "energy", "today", "kwh")
	// Cache truth: the engine's aggregate input counters, NOT the OpenAI
	// usage block (which reports prompt_tokens_details.cached_tokens = 0
	// on this NInfer build regardless of actual reuse).
	hits := usageNum(raw, "tokens", "input", "cache_hits")
	total := usageNum(raw, "tokens", "input", "total")
	if tok == 0 && kwh == 0 && total == 0 {
		return
	}
	if err := ops.UpsertGPUDaily(time.Now().UTC().Format("2006-01-02"), backend, int64(tok), int64(hits), int64(total), kwh); err != nil {
		log.Printf("gpu_daily upsert %s: %v", backend, err)
	}
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

// pollNinferFull fetches /slots + /usage and returns both the routing Load
// and the raw /usage payload (uptime, KV windows, lane breakdown — the
// /backends view needs them; see Python's _backend_load field list).
// Works for LAN backends and link engines alike (s.backendJSON).
func (s *Server) pollNinferFull(backend string) (*routing.Load, map[string]any) {
	slots, ok := s.backendJSON(backend, "/slots", 3*time.Second)
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
	usage, ok := s.backendJSON(backend, "/usage", 3*time.Second)
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

func (s *Server) pollVLLM(backend string) *routing.Load {
	status, text, err := s.backendGet(backend, "/metrics", 3*time.Second)
	if err != nil || status != 200 {
		return nil
	}
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
		MaxSeqs: s.cfg.MaxSeqsFor(backend), LastUpdated: time.Now(),
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

// preferredModel: the model a new user should default to — the pool name
// (virtual id) if pools exist, else the first public model, else the
// first discovered model id.
func (s *Server) preferredModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.cfg.ModelPools {
		return name
	}
	for _, info := range s.backends {
		if len(info.Models) > 0 {
			return info.Models[0]
		}
	}
	return ""
}

// hostByIP: the cost-config host owning this backend IP, if any.
func (s *Server) hostByIP(ip string) (config.HostCfg, bool) {
	for _, h := range s.cfg.Hosts {
		for _, hip := range h.HostKeys() {
			if hip == ip {
				return h, true
			}
		}
	}
	return config.HostCfg{}, false
}

// streamTransport: the default transport with a short dial timeout. Streams
// keep no overall deadline, but connecting to a dead LAN host must fail in
// seconds (the default dialer waits 30s) so failover stays fast.
func streamTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return t
}

// dedupeStrings keeps the first occurrence of each string, in order.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// affinityPath: the cache-affinity table's file, next to usage.json.
func (s *Server) affinityPath() string {
	return filepath.Join(filepath.Dir(UsagePath(s.cfg)), "cache_affinity.json")
}

// LoadAffinity restores the cache-affinity table at startup.
func (s *Server) LoadAffinity() {
	if s.cacheTable == nil {
		return
	}
	n, err := s.cacheTable.Load(s.affinityPath())
	if err != nil {
		log.Printf("cache affinity: load failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("cache affinity: restored %d conversations", n)
	}
}

// SaveAffinity persists the cache-affinity table (periodically + shutdown).
func (s *Server) SaveAffinity() {
	if s.cacheTable == nil {
		return
	}
	if err := s.cacheTable.Save(s.affinityPath()); err != nil {
		log.Printf("cache affinity: save failed: %v", err)
	}
}
