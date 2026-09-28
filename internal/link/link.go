// Package link: outbound-only remote GPU registration.
//
// A GPU machine on a remote network runs the link agent, which dials the
// gateway's /link/agent WebSocket with a link token and registers its
// local engines. The gateway exposes each engine as a virtual backend
// (http://link/<agent>:<port>) that pool members can reference; requests
// to it are relayed over the agent's socket as id-framed HTTP exchanges,
// so SSE streams pass through untouched and new engine endpoints need no
// agent changes.
//
// Frame protocol on the socket:
//
//	text    JSON control: hello / welcome / error
//	binary  [8B zero][8B request-id][1B kind][payload]
//	          kind 0 = request header (gw→agent) / response header (agent→gw)
//	          kind 1 = body chunk
//	          kind 2 = end-of-body
//	          kind 3 = cancel (gw→agent): the client went away or the
//	                   response was abandoned; the agent aborts the engine
//	                   request. Older agents ignore it.
package link

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// IDLen: request-id byte width in relay frame headers.
const IDLen = 16

// frame kinds.
const (
	kindHeader = 0
	kindBody   = 1
	kindEnd    = 2
	kindCancel = 3
)

// respBufCap: per-response buffer between the socket read loop and the
// HTTP relay. LLM streams trickle far below this; only an abandoned
// reader fills it.
const respBufCap = 16 << 20

// Engine: one locally-served model endpoint declared by an agent.
type Engine struct {
	ModelID        string `json:"model_id"`
	Port           int    `json:"port"`
	MaxConcurrency int    `json:"max_concurrency"`
	Label          string `json:"label,omitempty"`
}

// helloMsg: agent → gateway after upgrade.
type helloMsg struct {
	Type         string   `json:"type"`
	AgentVersion string   `json:"agent_version,omitempty"`
	Agent        string   `json:"agent"`
	Links        []Engine `json:"links"`
}

// welcomeMsg: gateway → agent acknowledging registration.
type welcomeMsg struct {
	Type    string   `json:"type"`
	LinkIDs []string `json:"link_ids"`
}

// relayRequest: gateway → agent request header (kind 0).
type relayRequest struct {
	ID      uint64      `json:"id"`
	Method  string      `json:"method"`
	Host    string      `json:"host"` // agent-local engine host:port
	Path    string      `json:"path"`
	Headers http.Header `json:"headers,omitempty"`
}

