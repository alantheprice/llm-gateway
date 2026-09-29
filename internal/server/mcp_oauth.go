package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"llmgateway/internal/auth"
)

// OAuth sign-in for remote MCP servers (MCP authorization spec, 2025-06-18):
//
//  1. Discovery: the server's 401 names its protected-resource metadata
//     (RFC 9728), which lists the authorization server; that server's
//     metadata (RFC 8414 / OpenID) gives the endpoints. Servers following
//     the older spec are their own authorization server.
//  2. The gateway registers itself as a client (RFC 7591) unless it already
//     did for this server and callback address.
//  3. Authorization code with PKCE (S256) and the resource indicator
//     (RFC 8707), in a popup that returns to /api/mcp/oauth/callback.
//  4. Tokens are stored with the server (encrypted at rest) and refreshed
//     before each use.

const oauthPendingTTL = 10 * time.Minute

type oauthPending struct {
	user, serverID, verifier, redirect string
	admin                              bool // may reach LAN addresses
	created                            time.Time
}

type oauthState struct {
	mu      sync.Mutex
	pending map[string]*oauthPending
}

func (o *oauthState) put(state string, p *oauthPending) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending == nil {
		o.pending = map[string]*oauthPending{}
	}
	for k, v := range o.pending {
		if time.Since(v.created) > oauthPendingTTL {
			delete(o.pending, k)
		}
	}
	o.pending[state] = p
}

func (o *oauthState) take(state string) *oauthPending {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.pending[state]
	delete(o.pending, state)
	if p == nil || time.Since(p.created) > oauthPendingTTL {
		return nil
	}
	return p
}

// ---- outbound HTTP with an address guard ----

func publicAddr(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64 { // carrier-grade NAT
		return false
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast())
}

// outboundClient: for requests to user-supplied URLs. Unless allowPrivate
// (admins), only public addresses are dialled, checked after DNS.
func outboundClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !publicAddr(ip) {
				return errors.New("address is not public")
			}
			return nil
		}
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second, Proxy: nil}}
}

func fetchJSON(ctx context.Context, c *http.Client, u string, dest any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode, fmt.Errorf("%s: %d", u, resp.StatusCode)
	}
	return 200, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dest)
}

// ---- discovery ----

var wwwAuthParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

// wellKnown: RFC 8414 path insertion, e.g. https://a/x → https://a/.well-known/<name>/x.
func wellKnown(base *url.URL, name string) string {
	p := strings.TrimRight(base.EscapedPath(), "/")
	return base.Scheme + "://" + base.Host + "/.well-known/" + name + p
}

// discoverOAuth finds the authorization server for an MCP server URL.
func discoverOAuth(ctx context.Context, c *http.Client, mcpURL string) (*asMetadata, string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return nil, "", err
	}
	// 1. Ask the server: its 401 names the resource metadata (and a scope).
	var prmURL, scope string
	probe, _ := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"llm-gateway","version":"1"}}}`))
	probe.Header.Set("Content-Type", "application/json")
	probe.Header.Set("Accept", "application/json, text/event-stream")
	if resp, err := c.Do(probe); err == nil {
		resp.Body.Close()
		for _, m := range wwwAuthParam.FindAllStringSubmatch(resp.Header.Get("WWW-Authenticate"), -1) {
			switch m[1] {
			case "resource_metadata":
				prmURL = m[2]
			case "scope":
				scope = m[2]
			}
		}
	}
	// 2. Protected resource metadata: advertised, else the well-known paths.
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	candidates := []string{prmURL, wellKnown(u, "oauth-protected-resource"), u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"}
	var issuers []string
	for _, cu := range candidates {
		if cu == "" {
			continue
		}
		if _, err := fetchJSON(ctx, c, cu, &prm); err == nil && len(prm.AuthorizationServers) > 0 {
			issuers = prm.AuthorizationServers
			if scope == "" {
				scope = strings.Join(prm.ScopesSupported, " ")
			}
			break
		}
	}
	if len(issuers) == 0 {
		issuers = []string{u.Scheme + "://" + u.Host} // older spec: the server is its own AS
	}
	// 3. Authorization server metadata.
	for _, iss := range issuers {
		iu, err := url.Parse(iss)
		if err != nil {
			continue
		}
		var urls []string
		if strings.Trim(iu.Path, "/") == "" {
			urls = []string{wellKnown(iu, "oauth-authorization-server"), wellKnown(iu, "openid-configuration")}
		} else {
			urls = []string{wellKnown(iu, "oauth-authorization-server"), wellKnown(iu, "openid-configuration"),
				strings.TrimRight(iss, "/") + "/.well-known/openid-configuration"}
		}
		for _, mu := range urls {
			var md asMetadata
			if _, err := fetchJSON(ctx, c, mu, &md); err == nil && md.AuthorizationEndpoint != "" && md.TokenEndpoint != "" {
				if scope == "" {
					scope = strings.Join(md.ScopesSupported, " ")
				}
				return &md, scope, nil
			}
		}
		if len(issuers) == 1 && iss == u.Scheme+"://"+u.Host {
			// Older-spec default endpoints.
			return &asMetadata{Issuer: iss, AuthorizationEndpoint: iss + "/authorize", TokenEndpoint: iss + "/token",
				RegistrationEndpoint: iss + "/register"}, scope, nil
		}
	}
	return nil, "", errors.New("couldn't find this server's sign-in (OAuth) endpoints")
}

func oauthRand(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// requestOrigin: the origin the browser used (behind a proxy or tunnel too).
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// register: dynamic client registration (RFC 7591), public client.
func registerClient(ctx context.Context, c *http.Client, endpoint, redirect string) (id, secret string, err error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "LLM Gateway",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Error        string `json:"error_description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode >= 300 || out.ClientID == "" {
		return "", "", fmt.Errorf("client registration failed (%d) %s", resp.StatusCode, out.Error)
	}
	return out.ClientID, out.ClientSecret, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// tokenRequest posts a token-endpoint form (client_secret_post when the
