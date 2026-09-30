package main

// Remote MCP client: connects to MCP servers over HTTP so their tools can
// join the agent loop, with nothing running on the user's machine.
//
// Transports:
//   - Streamable HTTP (MCP 2025-03-26+): JSON-RPC POSTs to one endpoint;
//     each reply is JSON or an SSE stream; the server may assign an
//     Mcp-Session-Id that later requests carry.
//   - HTTP+SSE (MCP 2024-11-05): GET opens an event stream whose first
//     "endpoint" event names where to POST; replies arrive on the stream.
//     Tried when the endpoint refuses a POST.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sprout-foundry/seed/core"
)

const mcpProtocolVersion = "2025-06-18"

// mcpServerCfg is what the gateway sends for each enabled server.
type mcpServerCfg struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// ---- network guard ----

// errPrivate: a user-supplied server resolved to a non-public address.
var errPrivate = errors.New("address is not public (loopback, private or link-local networks are refused)")

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		// carrier-grade NAT and IPv4-mapped/documentation ranges
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64))
}

// mcpHTTPClient: allowPrivate lets admins reach LAN servers; everyone else
// is limited to public addresses, checked at connect time (after DNS, so
// rebinding can't slip through).
func mcpHTTPClient(allowPrivate bool, timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !publicIP(ip) {
				return errPrivate
			}
			return nil
		}
	}
	tr := &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: timeout, MaxIdleConnsPerHost: 2, Proxy: nil}
	return &http.Client{Transport: tr, Timeout: 0, // streams are bounded by ctx
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		}}
}

// ---- client ----

type mcpClient struct {
	cfg     mcpServerCfg
	http    *http.Client
	nextID  atomic.Int64
	session string // Mcp-Session-Id (streamable)
	version string // negotiated protocol version

	// legacy HTTP+SSE transport
	legacy   bool
	postURL  string
	stream   io.ReadCloser
	pending  map[string]chan rpcResponse
	pendMu   sync.Mutex
	streamOK chan struct{}
	endpoint chan string
}

func newMCPClient(cfg mcpServerCfg, allowPrivate bool) *mcpClient {
	return &mcpClient{cfg: cfg, http: mcpHTTPClient(allowPrivate, 60*time.Second)}
}

func (c *mcpClient) setHeaders(h http.Header) {
	for k, v := range c.cfg.Headers {
		if strings.TrimSpace(k) != "" {
			h.Set(k, v)
		}
	}
}

// Connect performs the initialize handshake.
func (c *mcpClient) Connect(ctx context.Context) error {
	u, err := url.Parse(c.cfg.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("invalid server URL")
	}
	params := map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "llm-gateway-agent", "version": "1.0"},
	}
	res, err := c.call(ctx, "initialize", params)
	var he *httpStatusError
	if err != nil && errors.As(err, &he) && (he.code == 404 || he.code == 405 || he.code == 400) {
		// Older servers: HTTP+SSE transport.
		if lerr := c.openLegacy(ctx); lerr != nil {
			return fmt.Errorf("%v (and the older SSE transport failed: %v)", err, lerr)
		}
		res, err = c.call(ctx, "initialize", params)
	}
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(res, &init)
	c.version = init.ProtocolVersion
	return c.notify(ctx, "notifications/initialized")
}

// ListTools pages through tools/list.
func (c *mcpClient) ListTools(ctx context.Context) ([]mcpTool, error) {
	var all []mcpTool
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var out struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &out); err != nil {
			return nil, fmt.Errorf("tools/list: %v", err)
		}
		all = append(all, out.Tools...)
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	return all, nil
}

// CallTool runs one tool and flattens its result to text.
func (c *mcpClient) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Resource *struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("tools/call: %v", err)
	}
	var sb strings.Builder
	for _, p := range out.Content {
		switch p.Type {
		case "text":
			sb.WriteString(p.Text)
		case "resource":
			if p.Resource != nil {
				if p.Resource.Text != "" {
					sb.WriteString(p.Resource.Text)
				} else {
					sb.WriteString("[resource " + p.Resource.URI + "]")
				}
			}
		case "resource_link":
			sb.WriteString("[link " + p.Name + " " + p.URI + "]")
		default: // image, audio
			sb.WriteString("[" + p.Type + " " + p.MimeType + " omitted]")
		}
		sb.WriteString("\n")
	}
	if sb.Len() == 0 && len(out.StructuredContent) > 0 {
		sb.Write(out.StructuredContent)
	}
	text := strings.TrimSpace(sb.String())
	if out.IsError {
		return "", errors.New(truncate(text, 2000))
	}
	return text, nil
}

