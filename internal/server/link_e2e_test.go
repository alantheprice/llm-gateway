package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// End-to-end link tests drive the REAL cmd/link-agent binary (built once
// per run) against an in-process gateway and a fake streaming engine. The
// package-level fakeAgent in internal/link mirrors the protocol but not
// the agent's concurrency, which is where the agent broke before.

var (
	agentBinOnce sync.Once
	agentBin     string
	agentBinErr  error
)

func buildAgent(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds cmd/link-agent")
	}
	agentBinOnce.Do(func() {
		dir, err := osMkdirTemp()
		if err != nil {
			agentBinErr = err
			return
		}
		agentBin = filepath.Join(dir, "llm-link-agent")
		out, err := exec.Command("go", "build", "-o", agentBin, "llmgateway/cmd/link-agent").CombinedOutput()
		if err != nil {
			agentBinErr = fmt.Errorf("build agent: %v\n%s", err, out)
		}
	})
	if agentBinErr != nil {
		t.Fatal(agentBinErr)
	}
	return agentBin
}

// linkRig: gateway + fake SSE engine + running agent.
type linkRig struct {
	s        *Server
	gw       *httptest.Server
	engPort  string
	engAuth  chan string   // Authorization header of each engine request
	engGone  chan struct{} // an engine request saw its context cancelled
	agentOut *syncBuf
	agent    *exec.Cmd
}

func newLinkRig(t *testing.T, keyOwner, keyRole string, agentArgs ...string) (*linkRig, string) {
	t.Helper()
	bin := buildAgent(t)
	rig := &linkRig{engAuth: make(chan string, 64), engGone: make(chan struct{}, 64), agentOut: &syncBuf{}}
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slots": // NInfer shape, so the poller scores this engine
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"max_concurrency":8,"requests_processing":1,"requests_waiting":0}`)
			return
		case "/usage":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"energy":{"today":{"kwh":1.5,"cost_usd":0.19,"tokens":100000}},"tokens":{"input":{"cache_hit_rate_pct":42}}}`)
			return
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"m","max_model_len":262144}]}`)
			return
		case "/v1/models/m":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"m","model_card":{"schema":"ninfer-model-card/1","hardware":{"gpu":"RTX PRO 6000"}}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(404)
			return
		}
		rig.engAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		n := 30
		if r.Header.Get("Accept") == "slow" {
			n = 2000 // long stream for the cancellation test
		}
		for i := 0; i < n; i++ {
			select {
			case <-r.Context().Done():
				rig.engGone <- struct{}{}
				return
			default:
			}
			fmt.Fprintf(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			f.Flush()
			time.Sleep(3 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(eng.Close)
	_, rig.engPort, _ = net.SplitHostPort(strings.TrimPrefix(eng.URL, "http://"))

	conf := `{"gateway":{"port":0,"trust_local_networks":true},"local_networks":["192.168.1.0/24"],
	 "model_pools":{"qwen":{"members":[{"model_id":"m","backend":"http://link/e2e:` + rig.engPort + `"}]}},
	 "public_models":["qwen"]}`
	rig.s = testServer(t, conf, nil)
	key, _, err := rig.s.store.CreateKey(keyOwner, "link-e2e", keyRole, false)
	if err != nil {
		t.Fatal(err)
	}
	rig.gw = httptest.NewServer(rig.s.Handler())
	t.Cleanup(rig.gw.Close)

	args := append([]string{"--server", rig.gw.URL, "--token", key, "--name", "e2e",
		"--engine", "127.0.0.1:" + rig.engPort + "=m:8"}, agentArgs...)
	rig.agent = exec.Command(bin, args...)
	rig.agent.Stdout, rig.agent.Stderr = rig.agentOut, rig.agentOut
	if err := rig.agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rig.agent.Process.Kill(); rig.agent.Wait() })
	return rig, key
}

func (rig *linkRig) waitRegistered(t *testing.T) {
	t.Helper()
	url := "http://link/e2e:" + rig.engPort
	for i := 0; i < 100 && rig.s.linkReg.Lookup(url) == nil; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if rig.s.linkReg.Lookup(url) == nil {
		t.Fatalf("agent never registered:\n%s", rig.agentOut.String())
	}
}

func (rig *linkRig) stream(clientKey string) (int, string, error) {
	body := `{"model":"qwen","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", rig.gw.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientKey)
	req.Header.Set("Cookie", "llmgw_session=secret-session")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if e != nil {
			break
		}
	}
	return resp.StatusCode, sb.String(), nil
}

