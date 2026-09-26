// Package config loads the gateway JSON configuration (same format as the
// Python gateway's llm_gateway.conf). See docs/SPEC.md §10.
package config

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type GatewayCfg struct {
	Port               int  `json:"port"`
	TrustLocalNetworks bool `json:"trust_local_networks"`
	// ModelsRequireAuth gates GET /v1/models. Default false: the catalog is
	// public (OpenAI-compatible servers list models unauthenticated; some
	// client UIs probe /v1/models before sending a key). Set true to gate it
	// behind key/session/LAN trust like every other /v1 surface.
	ModelsRequireAuth  bool   `json:"models_require_auth"`
	APIKeysFile        string `json:"api_keys_file"`
	InternalAPIKeyFile string `json:"internal_api_key_file"`
	UsersFile          string `json:"users_file"`
	UsageFile          string `json:"usage_file"`
	// PublicBaseURL: externally-reachable URL of this gateway (e.g.
	// https://llm.example.com). Used in invite emails / copy-paste
	// snippets shown to new users. Falls back to http://<host>:<port>.
	PublicBaseURL string `json:"public_base_url"`
}

type DiscoveryCfg struct {
	LocalPorts  []int  `json:"local_ports"`
	RemoteHost  string `json:"remote_host"`
	RemotePorts []int  `json:"remote_ports"`
}

type MetricsCfg struct {
	PollInterval      int            `json:"poll_interval"`
	StaleThreshold    int            `json:"stale_threshold"`
	DefaultMaxSeqs    int            `json:"default_max_seqs"`
	NinferLaneWeight  float64        `json:"ninfer_lane_weight"`
	NinferQueueWeight float64        `json:"ninfer_queue_weight"`
	NinferPressureWt  float64        `json:"ninfer_pressure_weight"`
	BackendMaxSeqs    map[string]int `json:"backend_max_seqs"`
}

type PoolMemberCfg struct {
	ModelID        string `json:"model_id"`
	Backend        string `json:"backend"`
	LargeContext   bool   `json:"large_context"`
	CapacityWeight int    `json:"capacity_weight"`
}

type PoolCfg struct {
	Members           []PoolMemberCfg `json:"members"`
	OverflowThreshold float64         `json:"overflow_threshold"`
	StickyBias        float64         `json:"sticky_bias"`
	LargePromptTokens int             `json:"large_prompt_tokens"`
	CapacityBias      float64         `json:"capacity_bias"`
	// CacheAffinity: route conversations to the GPU holding their KV
	// prefix (content-hash table), falling back to score-based pick.
	CacheAffinity bool `json:"cache_affinity"`
}

type OverflowPair struct {
	FallbackModelID string  `json:"fallback_model_id"`
	FallbackBackend string  `json:"fallback_backend"`
	Threshold       float64 `json:"overflow_threshold"`
}

type CacheCfg struct {
	TTL int `json:"ttl"`
}

// PriceBook: operator-set prices ($/1M tokens) for value-vs-cost
// reporting. These are YOUR numbers — the gateway never derives them.
type PriceBook struct {
	PromptUSDPerM float64 `json:"prompt_usd_per_m"` // non-cached prompt
	CachedUSDPerM float64 `json:"cached_usd_per_m"` // cached prompt tokens
	OutputUSDPerM float64 `json:"output_usd_per_m"` // generated tokens
}

// HostCfg describes one physical box (1 IP = 1 box) for full-cost
// accounting: GPU energy comes from the engines' NVML; overhead watts and
// capex amortization are declared here (SPEC §11).
type HostCfg struct {
	Label           string   `json:"label"`
	IPs             []string `json:"ips"`               // backend URL hosts on this box
	OverheadWatts   float64  `json:"overhead_watts"`    // CPU/RAM/fans/PSU, GPU excluded
	HardwareCostUSD float64  `json:"hardware_cost_usd"` // original purchase price
	Purchased       string   `json:"purchased"`         // ISO date
	AmortizeYears   float64  `json:"amortize_years"`    // straight-line term
	GPUIdleWatts    float64  `json:"gpu_idle_watts"`    // per-GPU idle draw; 0 → 40 (fixed-cost layer)
}

