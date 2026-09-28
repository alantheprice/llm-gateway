package link

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeAgent: dials the server, sends hello, then proxies exactly like
// cmd/link-agent does (header/body/end framing, engine relay).
type fakeAgent struct {
	ws     *websocket.Conn
	server string
	token  string
	agent  string
	port   int // local "engine" port to proxy to
}

func (f *fakeAgent) dial(t *testing.T) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(f.server, "http") + "/link/agent"
	ws, _, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + f.token}})
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	f.ws = ws
	hello := map[string]any{
		"type": "hello", "agent": f.agent, "agent_version": MinAgentVersion,
		"links": []map[string]any{{
			"model_id": "test-model", "port": f.port, "max_concurrency": 2, "label": "test",
		}},
	}
	if err := ws.WriteJSON(hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	var welcome struct {
		Type    string   `json:"type"`
		LinkIDs []string `json:"link_ids"`
	}
	if err := ws.ReadJSON(&welcome); err != nil {
		t.Fatalf("welcome: %v", err)
	}
	if welcome.Type != "welcome" || len(welcome.LinkIDs) == 0 {
		t.Fatalf("welcome = %+v", welcome)
	}
	go f.readLoop()
}

func (f *fakeAgent) readLoop() {
	// Mirror the fixed protocol: buffer body frames per id, dispatch at
	// the end frame with the body attached.
	pending := map[uint64]*struct {
		method, path string
		body         []byte
	}{}
	for {
		mt, data, err := f.ws.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage || len(data) < 17 {
			continue
		}
		id := binary.BigEndian.Uint64(data[8:16])
		kind := data[16]
		payload := data[17:]
		switch kind {
		case 0:
			var req struct {
				Method string `json:"method"`
				Path   string `json:"path"`
			}
			if json.Unmarshal(payload, &req) != nil {
				continue
			}
			pending[id] = &struct {
				method, path string
				body         []byte
			}{method: req.Method, path: req.Path}
		case 1:
			if p := pending[id]; p != nil {
				p.body = append(p.body, payload...)
			}
		case 2:
			p := pending[id]
			delete(pending, id)
			if p == nil {
				continue
			}
			out, status := f.callEngine(p.method, "http://127.0.0.1:"+itoa(f.port)+p.path, p.body)
			hdr, _ := json.Marshal(map[string]any{
				"id": id, "status": status, "headers": map[string][]string{"Content-Type": {"application/json"}},
			})
			f.writeFrame(id, 0, hdr)
			f.writeFrame(id, 1, out)
			f.writeFrame(id, 2, nil)
		}
	}
}

func (f *fakeAgent) callEngine(method, url string, body []byte) ([]byte, int) {
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []byte(`{"error":"engine down"}`), http.StatusBadGateway
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode
}

func (f *fakeAgent) writeFrame(id uint64, kind byte, payload []byte) {
	buf := make([]byte, 17+len(payload))
	binary.BigEndian.PutUint64(buf[8:16], id)
	buf[16] = kind
	copy(buf[17:], payload)
	f.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	f.ws.WriteMessage(websocket.BinaryMessage, buf)
}

func TestRegistryLifecycle(t *testing.T) {
	r := NewRegistry()
	c := newConn("dave", []Engine{{ModelID: "m", Port: 8006, MaxConcurrency: 4}})
	ids := r.Register(c)
	if len(ids) != 1 || ids[0] != "http://link/dave:8006" {
		t.Fatalf("ids = %v", ids)
	}
	if r.Lookup("http://link/dave:8006") != c {
		t.Fatal("lookup failed after register")
	}
	// same label re-register replaces
	c2 := newConn("dave", []Engine{{ModelID: "m", Port: 8006}})
	r.Register(c2)
	if r.Lookup("http://link/dave:8006") != c2 {
		t.Fatal("re-register did not replace")
	}
	r.Unregister("dave", c2)
	if r.Lookup("http://link/dave:8006") != nil {
		t.Fatal("unregister left the link")
	}
}