// client has a secret).
func tokenRequest(ctx context.Context, c *http.Client, o *auth.MCPOAuth, form url.Values) (*tokenResponse, error) {
	form.Set("client_id", o.ClientID)
	if o.ClientSecret != "" {
		form.Set("client_secret", o.ClientSecret)
	}
	if o.Resource != "" {
		form.Set("resource", o.Resource)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr tokenResponse
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &tr); err != nil {
		// Some servers answer form-encoded (GitHub without Accept honoured).
		if v, perr := url.ParseQuery(string(body)); perr == nil && v.Get("access_token") != "" {
			tr.AccessToken, tr.RefreshToken, tr.Scope = v.Get("access_token"), v.Get("refresh_token"), v.Get("scope")
		}
	}
	if resp.StatusCode >= 300 || tr.AccessToken == "" {
		msg := tr.ErrorDesc
		if msg == "" {
			msg = tr.Error
		}
		return nil, fmt.Errorf("token request failed (%d) %s", resp.StatusCode, msg)
	}
	return &tr, nil
}

func applyTokens(o *auth.MCPOAuth, tr *tokenResponse) {
	o.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		o.RefreshToken = tr.RefreshToken
	}
	o.Expiry = 0
	if tr.ExpiresIn > 0 {
		o.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
}

// ---- handlers ----

// handleMCPOAuthStart: GET /api/mcp/oauth/start?id=<server id> — discovers,
// registers if needed, and redirects the (popup) browser to sign in.
func (s *Server) handleMCPOAuthStart(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	user, isAdmin := sess.U, sess.Role == "admin"
	id := r.URL.Query().Get("id")
	list := s.store.MCPServersOf(user)
	idx := -1
	for i, m := range list {
		if m.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		oauthResultPage(w, false, "No such connector.")
		return
	}
	m := list[idx]
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c := outboundClient(isAdmin)
	md, scope, err := discoverOAuth(ctx, c, m.URL)
	if err != nil {
		oauthResultPage(w, false, err.Error())
		return
	}
	redirect := requestOrigin(r) + "/api/mcp/oauth/callback"
	o := m.OAuth
	if o == nil {
		o = &auth.MCPOAuth{}
	}
	if o.ClientID == "" || o.RedirectURI != redirect || o.TokenEndpoint != md.TokenEndpoint {
		if md.RegistrationEndpoint == "" {
			oauthResultPage(w, false, "This server doesn't support automatic client registration, so the gateway can't sign in to it yet. Use an API token in a header instead, if the service offers one.")
			return
		}
		cid, csec, err := registerClient(ctx, c, md.RegistrationEndpoint, redirect)
		if err != nil {
			oauthResultPage(w, false, err.Error())
			return
		}
		o.ClientID, o.ClientSecret, o.RedirectURI = cid, csec, redirect
	}
	o.Issuer, o.AuthorizationEndpoint, o.TokenEndpoint = md.Issuer, md.AuthorizationEndpoint, md.TokenEndpoint
	o.Scope, o.Resource = scope, canonicalResource(m.URL)
	list[idx].OAuth = o
	if err := s.store.SetMCPServers(user, list); err != nil {
		oauthResultPage(w, false, "Couldn't save the connector.")
		return
	}

	verifier := oauthRand(48)
	sum := sha256.Sum256([]byte(verifier))
	state := oauthRand(24)
	s.oauth.put(state, &oauthPending{user: user, serverID: id, verifier: verifier, redirect: redirect, admin: isAdmin, created: time.Now()})
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.ClientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {o.Resource},
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	sep := "?"
	if strings.Contains(o.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	http.Redirect(w, r, o.AuthorizationEndpoint+sep+q.Encode(), http.StatusFound)
}

// canonicalResource: the MCP server URL as a resource indicator (RFC 8707:
// no fragment; lowercase scheme and host).
func canonicalResource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment, u.Scheme, u.Host = "", strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return u.String()
}

