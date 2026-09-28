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
	// OnDrop, if set, is called (outside the lock) with the virtual URLs
	// a disconnecting or replaced agent stops serving, so routing can
	// take them out of rotation at once instead of on the next failure.
	OnDrop func(urls []string)

	mu     sync.RWMutex
	conns  map[string]*Conn  // virtual URL -> conn
	byAg   map[string]*Conn  // agent label -> latest conn
	owners map[string]string // agent label -> owning user (sticky for the process lifetime)
}

func NewRegistry() *Registry {
	return &Registry{conns: map[string]*Conn{}, byAg: map[string]*Conn{}, owners: map[string]string{}}
}

// RegisterAs attaches an agent connection owned by owner. The first
// owner of a label keeps it: another user's agent announcing the same
// name is refused, so it cannot take over (and read) traffic routed to
// that label. The same owner reconnecting replaces its old connection.
func (r *Registry) RegisterAs(c *Conn, owner string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.owners[c.Agent]; ok && prev != owner {
		return nil, fmt.Errorf("agent name %q is registered to another user", c.Agent)
	}
	r.owners[c.Agent] = owner
	return r.registerLocked(c), nil
}

// Register attaches c without an ownership check (tests only).
// Same-label re-registration replaces the old entry.
func (r *Registry) Register(c *Conn) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerLocked(c)
}

func (r *Registry) registerLocked(c *Conn) []string {
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

// Unregister: drop an agent's connection on disconnect. Only removes
// entries still pointing at THIS connection — an agent that reconnected
// (fresh ServeAgent replaced the registry entry) must not have its new
// registration torn down by the old socket's deferred cleanup.
func (r *Registry) Unregister(agent string, dead *Conn) {
	r.mu.Lock()
	if cur := r.byAg[agent]; cur != nil && cur != dead {
		r.mu.Unlock()
		return // a newer connection owns the label now
	}
	var dropped []string
	for _, e := range dead.engines {
		url := VirtualURL(agent, e.Port)
		if r.conns[url] == dead {
			delete(r.conns, url)
			dropped = append(dropped, url)
		}
	}
	if r.byAg[agent] == dead {
		delete(r.byAg, agent)
	}
	hook := r.OnDrop
	r.mu.Unlock()
	if hook != nil && len(dropped) > 0 {
		hook(dropped)
	}
}

// Conns: one entry per live agent connection (admin views).
func (r *Registry) Conns() []*Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Conn, 0, len(r.byAg))
	for _, c := range r.byAg {
		out = append(out, c)
	}
	return out
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

// DisconnectKey closes every live agent that authenticated with owner's
// link token keyID (the token was revoked). Returns how many were closed.
func (r *Registry) DisconnectKey(owner, keyID string) int {
	r.mu.RLock()
	var victims []*Conn
	for _, c := range r.byAg {
		if c.Owner == owner && c.KeyID == keyID {
			victims = append(victims, c)
		}
	}
	r.mu.RUnlock()
	for _, c := range victims {
		c.Close()
	}
	return len(victims)
}
