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
	OAuth   *MCPOAuth         `json:"oauth,omitempty"` // set for servers you sign in to
}

// MCPOAuth: an OAuth sign-in to a connector. Secrets (client secret,
// tokens) are encrypted at rest with the rest of users.json's secrets.
type MCPOAuth struct {
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
	ClientID              string `json:"client_id,omitempty"`
	ClientSecret          string `json:"client_secret,omitempty"`
	RedirectURI           string `json:"redirect_uri,omitempty"`
	Scope                 string `json:"scope,omitempty"`
	Resource              string `json:"resource,omitempty"`
	AccessToken           string `json:"access_token,omitempty"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	Expiry                int64  `json:"expiry,omitempty"` // unix seconds; 0 = unknown
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
		if m.OAuth != nil {
			o := *m.OAuth
			out[i].OAuth = &o
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
