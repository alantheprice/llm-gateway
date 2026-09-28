package server

import (
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
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
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