// Concurrent streams through one link all complete, and the agent
// survives (it used to panic on concurrent websocket writes).
func TestLinkE2EConcurrentStreams(t *testing.T) {
	rig, key := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, out, err := rig.stream(key)
			if err != nil || code != 200 || strings.Count(out, "tok") != 30 || !strings.Contains(out, "[DONE]") {
				errs <- fmt.Sprintf("stream %d: code=%d err=%v tokens=%d done=%t", i, code, err,
					strings.Count(out, "tok"), strings.Contains(out, "[DONE]"))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if strings.Contains(rig.agentOut.String(), "panic") {
		t.Fatalf("agent panicked:\n%s", rig.agentOut.String())
	}
	if rig.s.linkReg.Lookup("http://link/e2e:"+rig.engPort) == nil {
		t.Fatalf("link dropped during concurrent load:\n%s", rig.agentOut.String())
	}
}

// The client's gateway credentials never reach the remote engine; the
// agent's own --engine-key is used instead.
func TestLinkE2ENoCredentialLeak(t *testing.T) {
	rig, key := newLinkRig(t, "admin", "admin", "--engine-key", "engine-local-key")
	rig.waitRegistered(t)
	if code, _, err := rig.stream(key); err != nil || code != 200 {
		t.Fatalf("stream: %d %v", code, err)
	}
	got := <-rig.engAuth
	if strings.Contains(got, key) {
		t.Fatalf("client gateway key leaked to the engine: %q", got)
	}
	if got != "Bearer engine-local-key" {
		t.Fatalf("engine Authorization = %q, want the agent's --engine-key", got)
	}
}

// Ordinary user keys cannot open a link.
func TestLinkE2EUserKeyRefused(t *testing.T) {
	rig, _ := newLinkRig(t, "bob", "user")
	time.Sleep(500 * time.Millisecond)
	if rig.s.linkReg.Lookup("http://link/e2e:"+rig.engPort) != nil {
		t.Fatal("a plain user key registered a link")
	}
}

// A dedicated role=link key works without admin rights.
func TestLinkE2ELinkRoleKeyAccepted(t *testing.T) {
	rig, _ := newLinkRig(t, "carol", "link")
	rig.waitRegistered(t)
}

func osMkdirTemp() (string, error) { return os.MkdirTemp("", "link-agent-e2e-") }

// syncBuf: agent output captured by exec's copier goroutine and read by
// the test concurrently.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A client that disconnects mid-stream stops the remote engine: the
// gateway sends a cancel frame and the agent aborts the engine request.
func TestLinkE2EClientDisconnectCancelsEngine(t *testing.T) {
	rig, key := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)
	body := `{"model":"qwen","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", rig.gw.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "slow")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // client walks away mid-stream
	select {
	case <-rig.engGone:
	case <-time.After(5 * time.Second):
		t.Fatalf("engine request not cancelled after client disconnect:\n%s", rig.agentOut.String())
	}
}

// Link engines are polled over the agent socket: load score, lanes and the
// raw /usage payload exist exactly as for LAN backends.
func TestLinkE2EPolledOverRelay(t *testing.T) {
	rig, _ := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)
	u := "http://link/e2e:" + rig.engPort
	rig.s.PollOnce()
	l := rig.s.tracker.Get(u)
	if l == nil || l.Lanes != 8 || l.Running != 1 || l.CacheHitPct != 42 {
		t.Fatalf("link engine not polled over the relay: %+v", l)
	}
	if rig.s.tracker.IsDown(u) {
		t.Fatal("polled link engine marked down")
	}
	rig.s.mu.Lock()
	raw := rig.s.lastMetrics[u]
	rig.s.mu.Unlock()
	if usageNum(raw, "energy", "today", "kwh") != 1.5 {
		t.Fatalf("link /usage payload not captured: %v", raw)
	}
}

// An agent that disconnects takes its engines out of rotation at once.
func TestLinkE2EDisconnectMarksDown(t *testing.T) {
	rig, _ := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)
	u := "http://link/e2e:" + rig.engPort
	rig.agent.Process.Kill()
	for i := 0; i < 50 && !rig.s.tracker.IsDown(u); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if !rig.s.tracker.IsDown(u) {
		t.Fatal("disconnected link engine still in rotation")
	}
}

// A linked engine is reachable only through the pools that name it —
// never by its raw model id, which could shadow another model.
func TestLinkE2ENotResolvableByRawModelID(t *testing.T) {
	rig, key := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)
	rig.s.PollOnce()
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", rig.gw.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("raw engine model id routed to the linked engine")
	}
}

// The token must travel in the Authorization header; ?key= is refused.
func TestLinkQueryTokenRefused(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	key, _, _ := s.store.CreateKey("admin", "k", "admin", false)
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()
	req, _ := http.NewRequest("GET", gw.URL+"/link/agent?key="+key, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query-string token: status %d, want 401", resp.StatusCode)
	}
}

// /admin/links shows the agent, its owner and each engine's poll state.
func TestLinkE2EAdminView(t *testing.T) {
	rig, _ := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)
	key := uiAdminKey(t, rig.s)
	rig.s.PollOnce()
	req, _ := http.NewRequest("GET", rig.gw.URL+"/admin/links", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Agents []struct {
			Agent   string `json:"agent"`
			Owner   string `json:"owner"`
			Version string `json:"agent_version"`
			Engines []struct {
				URL    string   `json:"url"`
				Polled bool     `json:"polled"`
				Pools  []string `json:"pools"`
			} `json:"engines"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d err %v", resp.StatusCode, err)
	}
	if len(v.Agents) != 1 || len(v.Agents[0].Engines) != 1 {
		t.Fatalf("admin view = %+v", v)
	}
	e := v.Agents[0].Engines[0]
	if !e.Polled || len(e.Pools) != 1 || e.Pools[0] != "qwen" {
		t.Fatalf("engine view = %+v", e)
	}
}