// relayHeader: agent → gateway response header (kind 0).
type relayHeader struct {
	ID      uint64              `json:"id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
}

func (h relayHeader) header() http.Header {
	out := http.Header{}
	for k, vs := range h.Headers {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

// Conn: one connected agent (possibly several engines).
type Conn struct {
	Agent      string // stable label; virtual URLs derive from it
	Owner      string // gateway user whose key registered this agent
	Version    string // agent_version from hello
	RemoteAddr string
	Since      time.Time
	ws         *websocket.Conn
	wmu        sync.Mutex // serialize writes
	engines    []Engine

	mu     sync.Mutex
	nextID uint64
	reqs   map[uint64]*pendingReq
	closed bool
}

// pendingReq: one relayed exchange, from request send to end-of-body.
type pendingReq struct {
	hdr      chan *relayResponse // response header (or error), sent once
	answered bool                // hdr has been sent
	pipe     *bufPipe            // response body once the header arrived
	fin      chan struct{}       // closed when the exchange ends
}

func newConn(agent string, engines []Engine) *Conn {
	return &Conn{
		Agent:   agent,
		engines: engines,
		reqs:    map[uint64]*pendingReq{},
	}
}

// relayResponse: handed to the Relay caller.
type relayResponse struct {
	status int
	header http.Header
	body   io.ReadCloser
	err    error
}

// bodyReader: the caller's view of a relayed body. Closing it before the
// end of the stream abandons the exchange, which cancels it at the agent.
type bodyReader struct {
	c    *Conn
	id   uint64
	pipe *bufPipe
}

func (b *bodyReader) Read(p []byte) (int, error) { return b.pipe.Read(p) }
func (b *bodyReader) Close() error {
	b.c.cancel(b.id, io.ErrClosedPipe) // no-op once the exchange ended
	return nil
}

func (c *Conn) sendJSON(v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteJSON(v)
}

// sendFrame: [8B zero][8B id][1B kind][payload] as one binary message.
func (c *Conn) sendFrame(id uint64, kind byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	buf := make([]byte, IDLen+1+len(payload))
	binary.BigEndian.PutUint64(buf[0:8], 0)
	binary.BigEndian.PutUint64(buf[8:IDLen], id)
	buf[IDLen] = kind
	copy(buf[IDLen+1:], payload)
	return c.ws.WriteMessage(websocket.BinaryMessage, buf)
}

func (c *Conn) reserveID() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// Closed: socket down.
func (c *Conn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Engines: declared engines.
func (c *Conn) Engines() []Engine {
	out := make([]Engine, len(c.engines))
	copy(out, c.engines)
	return out
}

// InFlight: requests currently relayed through this link.
func (c *Conn) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// MinAgentVersion: oldest agent protocol the gateway accepts. 0.2 added
// header-only token auth, cancel frames and serialized socket writes;
// 0.1 agents panic under concurrent load and leak nothing but also
// cannot be cancelled, so they are refused.
const MinAgentVersion = "0.2"

// VersionAtLeast compares dotted numeric versions ("0.10" > "0.9").
func VersionAtLeast(have, want string) bool {
	hp, wp := strings.Split(have, "."), strings.Split(want, ".")
	for i := 0; i < len(wp); i++ {
		var h, w int
		if i < len(hp) {
			if _, err := fmt.Sscanf(hp[i], "%d", &h); err != nil {
				return false
			}
		}
		fmt.Sscanf(wp[i], "%d", &w)
		if h != w {
			return h > w
		}
	}
	return true
}

// VirtualURL: pool-member backend URL for an engine on this link.
func VirtualURL(agent string, port int) string {
	return "http://link/" + strings.ToLower(agent) + ":" + itoa(port)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Relay: RelayContext without cancellation (tests, internal probes).
func (c *Conn) Relay(method, engineHost, path string, header http.Header, body []byte) (*http.Response, error) {
	return c.RelayContext(context.Background(), method, engineHost, path, header, body)
}

// RelayContext sends an HTTP request over the link; the response body
// streams back through a buffered pipe. When ctx ends first (client gone)
// the agent is told to abort the engine request and the body closes with
// ctx's error.
func (c *Conn) RelayContext(ctx context.Context, method, engineHost, path string, header http.Header, body []byte) (*http.Response, error) {
	if c.Closed() {
		return nil, fmt.Errorf("link %q is disconnected", c.Agent)
	}
	if err := ValidateRelay(engineHost, path); err != nil {
		return nil, err
	}

	id := c.reserveID()
	p := &pendingReq{hdr: make(chan *relayResponse, 1), fin: make(chan struct{})}
	c.mu.Lock()
	c.reqs[id] = p
	c.mu.Unlock()

	if err := c.sendJSONRelayRequest(id, method, engineHost, path, header); err != nil {
		c.finish(id, err)
		return nil, err
	}
	if len(body) > 0 {
		if err := c.sendFrame(id, kindBody, body); err != nil {
			c.finish(id, err)
			return nil, err
		}
	}
	if err := c.sendFrame(id, kindEnd, nil); err != nil {
		c.finish(id, err)
		return nil, err
	}

	// Watch the caller for the whole exchange, header wait AND body.
	go func() {
		select {
		case <-ctx.Done():
			c.cancel(id, ctx.Err())
		case <-p.fin:
		}
	}()

	res := <-p.hdr
	if res.err != nil {
		return nil, res.err
	}
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", res.status, http.StatusText(res.status)),
		StatusCode: res.status,
		Header:     res.header,
		Body:       res.body,
	}, nil
}

func (c *Conn) sendJSONRelayRequest(id uint64, method, host, path string, header http.Header) error {
	payload, err := json.Marshal(relayRequest{ID: id, Method: method, Host: host, Path: path, Headers: header})
	if err != nil {
		return err
	}
	return c.sendFrame(id, kindHeader, payload)
}

// finish ends exchange id: an unanswered caller gets err, an open body
// closes with err (nil = clean EOF). Idempotent.
func (c *Conn) finish(id uint64, err error) {
	c.mu.Lock()
	p := c.reqs[id]
	delete(c.reqs, id)
	c.mu.Unlock()
	if p == nil {
		return
	}
	c.settle(p, err)
}

func (c *Conn) settle(p *pendingReq, err error) {
	if !p.answered {
		p.answered = true
		if err == nil {
			err = fmt.Errorf("link %q: exchange ended before a response header", c.Agent)
		}
		p.hdr <- &relayResponse{err: err}
	}
	if p.pipe != nil {
		p.pipe.CloseWithError(err)
	}
	close(p.fin)
}

// cancel aborts exchange id locally and asks the agent to stop the
// engine request. The frame is sent off the caller's goroutine: cancel can
// run from the socket read loop, which must never block on a write.
func (c *Conn) cancel(id uint64, err error) {
	c.mu.Lock()
	_, live := c.reqs[id]
	c.mu.Unlock()
	if !live {
		return
	}
	c.finish(id, err)
	go func() { _ = c.sendFrame(id, kindCancel, nil) }()
}

// reader-side state machine (driven by ServeAgent's read loop). Never
// blocks: body chunks land in a buffered pipe.
func (c *Conn) onFrame(id uint64, kind byte, payload []byte) error {
	switch kind {
	case kindHeader:
		var h relayHeader
		if err := json.Unmarshal(payload, &h); err != nil {
			return nil // malformed header frame: ignore
		}
		c.mu.Lock()
		p := c.reqs[id]
		if p == nil || p.answered {
			c.mu.Unlock()
			return nil // cancelled or duplicate
		}
		p.answered = true
		p.pipe = newBufPipe(respBufCap)
		c.mu.Unlock()
		p.hdr <- &relayResponse{status: h.Status, header: h.header(), body: &bodyReader{c: c, id: id, pipe: p.pipe}}
	case kindBody:
		c.mu.Lock()
		p := c.reqs[id]
		c.mu.Unlock()
		if p != nil && p.pipe != nil {
			if _, err := p.pipe.Write(payload); err != nil {
				c.cancel(id, err) // reader abandoned: stop the engine too
			}
		}
	case kindEnd:
		c.finish(id, nil)
	}
	return nil
}

func (c *Conn) failAll(err error) {
	c.mu.Lock()
	c.closed = true
	reqs := c.reqs
	c.reqs = map[uint64]*pendingReq{}
	c.mu.Unlock()
	for _, p := range reqs {
		c.settle(p, err)
	}
}
