package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"llmgateway/internal/config"
)

// ---- /admin/config: admin view + edit of the gateway knobs ----
//
// GET  /admin/config  → full config JSON + file path (adminIdentity).
// POST /admin/config  → overlay the posted JSON onto the current config
//                       (absent fields keep their values), validate,
//                       atomically persist to the conf file, hot-swap.
//
// The write-back means the running process AND the conf file stay in sync —
// a restart doesn't undo what the UI changed.

func (s *Server) handleAdminConfigPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok || sess.Role != "admin" {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	s.renderPage(w, r, "admin_config.html", "admin_config", "Gateway Config")
}

func (s *Server) handleAdminConfigGet(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.adminIdentity(w, r); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": "admin only"})
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	data, err := json.Marshal(cfg)
	if err != nil {
		errBody(w, 500, err.Error())
		return
	}
	var pretty map[string]any
	if err := json.Unmarshal(data, &pretty); err != nil {
		errBody(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"config":   pretty,
		"confPath": cfg.Path(),
	})
}

func (s *Server) handleAdminConfigPost(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.adminIdentity(w, r); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": "admin only"})
		return
	}
	var posted json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&posted); err != nil {
		errBody(w, 400, "invalid JSON: "+err.Error())
		return
	}

	// Deep-copy the current config, then overlay the POST on top: fields the
	// UI didn't send keep their values (partial-update semantics). Overlay
	// runs in the config package so path/mtime survive for Save().
	s.mu.Lock()
	cur := s.cfg
	s.mu.Unlock()
	nc, err := cur.Overlay(posted)
	if err != nil {
		errBody(w, 400, err.Error())
		return
	}
	if err := validateConfig(nc); err != nil {
		errBody(w, 400, err.Error())
		return
	}
	// Keep identity-plane paths exactly as loaded — the UI doesn't edit them,
	// and pointing users_file somewhere new via a form post would be a
	// foot-gun (auth silently switches stores on restart).
	nc.Gateway.UsersFile = cur.Gateway.UsersFile
	nc.Gateway.APIKeysFile = cur.Gateway.APIKeysFile
	nc.Gateway.InternalAPIKeyFile = cur.Gateway.InternalAPIKeyFile
	nc.Gateway.UsageFile = cur.Gateway.UsageFile

	if err := nc.Save(); err != nil {
		errBody(w, 500, "persist failed: "+err.Error())
		return
	}
	s.SetConfig(nc) // swaps cfg + re-parses networks + rebuilds tracker weights

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "saved", "confPath": nc.Path()})
}

// validateConfig rejects values that would break routing or auth, clamps the
// merely-unwise ones. Returns a human-readable error for the UI flash.
func validateConfig(c *config.Config) error {
	if c.Gateway.Port < 1 || c.Gateway.Port > 65535 {
		return fmt.Errorf("gateway.port %d out of range 1-65535", c.Gateway.Port)
	}
	for i, cidr := range c.LocalNetworks {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("local_networks[%d] %q is not valid CIDR", i, cidr)
		}
	}
	for _, p := range c.Discovery.LocalPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("discovery.local_ports: %d out of range", p)
		}
	}
	for _, p := range c.Discovery.RemotePorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("discovery.remote_ports: %d out of range", p)
		}
	}
	if c.Metrics.PollInterval < 1 {
		c.Metrics.PollInterval = 1
	}
	if c.Metrics.StaleThreshold < 1 {
		c.Metrics.StaleThreshold = 1
	}
	if c.Metrics.DefaultMaxSeqs < 1 {
		c.Metrics.DefaultMaxSeqs = 1
	}
	c.Metrics.NinferLaneWeight = clamp01(c.Metrics.NinferLaneWeight)
	c.Metrics.NinferQueueWeight = clamp01(c.Metrics.NinferQueueWeight)
	c.Metrics.NinferPressureWt = clamp01(c.Metrics.NinferPressureWt)
	for url, seqs := range c.Metrics.BackendMaxSeqs {
		if seqs < 1 {
			c.Metrics.BackendMaxSeqs[url] = 1
		}
	}
	if c.Cache.TTL < 0 {
		c.Cache.TTL = 0
	}
	for name, pool := range c.ModelPools {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("model_pools: pool name cannot be empty")
		}
		if len(pool.Members) == 0 {
			return fmt.Errorf("model_pools.%s: needs at least one member", name)
		}
		for i, m := range pool.Members {
			if strings.TrimSpace(m.Backend) == "" {
				return fmt.Errorf("model_pools.%s.members[%d]: backend URL required", name, i)
			}
			u, err := url.Parse(m.Backend)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("model_pools.%s.members[%d]: %q is not a valid http(s) backend URL", name, i, m.Backend)
			}
			if m.CapacityWeight < 0 {
				pool.Members[i].CapacityWeight = 0
			}
		}
		pool.OverflowThreshold = clamp01(pool.OverflowThreshold)
		if pool.StickyBias < 0 {
			pool.StickyBias = 0
		}
		if pool.LargePromptTokens < 0 {
			pool.LargePromptTokens = 0
		}
		if pool.CapacityBias < 0 {
			pool.CapacityBias = 0
		}
		c.ModelPools[name] = pool
	}
	for model, pair := range c.OverflowPairs {
		if strings.TrimSpace(pair.FallbackBackend) == "" {
			return fmt.Errorf("overflow_pairs.%s: fallback_backend required", model)
		}
		pair.Threshold = clamp01(pair.Threshold)
		c.OverflowPairs[model] = pair
	}
	return nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