// handleMCPOAuthCallback: the authorization server sends the browser back
// here with a code; exchange it and store the tokens.
func (s *Server) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := s.oauth.take(q.Get("state"))
	if p == nil {
		oauthResultPage(w, false, "This sign-in link expired or was already used. Start again from Connectors.")
		return
	}
	if sess, ok := s.sessionFrom(r); !ok || sess.U != p.user {
		oauthResultPage(w, false, "Sign in to the gateway in this browser first, then try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		oauthResultPage(w, false, "The service declined: "+e+" "+q.Get("error_description"))
		return
	}
	list := s.store.MCPServersOf(p.user)
	idx := -1
	for i, m := range list {
		if m.ID == p.serverID {
			idx = i
		}
	}
	if idx < 0 || list[idx].OAuth == nil {
		oauthResultPage(w, false, "The connector was removed.")
		return
	}
	o := list[idx].OAuth
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tr, err := tokenRequest(ctx, outboundClient(p.admin), o, url.Values{
		"grant_type": {"authorization_code"}, "code": {q.Get("code")},
		"redirect_uri": {p.redirect}, "code_verifier": {p.verifier},
	})
	if err != nil {
		oauthResultPage(w, false, err.Error())
		return
	}
	applyTokens(o, tr)
	list[idx].Enabled = true
	if err := s.store.SetMCPServers(p.user, list); err != nil {
		oauthResultPage(w, false, "Couldn't save the sign-in.")
		return
	}
	log.Printf("User %s signed in to MCP server %s", p.user, list[idx].Name)
	oauthResultPage(w, true, "Signed in to "+list[idx].Name+". You can close this window.")
}

func oauthResultPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	status := "error"
	if ok {
		status = "ok"
	}
	payload, _ := json.Marshal(map[string]string{"type": "mcp-oauth", "status": status, "message": msg})
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Connector sign-in</title>
<body style="font:15px system-ui,sans-serif;max-width:460px;margin:60px auto;padding:0 16px;color:#222">
<h2 style="font-size:18px">%s</h2><p>%s</p>
<script>try{if(window.opener){window.opener.postMessage(%s,location.origin);%s}}catch(e){}</script></body>`,
		map[bool]string{true: "✓ Connected", false: "Sign-in didn't complete"}[ok], html.EscapeString(msg), payload,
		map[bool]string{true: "setTimeout(function(){window.close()},900);", false: ""}[ok])
}

// freshMCPToken returns a usable access token for an OAuth connector,
// refreshing (and saving) it when it expires within a minute.
func (s *Server) freshMCPToken(ctx context.Context, user string, m auth.MCPServer) (string, error) {
	o := m.OAuth
	if o == nil || o.AccessToken == "" {
		return "", errors.New("not signed in")
	}
	if o.Expiry == 0 || time.Until(time.Unix(o.Expiry, 0)) > time.Minute {
		return o.AccessToken, nil
	}
	if o.RefreshToken == "" {
		return "", errors.New("sign-in expired; sign in again")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tr, err := tokenRequest(ctx, outboundClient(s.store.RoleOf(user) == "admin"), o,
		url.Values{"grant_type": {"refresh_token"}, "refresh_token": {o.RefreshToken}})
	if err != nil {
		return "", fmt.Errorf("refreshing sign-in: %v", err)
	}
	list := s.store.MCPServersOf(user)
	for i := range list {
		if list[i].ID == m.ID && list[i].OAuth != nil {
			applyTokens(list[i].OAuth, tr)
			o = list[i].OAuth
			_ = s.store.SetMCPServers(user, list)
		}
	}
	return o.AccessToken, nil
}

// mcpHeaders: the headers to send a connector, with a fresh bearer token
// for OAuth ones.
func (s *Server) mcpHeaders(ctx context.Context, user string, m auth.MCPServer) (map[string]string, error) {
	h := map[string]string{}
	for k, v := range m.Headers {
		h[k] = v
	}
	if m.OAuth != nil {
		tok, err := s.freshMCPToken(ctx, user, m)
		if err != nil {
			return nil, err
		}
		h["Authorization"] = "Bearer " + tok
	}
	return h, nil
}
