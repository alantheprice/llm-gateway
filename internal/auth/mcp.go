package auth

// MCPServer is one remote MCP server a user connected for agent chat. It
// is reached over HTTP (Streamable HTTP or the older HTTP+SSE transport),
// so nothing runs on the user's machine.
type MCPServer struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"` // tool prefix: <name>__<tool>
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"` // e.g. Authorization; secret
	Enabled bool              `json:"enabled"`
	Added   string            `json:"added,omitempty"`
}

func copyMCP(in []MCPServer) []MCPServer {
	out := make([]MCPServer, len(in))
	for i, m := range in {
		out[i] = m
		if m.Headers != nil {
			out[i].Headers = make(map[string]string, len(m.Headers))
			for k, v := range m.Headers {
				out[i].Headers[k] = v
			}
		}
	}
	return out
}

// MCPServersOf returns a copy of user's MCP servers.
func (s *Store) MCPServersOf(user string) []MCPServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyMCP(s.MCPServers[user])
}

// SetMCPServers replaces user's MCP servers and persists them.
func (s *Store) SetMCPServers(user string, list []MCPServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.MCPServers == nil {
		s.MCPServers = map[string][]MCPServer{}
	}
	if len(list) == 0 {
		delete(s.MCPServers, user)
	} else {
		s.MCPServers[user] = copyMCP(list)
	}
	return s.saveLocked()
}