// Pooled link engines appear in /backends (admin, marked via=link) and in
// /slots, like LAN backends.
func TestLinkE2EInBackendsAndSlots(t *testing.T) {
	rig, _ := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)
	key := uiAdminKey(t, rig.s)
	rig.s.PollOnce()
	u := "http://link/e2e:" + rig.engPort
	get := func(path string) []byte {
		req, _ := http.NewRequest("GET", rig.gw.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, e := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if e != nil {
				break
			}
		}
		return []byte(sb.String())
	}
	var backends struct {
		Backends []map[string]any `json:"backends"`
	}
	raw := get("/backends")
	if err := json.Unmarshal(raw, &backends); err != nil {
		t.Fatalf("/backends: %v %s", err, raw)
	}
	found := false
	for _, b := range backends.Backends {
		if b["url"] == u {
			found = true
			if b["via"] != "link" || b["lanes"] != float64(8) {
				t.Fatalf("link entry = %v", b)
			}
		}
	}
	if !found {
		t.Fatalf("link engine missing from /backends: %s", raw)
	}
	var slots map[string]any
	json.Unmarshal(get("/slots"), &slots)
	if _, ok := slots[u]; !ok {
		t.Fatalf("pooled link engine missing from /slots: %v", slots)
	}
}

// uiAdminKey: the admin plane (/backends, /admin/links) takes admin UI keys.
func uiAdminKey(t *testing.T, s *Server) string {
	t.Helper()
	k, _, err := s.store.CreateKey("admin", "ui-admin-test", "admin", true)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A linked engine's card reaches the pool view over the relay.
func TestLinkE2ECardOverRelay(t *testing.T) {
	rig, _ := newLinkRig(t, "admin", "admin")
	rig.waitRegistered(t)
	rig.s.PollOnce()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models/qwen", nil)
	r.RemoteAddr = "192.168.1.50:5555"
	rig.s.Handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "RTX PRO 6000") || !strings.Contains(w.Body.String(), `"via":"link"`) {
		t.Fatalf("link card missing from pool view: %s", w.Body.String())
	}
}
