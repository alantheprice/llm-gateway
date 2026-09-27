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
package link

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// IDLen: request-id byte width in relay frame headers.
const IDLen = 16

// frame kinds.
const (
	kindHeader = 0
	kindBody   = 1
	kindEnd    = 2
)

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
	Agent   string // stable label; virtual URLs derive from it
	ws      *websocket.Conn
	wmu     sync.Mutex // serialize writes
	engines []Engine

	mu      sync.Mutex
	nextID  uint64
	waiters map[uint64]chan *relayResponse // waiting for response header
	streams map[uint64]*io.PipeWriter      // active response bodies
	closed  bool
}

func newConn(agent string, engines []Engine) *Conn {
	return &Conn{
		Agent:   agent,
		engines: engines,
		waiters: map[uint64]chan *relayResponse{},
		streams: map[uint64]*io.PipeWriter{},
	}
}

// relayResponse: handed to the Relay caller.
type relayResponse struct {
	status int
	header http.Header
	body   io.Reader
	err    error
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
	return len(c.streams)
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

// Relay: send an HTTP request over the link; response body streams back
// through a pipe. Caller must read to completion (or abandon it — a
// dead waiter just drains).
func (c *Conn) Relay(method, engineHost, path string, header http.Header, body []byte) (*http.Response, error) {
	if c.Closed() {
		return nil, fmt.Errorf("link %q is disconnected", c.Agent)
	}
	if err := ValidateRelay(engineHost, path); err != nil {
		return nil, err
	}

	id := c.reserveID()
	done := make(chan *relayResponse, 1)
	c.mu.Lock()
	c.waiters[id] = done
	c.mu.Unlock()

	if err := c.sendJSONRelayRequest(id, method, engineHost, path, header); err != nil {
		c.dropWaiter(id)
		return nil, err
	}
	if len(body) > 0 {
		if err := c.sendFrame(id, kindBody, body); err != nil {
			c.dropWaiter(id)
			return nil, err
		}
	}
	if err := c.sendFrame(id, kindEnd, nil); err != nil {
		c.dropWaiter(id)
		return nil, err
	}

	res := <-done
	if res.err != nil {
		return nil, res.err
	}
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", res.status, http.StatusText(res.status)),
		StatusCode: res.status,
		Header:     res.header,
		Body:       io.NopCloser(res.body),
	}, nil
}

func (c *Conn) sendJSONRelayRequest(id uint64, method, host, path string, header http.Header) error {
	payload, err := json.Marshal(relayRequest{ID: id, Method: method, Host: host, Path: path, Headers: header})
	if err != nil {
		return err
	}
	return c.sendFrame(id, kindHeader, payload)
}

func (c *Conn) dropWaiter(id uint64) {
	c.mu.Lock()
	delete(c.waiters, id)
	c.mu.Unlock()
}

// reader-side state machine (drive from ServeAgent's read loop).
func (c *Conn) onFrame(id uint64, kind byte, payload []byte) error {
	switch kind {
	case kindHeader:
		var h relayHeader
		if err := json.Unmarshal(payload, &h); err != nil {
			return nil // malformed header frame: ignore
		}
		h.ID = id
		pr, pw := io.Pipe()
		c.mu.Lock()
		wait, wok := c.waiters[h.ID]
		if wok {
			delete(c.waiters, h.ID)
			c.streams[h.ID] = pw
		}
		c.mu.Unlock()
		if !wok {
			pw.Close()
			return nil
		}
		wait <- &relayResponse{status: h.Status, header: h.header(), body: pr}
	case kindBody:
		c.mu.Lock()
		pw := c.streams[id]
		c.mu.Unlock()
		if pw != nil {
			_, _ = pw.Write(payload)
		}
	case kindEnd:
		c.mu.Lock()
		pw := c.streams[id]
		delete(c.streams, id)
		c.mu.Unlock()
		if pw != nil {
			pw.Close()
		}
	}
	return nil
}

func (c *Conn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, pw := range c.streams {
		pw.CloseWithError(err)
		delete(c.streams, id)
	}
	for id, ch := range c.waiters {
		ch <- &relayResponse{err: err}
		delete(c.waiters, id)
	}
}