func TestValidEnginePath(t *testing.T) {
	for _, p := range []string{"/v1/chat/completions", "/v1/models", "/health", "/usage"} {
		if !ValidEnginePath(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []string{"/admin", "/", "/etc/passwd", "/v2/x"} {
		if ValidEnginePath(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
	if err := ValidateRelay("192.168.5.5:8006", "/v1/models"); err == nil {
		t.Error("non-local relay host must be rejected")
	}
}

func TestRelayRoundTrip(t *testing.T) {
	var reg = NewRegistry()

	// fake engine
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	}))
	defer engine.Close()

	// gateway-side ws endpoint
	mux := http.NewServeMux()
	mux.HandleFunc("/link/agent", func(w http.ResponseWriter, r *http.Request) {
		ServeAgent(reg, w, r, "fallback-owner")
	})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	pn := 0
	fmt.Sscanf(engine.URL[len("http://127.0.0.1:"):], "%d", &pn)
	fa := &fakeAgent{server: gw.URL, token: "test", agent: "dave-4090"}
	fa.port = pn
	fa.dial(t)

	// wait for registration
	var conn *Conn
	for i := 0; i < 50; i++ {
		if conn = reg.Lookup("http://link/dave-4090:" + itoa(fa.port)); conn != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("agent never registered")
	}

	resp, err := conn.Relay(http.MethodGet, "127.0.0.1:"+itoa(fa.port), "/v1/models", http.Header{}, nil)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), `"ok":true`) {
		t.Fatalf("relay body = %q", string(buf[:n]))
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// SSRF: non-engine path rejected before leaving the gateway
	if _, err := conn.Relay(http.MethodGet, "127.0.0.1:"+itoa(fa.port), "/admin/users", nil, nil); err == nil {
		t.Fatal("non-relayable path was allowed")
	}
}

// Regression (audit MF1): POST bodies must reach the engine. The original
// agent dropped kind-1/kind-2 frames, so every POST through a link
// backend hit the engine with an empty body.
func TestRelayRoundTrip_PostBody(t *testing.T) {
	var gotBody string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":true}`))
	}))
	defer engine.Close()
	pn := 0
	fmt.Sscanf(engine.URL[len("http://127.0.0.1:"):], "%d", &pn)

	reg := NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("/link/agent", func(w http.ResponseWriter, r *http.Request) {
		ServeAgent(reg, w, r, "owner")
	})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	// in-process agent mirroring cmd/link-agent's fixed protocol
	wsURL := "ws" + strings.TrimPrefix(gw.URL, "http") + "/link/agent?key=tok"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	hello := map[string]any{"type": "hello", "agent": "postbot", "agent_version": MinAgentVersion,
		"links": []map[string]any{{"model_id": "m", "port": pn, "max_concurrency": 1}}}
	if err := ws.WriteJSON(hello); err != nil {
		t.Fatalf("hello: %v", err)
	}

	// serve the agent side: header → buffer body → end → engine w/ body
	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage || len(data) < 17 {
				continue
			}
			id := binary.BigEndian.Uint64(data[8:16])
			kind := data[16]
			payload := data[17:]
			switch kind {
			case 0:
				var req struct {
					Method string `json:"method"`
					Path   string `json:"path"`
				}
				json.Unmarshal(payload, &req)
				t.Logf("postbot: got request %s %s", req.Method, req.Path)
				resp, err := http.Post("http://127.0.0.1:"+itoa(pn)+req.Path,
					"application/json", bytes.NewReader([]byte(`{"probe":"body-arrived"}`)))
				t.Logf("postbot: engine responded %v", err)
				if err != nil {
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				writeAgentFrame := func(kind byte, payload []byte) {
					frame := make([]byte, 17+len(payload))
					binary.BigEndian.PutUint64(frame[8:16], id)
					frame[16] = kind
					copy(frame[17:], payload)
					ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
					ws.WriteMessage(websocket.BinaryMessage, frame)
				}
				hdr, _ := json.Marshal(map[string]any{"id": id, "status": resp.StatusCode,
					"headers": map[string][]string{"Content-Type": {"application/json"}}})
				writeAgentFrame(0, hdr)
				writeAgentFrame(1, b)
				writeAgentFrame(2, nil)
			}
		}
	}()

	// wait for registration
	var conn *Conn
	for i := 0; i < 50; i++ {
		if conn = reg.Lookup("http://link/postbot:" + itoa(pn)); conn != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("agent never registered")
	}

	resp, err := conn.Relay(http.MethodPost, "127.0.0.1:"+itoa(pn), "/v1/chat/completions",
		http.Header{"Content-Type": []string{"application/json"}},
		[]byte(`{"probe":"body-arrived"}`))
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(gotBody, "body-arrived") {
		t.Fatalf("engine request body = %q, want the relayed body", gotBody)
	}
	if !strings.Contains(string(b), `"echo":true`) {
		t.Fatalf("engine response = %q", string(b))
	}
}
