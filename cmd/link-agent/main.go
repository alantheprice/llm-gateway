// llm-link-agent: outbound-only connector that registers local LLM
// engines with a remote llm-gateway and relays inference requests to
// them over the resulting WebSocket. No inbound ports; reconnects with
// backoff; relays only the declared engine port(s).
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type engine struct {
	port    int
	modelID string
	label   string
	maxConc int
}

func main() {
	var (
		server   = flag.String("server", "", "gateway base URL, e.g. https://gw.example.com")
		token    = flag.String("token", os.Getenv("LLM_LINK_TOKEN"), "link token (a gateway key)")
		agent    = flag.String("name", "", "agent label (default: hostname)")
		engines  engineFlags
		interval = flag.Duration("retry-max", 30*time.Second, "max reconnect backoff")
		engKey   = flag.String("engine-key", os.Getenv("LLM_LINK_ENGINE_KEY"), "bearer key the local engine requires, if any (never taken from relayed requests)")
	)
	flag.Var(&engines, "engine", "local engine, host:port=model-id[:max-conc] (repeatable)")
	flag.Parse()

	if *server == "" || *token == "" || len(engines) == 0 {
		fmt.Fprintln(os.Stderr, "required: --server, --token, --engine host:port=model-id[:max-conc]")
		os.Exit(2)
	}
	agentName := *agent
	if agentName == "" {
		h, _ := os.Hostname()
		agentName = strings.ToLower(h)
	}

	backoff := time.Second
	for {
		err := run(*server, *token, agentName, *engKey, engines)
		log.Printf("link down: %v — reconnecting in %v", err, backoff)
		time.Sleep(backoff + time.Duration(rand.Intn(1000))*time.Millisecond)
		if backoff < *interval {
			backoff *= 2
		}
	}
}

type engineFlags []engine

func (e *engineFlags) String() string { return fmt.Sprint(*e) }
func (e *engineFlags) Set(v string) error {
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("engine must be host:port=model-id[:max-conc]")
	}
	hostport, spec := parts[0], parts[1]
	_, portStr, found := strings.Cut(hostport, ":")
	if !found {
		return fmt.Errorf("engine host must include :port")
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return fmt.Errorf("bad port %q", portStr)
	}
	bits := strings.SplitN(spec, ":", 3)
	maxc := 4
	if len(bits) > 2 {
		if _, err := fmt.Sscanf(bits[2], "%d", &maxc); err != nil {
			return fmt.Errorf("bad max-conc %q", bits[2])
		}
	}
	*e = append(*e, engine{port: port, modelID: bits[0], label: hostport, maxConc: maxc})
	return nil
}

