package server

import (
	"net/url"
	"strings"

	"llmgateway/internal/config"
)

// gpuIdentity is the one name every page and API uses for a service (one
// engine: host + port). Several services can share a card and one can span
// several; GPUs names the cards when the host config lists them.
// The same engine can be reached several ways over time (direct LAN URL,
// then a link virtual URL after a cutover); all of them resolve to the
// same Key, so history, costs and analytics stay on one row.
type gpuIdentity struct {
	Key   string   `json:"gpu_key"`        // stable: "<host label>:<port>", or the URL when no host claims it
	Label string   `json:"gpu_label"`      // display: "gpu-b (6000 Pro) :8006"
	Host  string   `json:"host"`           // cost-config host label ("" if no host claims the backend)
	Via   string   `json:"via"`            // "link" or "direct"
	GPUs  []string `json:"gpus,omitempty"` // the cards it runs on (hosts[].gpus)
}

// backendPort: the engine port of a backend URL ("" if none).
func backendPort(backend string) string {
	if isLinkURL(backend) {
		rest := strings.TrimPrefix(backend, "http://link/")
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			return strings.TrimRight(rest[i+1:], "/")
		}
		return ""
	}
	if u, err := url.Parse(backend); err == nil {
		return u.Port()
	}
	return ""
}

// identifyGPU resolves a backend URL against the hosts config. Pure, so
// callers already holding s.mu can pass their snapshot of the hosts.
func identifyGPU(hosts []config.HostCfg, backend string) gpuIdentity {
	via := "direct"
	if isLinkURL(backend) {
		via = "link"
	}
	key := backendHostIP(backend) // "192.168.1.200" or "link:gpu-b"
	port := backendPort(backend)
	for _, h := range hosts {
		for _, k := range h.HostKeys() {
			if k == key {
				id := gpuIdentity{Key: h.Label + ":" + port, Label: h.Label, Host: h.Label, Via: via}
				if port != "" {
					id.Label += " :" + port
				}
				if idx := h.GPUsFor(port); len(idx) > 0 {
					id.GPUs = gpuNames(h, idx)
					id.Label += " · " + strings.Join(id.GPUs, " + ")
				}
				return id
			}
		}
	}
	// Unclaimed: fall back to a readable form of the address itself.
	label := strings.TrimPrefix(strings.TrimPrefix(backend, "http://"), "https://")
	if via == "link" {
		label = "link " + strings.TrimPrefix(label, "link/")
	}
	return gpuIdentity{Key: backend, Label: label, Via: via}
}

// gpuIdentity: identifyGPU against the live config (takes s.mu).
func (s *Server) gpuIdentity(backend string) gpuIdentity {
	s.mu.Lock()
	hosts := s.cfg.Hosts
	s.mu.Unlock()
	return identifyGPU(hosts, backend)
}