func (c *mcpClient) Close() {
	if c.stream != nil {
		c.stream.Close()
	}
	if c.session != "" && !c.legacy {
		// Best effort: end the server-side session.
		req, err := http.NewRequest(http.MethodDelete, c.cfg.URL, nil)
		if err == nil {
			c.setHeaders(req.Header)
			req.Header.Set("Mcp-Session-Id", c.session)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if resp, err := c.http.Do(req.WithContext(ctx)); err == nil {
				resp.Body.Close()
			}
		}
	}
}

type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string {
	switch e.code {
	case 401, 403:
		return fmt.Sprintf("server refused the credentials (%d); check the auth header", e.code)
	}
	return fmt.Sprintf("server returned %d: %s", e.code, truncate(strings.TrimSpace(e.body), 200))
}

func (c *mcpClient) notify(ctx context.Context, method string) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if c.legacy {
		return c.legacyPost(ctx, msg)
	}
	resp, err := c.post(ctx, msg)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *mcpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	var r rpcResponse
	var err error
	if c.legacy {
		r, err = c.legacyCall(ctx, id, msg)
	} else {
		r, err = c.streamableCall(ctx, id, msg)
	}
	if err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s: %s (%d)", method, r.Error.Message, r.Error.Code)
	}
	return r.Result, nil
}

func (c *mcpClient) post(ctx context.Context, msg any) (*http.Response, error) {
	blob, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &httpStatusError{code: resp.StatusCode, body: string(b)}
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	return resp, nil
}

func (c *mcpClient) streamableCall(ctx context.Context, id int64, msg any) (rpcResponse, error) {
	resp, err := c.post(ctx, msg)
	if err != nil {
		return rpcResponse{}, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 16<<20)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var found *rpcResponse
		err := readSSE(body, func(event, data string) bool {
			var r rpcResponse
			if json.Unmarshal([]byte(data), &r) == nil && idMatches(r.ID, id) {
				found = &r
				return false
			}
			return true // notifications, progress: keep reading
		})
		if found != nil {
			return *found, nil
		}
		if err == nil {
			err = errors.New("stream ended without a reply")
		}
		return rpcResponse{}, err
	}
	var r rpcResponse
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return rpcResponse{}, fmt.Errorf("bad reply: %v", err)
	}
	return r, nil
}

func idMatches(raw json.RawMessage, id int64) bool {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n == id
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == fmt.Sprint(id)
}

// readSSE calls fn per event until fn returns false or the stream ends.
func readSSE(r io.Reader, fn func(event, data string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	event, data := "", []string{}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				if !fn(event, strings.Join(data, "\n")) {
					return nil
				}
			}
			event, data = "", data[:0]
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	return sc.Err()
}

// ---- legacy HTTP+SSE ----

func (c *mcpClient) openLegacy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.cfg.URL, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req.Header)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body.Close()
		return fmt.Errorf("no event stream (%d)", resp.StatusCode)
	}
	c.legacy, c.stream = true, resp.Body
	c.pending = map[string]chan rpcResponse{}
	c.endpoint = make(chan string, 1)
	go func() {
		_ = readSSE(resp.Body, func(event, data string) bool {
			if event == "endpoint" {
				select {
				case c.endpoint <- strings.TrimSpace(data):
				default:
				}
				return true
			}
			var r rpcResponse
			if json.Unmarshal([]byte(data), &r) == nil && len(r.ID) > 0 {
				c.pendMu.Lock()
				ch := c.pending[string(r.ID)]
				delete(c.pending, string(r.ID))
				c.pendMu.Unlock()
				if ch != nil {
					ch <- r
				}
			}
			return true
		})
		c.pendMu.Lock()
		for k, ch := range c.pending {
			close(ch)
			delete(c.pending, k)
		}
		c.pendMu.Unlock()
	}()
	select {
	case ep := <-c.endpoint:
		base, _ := url.Parse(c.cfg.URL)
		ref, err := url.Parse(ep)
		if err != nil {
			return fmt.Errorf("bad endpoint event")
		}
		post := base.ResolveReference(ref)
		if post.Host != base.Host {
			return fmt.Errorf("endpoint on another host refused")
		}
		c.postURL = post.String()
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("no endpoint event")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *mcpClient) legacyPost(ctx context.Context, msg any) error {
	blob, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.postURL, bytes.NewReader(blob))
	if err != nil {
		return err
	}
	c.setHeaders(req.Header)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpStatusError{code: resp.StatusCode, body: string(b)}
	}
	return nil
}

func (c *mcpClient) legacyCall(ctx context.Context, id int64, msg any) (rpcResponse, error) {
	ch := make(chan rpcResponse, 1)
	key := fmt.Sprint(id)
	c.pendMu.Lock()
	c.pending[key] = ch
	c.pendMu.Unlock()
	if err := c.legacyPost(ctx, msg); err != nil {
		c.pendMu.Lock()
		delete(c.pending, key)
		c.pendMu.Unlock()
		return rpcResponse{}, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return rpcResponse{}, errors.New("event stream closed")
		}
		return r, nil
	case <-ctx.Done():
		return rpcResponse{}, ctx.Err()
	}
}