type Config struct {
	Gateway       GatewayCfg              `json:"gateway"`
	Discovery     DiscoveryCfg            `json:"discovery"`
	LocalNetworks []string                `json:"local_networks"`
	Metrics       MetricsCfg              `json:"metrics"`
	ModelPools    map[string]PoolCfg      `json:"model_pools"`
	OverflowPairs map[string]OverflowPair `json:"overflow_pairs"`
	PublicModels  []string                `json:"public_models"`
	Cache         CacheCfg                `json:"cache"`
	// Full-cost accounting (admin /usage/costs):
	ElectricityRate float64   `json:"electricity_rate_usd_per_kwh"` // 0 → 0.125
	Hosts           []HostCfg `json:"hosts"`
	// PriceBook: the operator's own per-token prices ($/1M) — what the
	// service is "worth" for value-vs-cost reporting. Not derived; you
	// set these. 0 = unpriced (value chart reads zero until set).
	PriceBook PriceBook `json:"price_book"`

	path     string
	mtime    time.Time
	mu       sync.Mutex
	onReload []func(*Config)
}

// ApplyDefaults fills zero values the way the Python gateway defaults them.
func (c *Config) ApplyDefaults() { c.applyDefaults() }

// Defaults fill zero values the way the Python gateway defaults them.
func (c *Config) applyDefaults() {
	if c.Gateway.Port == 0 {
		c.Gateway.Port = 8033
	}
	if c.Metrics.PollInterval == 0 {
		c.Metrics.PollInterval = 10
	}
	if c.Metrics.StaleThreshold == 0 {
		c.Metrics.StaleThreshold = 30
	}
	if c.Metrics.DefaultMaxSeqs == 0 {
		c.Metrics.DefaultMaxSeqs = 3
	}
	if c.Metrics.NinferLaneWeight == 0 {
		c.Metrics.NinferLaneWeight = 0.75
	}
	if c.Metrics.NinferQueueWeight == 0 {
		c.Metrics.NinferQueueWeight = 0.15
	}
	if c.Metrics.NinferPressureWt == 0 {
		c.Metrics.NinferPressureWt = 0.10
	}
	if c.Cache.TTL == 0 {
		c.Cache.TTL = 60
	}
	if c.ElectricityRate <= 0 {
		c.ElectricityRate = 0.125
	}
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	c.path = path
	if st, err := os.Stat(path); err == nil {
		c.mtime = st.ModTime()
	}
	c.applyDefaults()
	return &c, nil
}

// Save atomically persists the config to its file and updates the recorded
// mtime so the hot-reload watcher doesn't re-apply our own write. 0600 like
// the other runtime state files.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}
	if st, err := os.Stat(c.path); err == nil {
		c.mtime = st.ModTime()
	}
	return nil
}

// Path returns the config file path (admin UI write-back).
func (c *Config) Path() string { return c.path }

// Overlay deep-copies the config and merges rawJSON on top (partial-update
// semantics: fields absent from rawJSON keep their values). The copy keeps
// path/mtime so Save persists to the same file without tripping the
// hot-reload watcher.
func (c *Config) Overlay(rawJSON []byte) (*Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	blob, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	nc := &Config{}
	if err := json.Unmarshal(blob, nc); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawJSON, nc); err != nil {
		return nil, err
	}
	nc.path = c.path
	nc.mtime = c.mtime
	nc.applyDefaults()
	return nc, nil
}

// OnReload registers a callback fired after a successful hot reload.
func (c *Config) OnReload(fn func(*Config)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onReload = append(c.onReload, fn)
}

// PollWatch checks the file mtime once; returns the new config if it changed.
// (Called from a ticker in main — no goroutine in the library.)
func (c *Config) PollWatch() (*Config, bool) {
	st, err := os.Stat(c.path)
	if err != nil || !st.ModTime().After(c.mtime) {
		return nil, false
	}
	nc, err := Load(c.path)
	if err != nil {
		return nil, false // keep old config on parse error
	}
	c.mu.Lock()
	fns := append([]func(*Config){}, c.onReload...)
	c.mu.Unlock()
	for _, fn := range fns {
		fn(nc)
	}
	return nc, true
}

// MaxSeqsFor returns the configured max concurrent seqs for a backend.
func (c *Config) MaxSeqsFor(backend string) int {
	if v, ok := c.Metrics.BackendMaxSeqs[backend]; ok && v > 0 {
		return v
	}
	return c.Metrics.DefaultMaxSeqs
}
