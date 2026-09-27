// llm-link-agent: outbound-only connector that registers local LLM
// engines with a remote llm-gateway and relays inference requests to
// them over the resulting WebSocket. No inbound ports; reconnects with
// backoff; relays only the declared engine port(s).
package main

import (
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
		err := run(*server, *token, agentName, engines)
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

func run(server, token, agent string, engines []engine) error {
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

	// ping loop keeps NAT mappings open and detects half-dead sockets.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()

	return readLoop(ws, engines)
}

// readLoop: dispatch control + relay-request frames.
func readLoop(ws *websocket.Conn, engines []engine) error {
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if mt != websocket.BinaryMessage || len(data) < 17 {
			continue
		}
		id := binary.BigEndian.Uint64(data[8:16])
		kind := data[16]
		payload := data[17:]
		if kind != 0 { // body/end frames belong to an active proxy, not new requests
			continue
		}

		var req struct {
			Method  string              `json:"method"`
			Host    string              `json:"host"`
			Path    string              `json:"path"`
			Headers map[string][]string `json:"headers"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			log.Printf("agent: bad header frame: %v (payload=%.80s)", err, payload)
			continue
		}
		log.Printf("agent: relay %s %s", req.Method, req.Path)
		// SSRF guard mirrors the gateway: only declared engine ports and
		// model-serving paths.
		eng := findEngine(engines, req.Host)
		if eng == nil || !validPath(req.Path) {
			writeError(ws, id, http.StatusForbidden, "not relayable")
			continue
		}
		go proxyRequest(ws, id, eng, req)
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

func proxyRequest(ws *websocket.Conn, id uint64, eng *engine, req struct {
	Method  string              `json:"method"`
	Host    string              `json:"host"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
}) {
	target := fmt.Sprintf("http://127.0.0.1:%d%s", eng.port, req.Path)
	httpReq, err := http.NewRequest(req.Method, target, nil)
	if err != nil {
		writeError(ws, id, http.StatusBadRequest, err.Error())
		return
	}
	for k, vs := range req.Headers {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	client := &http.Client{Timeout: 0} // streams have no deadline
	resp, err := client.Do(httpReq)
	if err != nil {
		writeError(ws, id, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	// response header frame
	hdr, _ := json.Marshal(map[string]any{
		"id": id, "status": resp.StatusCode, "headers": resp.Header,
	})
	if err := writeFrame(ws, id, 0, hdr); err != nil {
		return
	}
	// body chunks (streamed as they arrive — SSE safe)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if err := writeFrame(ws, id, 1, buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	_ = writeFrame(ws, id, 2, nil) // end
}

func writeFrame(ws *websocket.Conn, id uint64, kind byte, payload []byte) error {
	ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	buf := make([]byte, 17+len(payload))
	binary.BigEndian.PutUint64(buf[8:16], id)
	buf[16] = kind
	copy(buf[17:], payload)
	return ws.WriteMessage(websocket.BinaryMessage, buf)
}

func writeError(ws *websocket.Conn, id uint64, status int, msg string) {
	hdr, _ := json.Marshal(map[string]any{"id": id, "status": status,
		"headers": map[string][]string{"Content-Type": {"application/json"}}})
	_ = writeFrame(ws, id, 0, hdr)
	_ = writeFrame(ws, id, 1, []byte(`{"error":{"message":"`+msg+`"}}`))
	_ = writeFrame(ws, id, 2, nil)
}