// ---- executor ----

var toolNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// mcpToolName: <server>__<tool>, safe for OpenAI function names (≤64).
func mcpToolName(server, tool string) string {
	n := toolNameUnsafe.ReplaceAllString(server, "_") + "__" + toolNameUnsafe.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

type boundTool struct {
	client *mcpClient
	server string
	name   string // the server's own tool name
}

// ToolsExecutor serves the built-in web tools plus every connected MCP
// server's tools.
type ToolsExecutor struct {
	base    *SearchExecutor
	tools   []core.Tool
	bound   map[string]boundTool
	clients []*mcpClient
}

// connectMCP connects to each server (in parallel, bounded) and returns an
// executor with their tools; servers that fail are reported, not fatal.
func connectMCP(ctx context.Context, base *SearchExecutor, servers []mcpServerCfg, allowPrivate bool) (*ToolsExecutor, []string) {
	ex := &ToolsExecutor{base: base, bound: map[string]boundTool{}}
	ex.tools = append(ex.tools, base.GetTools()...)
	type result struct {
		cfg    mcpServerCfg
		client *mcpClient
		tools  []mcpTool
		err    error
	}
	results := make([]result, len(servers))
	var wg sync.WaitGroup
	for i, sv := range servers {
		wg.Add(1)
		go func(i int, sv mcpServerCfg) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			c := newMCPClient(sv, allowPrivate)
			err := c.Connect(cctx)
			var tools []mcpTool
			if err == nil {
				tools, err = c.ListTools(cctx)
			}
			if err != nil {
				c.Close()
				c = nil
			}
			results[i] = result{cfg: sv, client: c, tools: tools, err: err}
		}(i, sv)
	}
	wg.Wait()
	var problems []string
	for _, r := range results {
		if r.err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", r.cfg.Name, r.err))
			continue
		}
		ex.clients = append(ex.clients, r.client)
		for _, t := range r.tools {
			name := mcpToolName(r.cfg.Name, t.Name)
			if _, dup := ex.bound[name]; dup {
				continue
			}
			var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
			if len(t.InputSchema) > 0 {
				var sc map[string]any
				if json.Unmarshal(t.InputSchema, &sc) == nil && sc != nil {
					if _, ok := sc["type"]; !ok {
						sc["type"] = "object"
					}
					schema = sc
				}
			}
			ex.bound[name] = boundTool{client: r.client, server: r.cfg.Name, name: t.Name}
			ex.tools = append(ex.tools, core.Tool{Type: "function", Function: core.ToolFunction{
				Name:        name,
				Description: truncate("["+r.cfg.Name+"] "+t.Description, 1024),
				Parameters:  schema,
			}})
		}
	}
	return ex, problems
}

func (e *ToolsExecutor) GetTools() []core.Tool { return e.tools }

func (e *ToolsExecutor) Execute(ctx context.Context, calls []core.ToolCall) []core.Message {
	out := make([]core.Message, 0, len(calls))
	var base []core.ToolCall
	for _, call := range calls {
		b, ok := e.bound[call.Function.Name]
		if !ok {
			base = append(base, call)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		result, err := b.client.CallTool(cctx, b.name, json.RawMessage(call.Function.Arguments))
		cancel()
		status := core.ToolStatusCompleted
		if err != nil {
			result, status = "tool error: "+err.Error(), core.ToolStatusError
		}
		out = append(out, core.Message{Role: "tool", Content: truncate(result, 16000), ToolCallID: call.ID, Status: status})
	}
	if len(base) > 0 {
		out = append(out, e.base.Execute(ctx, base)...)
	}
	// Keep replies in the order the calls were made.
	order := map[string]int{}
	for i, c := range calls {
		order[c.ID] = i
	}
	sortByCall(out, order)
	return out
}

func sortByCall(msgs []core.Message, order map[string]int) {
	for i := 1; i < len(msgs); i++ {
		for j := i; j > 0 && order[msgs[j].ToolCallID] < order[msgs[j-1].ToolCallID]; j-- {
			msgs[j], msgs[j-1] = msgs[j-1], msgs[j]
		}
	}
}

func (e *ToolsExecutor) Close() {
	for _, c := range e.clients {
		c.Close()
	}
}

// serverNames: the MCP servers that connected, for the system prompt.
func (e *ToolsExecutor) serverNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range e.bound {
		if !seen[b.server] {
			seen[b.server] = true
			out = append(out, b.server)
		}
	}
	return out
}
