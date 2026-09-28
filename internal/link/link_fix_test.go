package link

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Writes never block, reads see data as it arrives, overflow cuts the
// stream off instead of wedging the writer.
func TestBufPipe(t *testing.T) {
	p := newBufPipe(8)
	if _, err := p.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, _ := p.Read(buf)
	if string(buf[:n]) != "abc" {
		t.Fatalf("read %q", buf[:n])
	}
	got := make(chan string, 1)
	go func() {
		n, _ := p.Read(buf)
		got <- string(buf[:n])
	}()
	time.Sleep(10 * time.Millisecond)
	p.Write([]byte("de"))
	select {
	case s := <-got:
		if s != "de" {
			t.Fatalf("incremental read %q", s)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked reader not woken by a write")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			p.Write([]byte("xxxx")) // nobody reading: must not block
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write blocked on an abandoned reader")
	}
	if _, err := io.ReadAll(p); err != errPipeOverflow {
		t.Fatalf("after overflow: err=%v, want errPipeOverflow", err)
	}
}

// Another user's agent cannot take over a registered label; the owner
// can reconnect.
func TestRegistryOwnership(t *testing.T) {
	r := NewRegistry()
	a := newConn("gpu", []Engine{{Port: 8006}})
	if _, err := r.RegisterAs(a, "alice"); err != nil {
		t.Fatal(err)
	}
	evil := newConn("gpu", []Engine{{Port: 8006}})
	if _, err := r.RegisterAs(evil, "mallory"); err == nil {
		t.Fatal("another user's agent took over the label")
	}
	if r.Lookup("http://link/gpu:8006") != a {
		t.Fatal("label no longer points at the owner's connection")
	}
	again := newConn("gpu", []Engine{{Port: 8006}})
	if _, err := r.RegisterAs(again, "alice"); err != nil {
		t.Fatalf("owner reconnect refused: %v", err)
	}
}

// Cancelling the caller's context sends a cancel frame to the agent and
// fails the body.
func TestRelayCancelSendsCancelFrame(t *testing.T) {
	reg := NewRegistry()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeAgent(reg, w, r, "owner")
	}))
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.WriteJSON(map[string]any{"type": "hello", "agent": "c1",
		"links": []map[string]any{{"model_id": "m", "port": 9}}})
	var welcome map[string]any
	ws.ReadJSON(&welcome)
	conn := reg.Lookup("http://link/c1:9")
	if conn == nil {
		t.Fatal("not registered")
	}

	kinds := make(chan byte, 16)
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if len(data) >= 17 {
				kinds <- data[16]
				if data[16] == kindEnd { // answer with a header so the relay returns a body
					hdr := []byte(`{"status":200,"headers":{"Content-Type":["text/event-stream"]}}`)
					frame := append(append(make([]byte, 16), kindHeader), hdr...)
					copy(frame[8:16], data[8:16])
					ws.WriteMessage(websocket.BinaryMessage, frame)
				}
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := conn.RelayContext(ctx, "POST", "127.0.0.1:9", "/v1/chat/completions", http.Header{}, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case k := <-kinds:
			if k == kindCancel {
				if _, err := io.ReadAll(resp.Body); err == nil {
					t.Fatal("body should fail after cancel")
				}
				return
			}
		case <-deadline:
			t.Fatal("no cancel frame sent to the agent")
		}
	}
}
