package link

import (
	"fmt"
	"strings"
	"sync"
)

// Registry: live agent connections keyed by virtual engine URL
// (http://link/<agent>:<port>). Pool members reference that URL; the
// relay path asks the registry for the connection instead of dialing.
type Registry struct {
	mu    sync.RWMutex
	conns map[string]*Conn // virtual URL -> conn
	byAg  map[string]*Conn // agent label -> latest conn
}

func NewRegistry() *Registry {
	return &Registry{conns: map[string]*Conn{}, byAg: map[string]*Conn{}}
}

// Register: attach a fresh agent connection (after hello).
// Same-label re-registration replaces the old entry.
func (r *Registry) Register(c *Conn) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.byAg[c.Agent]; old != nil {
		for _, e := range old.engines {
			url := VirtualURL(c.Agent, e.Port)
			if r.conns[url] == old {
				delete(r.conns, url)
			}
		}
	}
	r.byAg[c.Agent] = c
	ids := make([]string, 0, len(c.engines))
	for _, e := range c.engines {
		url := VirtualURL(c.Agent, e.Port)
		r.conns[url] = c
		ids = append(ids, url)
	}
	return ids
}

// Unregister: drop an agent's connections (disconnect).
func (r *Registry) Unregister(agent string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.byAg[agent]
	if c == nil {
		return
	}
	for _, e := range c.engines {
		url := VirtualURL(agent, e.Port)
		if r.conns[url] == c {
			delete(r.conns, url)
		}
	}
	delete(r.byAg, agent)
}

// Lookup: live connection for a virtual backend URL, or nil.
func (r *Registry) Lookup(virtualURL string) *Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[strings.TrimRight(virtualURL, "/")]
}

// LiveURLs: registered engine URLs.
func (r *Registry) LiveURLs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.conns))
	for u := range r.conns {
		out = append(out, u)
	}
	return out
}

// ValidEnginePath: SSRF guard — only model-serving surfaces relay.
// Gateway and agent both enforce this list.
func ValidEnginePath(p string) bool {
	for _, prefix := range []string{"/v1/", "/health", "/usage", "/slots", "/metrics"} {
		if p == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// ValidateRelay: host must be an agent-local engine address.
func ValidateRelay(host, path string) error {
	if !ValidEnginePath(path) {
		return fmt.Errorf("path %q not relayable", path)
	}
	if !strings.HasPrefix(host, "127.0.0.1:") && !strings.HasPrefix(host, "localhost:") {
		return fmt.Errorf("relay host %q must be the agent's local engine", host)
	}
	return nil
}