func run(server, token, agent, engineKey string, engines []engine) error {
	wsURL, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("bad --server: %w", err)
	}
	switch wsURL.Scheme {
	case "https":
		wsURL.Scheme = "wss"
	case "http":
		wsURL.Scheme = "ws"
	default:
		return fmt.Errorf("--server must be http(s)")
	}
	wsURL.Path = "/link/agent"
	q := wsURL.Query()
	q.Set("key", token)
	wsURL.RawQuery = q.Encode()

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.Dial(wsURL.String(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	log.Printf("connected to %s", server)

	hello := map[string]any{
		"type":          "hello",
		"agent_version": "0.1",
		"agent":         agent,
	}
	links := make([]map[string]any, 0, len(engines))
	for _, e := range engines {
		links = append(links, map[string]any{
			"model_id": e.modelID, "port": e.port,
			"max_concurrency": e.maxConc, "label": e.label,
		})
	}
	hello["links"] = links
	if err := ws.WriteJSON(hello); err != nil {
		return err
	}

	// ping loop keeps NAT mappings open and lets the gateway detect a dead
	// socket. WriteControl is safe alongside the frame writer.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}()

	return readLoop(&wsWriter{ws: ws}, ws, engines, engineKey)
}

// wsWriter serializes data frames: gorilla/websocket allows one writer at
// a time, and every relayed request writes from its own goroutine.
// Concurrent writes panic the agent ("concurrent write to websocket
// connection"), dropping every in-flight request on the link.
type wsWriter struct {
	mu sync.Mutex
	ws *websocket.Conn
}

// relayRequest: the kind-0 header frame's JSON shape (gateway → agent).
type relayRequest struct {
	ID      uint64              `json:"id"`
	Method  string              `json:"method"`
	Host    string              `json:"host"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
}

// pendingReq: a relay request whose body frames are still arriving.
type pendingReq struct {
	req  relayRequest
	body []byte
}

// readLoop: dispatch control + relay-request frames.
func readLoop(w *wsWriter, ws *websocket.Conn, engines []engine, engineKey string) error {
	pending := map[uint64]*pendingReq{}
	var cmu sync.Mutex
	cancels := map[uint64]context.CancelFunc{} // in-flight engine requests
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			cmu.Lock()
			for _, cancel := range cancels {
				cancel() // gateway gone: stop every engine request
			}
			cmu.Unlock()
			return err
		}
		if mt != websocket.BinaryMessage || len(data) < 17 {
			continue
		}
		id := binary.BigEndian.Uint64(data[8:16])
		kind := data[16]
		payload := data[17:]

		switch kind {
		case 0: // request header
			var req relayRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				log.Printf("agent: bad header frame: %v (payload=%.80s)", err, payload)
				continue
			}
			log.Printf("agent: relay %s %s", req.Method, req.Path)
			pending[id] = &pendingReq{req: req}

		case 1: // request body chunk
			if pr := pending[id]; pr != nil {
				pr.body = append(pr.body, payload...)
			}

		case 2: // request end → dispatch to the engine
			pr := pending[id]
			delete(pending, id)
			if pr == nil {
				continue
			}
			// SSRF guard mirrors the gateway: only declared engine ports
			// and model-serving paths.
			eng := findEngine(engines, pr.req.Host)
			if eng == nil || !validPath(pr.req.Path) {
				writeError(w, id, http.StatusForbidden, "not relayable")
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			cmu.Lock()
			cancels[id] = cancel
			cmu.Unlock()
			go func() {
				defer func() {
					cmu.Lock()
					delete(cancels, id)
					cmu.Unlock()
					cancel()
				}()
				proxyRequest(ctx, w, id, eng, pr.req, pr.body, engineKey)
			}()

		case 3: // cancel: client gone or response abandoned at the gateway
			delete(pending, id)
			cmu.Lock()
			if cancel := cancels[id]; cancel != nil {
				cancel()
			}
			cmu.Unlock()
		}
	}
}

func findEngine(engines []engine, host string) *engine {
	for i := range engines {
		if strings.HasSuffix(host, fmt.Sprintf(":%d", engines[i].port)) ||
			host == fmt.Sprintf("127.0.0.1:%d", engines[i].port) ||
			host == fmt.Sprintf("localhost:%d", engines[i].port) {
			return &engines[i]
		}
	}
	return nil
}

func validPath(p string) bool {
	for _, prefix := range []string{"/v1/", "/health", "/usage", "/slots", "/metrics"} {
		if p == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// stripHeaders: never forwarded to the engine, whatever the gateway sent.
var stripHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "Cookie": true,
	"Host": true, "Connection": true, "Content-Length": true,
	"Transfer-Encoding": true, "Upgrade": true, "Keep-Alive": true,
}

func proxyRequest(ctx context.Context, w *wsWriter, id uint64, eng *engine, req relayRequest, body []byte, engineKey string) {
	target := fmt.Sprintf("http://127.0.0.1:%d%s", eng.port, req.Path)
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, id, http.StatusBadRequest, err.Error())
		return
	}
	for k, vs := range req.Headers {
		if stripHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if engineKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+engineKey)
	}
	client := &http.Client{Timeout: 0} // streams have no deadline; ctx cancels
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return // cancelled by the gateway; nobody is waiting
		}
		writeError(w, id, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	// response header frame
	hdr, _ := json.Marshal(map[string]any{
		"id": id, "status": resp.StatusCode, "headers": resp.Header,
	})
	if err := w.writeFrame(id, 0, hdr); err != nil {
		return
	}
	// body chunks (streamed as they arrive — SSE safe)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if err := w.writeFrame(id, 1, buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return // cancelled mid-stream: the gateway already closed its side
	}
	_ = w.writeFrame(id, 2, nil) // end
}

func (w *wsWriter) writeFrame(id uint64, kind byte, payload []byte) error {
	buf := make([]byte, 17+len(payload))
	binary.BigEndian.PutUint64(buf[8:16], id)
	buf[16] = kind
	copy(buf[17:], payload)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return w.ws.WriteMessage(websocket.BinaryMessage, buf)
}

func writeError(w *wsWriter, id uint64, status int, msg string) {
	hdr, _ := json.Marshal(map[string]any{"id": id, "status": status,
		"headers": map[string][]string{"Content-Type": {"application/json"}}})
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg}})
	_ = w.writeFrame(id, 0, hdr)
	_ = w.writeFrame(id, 1, body)
	_ = w.writeFrame(id, 2, nil)
}
