package link

import (
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// agentIdleTimeout: an agent socket silent this long is treated as dead
// (agents ping every 15s).
const agentIdleTimeout = 60 * time.Second

// validAgentLabel: labels become virtual-URL hosts (http://link/<label>:port).
func validAgentLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return false
		}
	}
	return true
}

// ServeAgent: WebSocket upgrade + read loop for one agent connection.
// Blocks until the socket dies; on death every in-flight relay fails and
// the registry drops the link's backends. owner is the authenticated
// link token's user: it owns the agent label (fallback label if the hello
// carries none), and no other user can take the label over.
func ServeAgent(reg *Registry, w http.ResponseWriter, r *http.Request, owner string) {
	log.Printf("link: agent connection from %s (upgrade)", r.RemoteAddr)
	up := websocket.Upgrader{ReadBufferSize: 64 * 1024, WriteBufferSize: 64 * 1024}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	ws.SetReadLimit(64 << 20) // single relay frame ceiling
	// Dead-socket detection: the agent pings every 15s; a silent socket
	// (half-open TCP, frozen host) fails every in-flight relay within
	// agentIdleTimeout instead of hanging them forever.
	extend := func() { _ = ws.SetReadDeadline(time.Now().Add(agentIdleTimeout)) }
	extend()
	ws.SetPingHandler(func(data string) error {
		extend()
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	var hello helloMsg
	if err := ws.ReadJSON(&hello); err != nil {
		return
	}
	if hello.Type != "hello" || len(hello.Links) == 0 {
		_ = ws.WriteJSON(map[string]string{"type": "error", "error": "first message must be hello with links"})
		return
	}
	agent := strings.ToLower(strings.TrimSpace(hello.Agent))
	if agent == "" {
		agent = strings.ToLower(owner)
	}
	if !validAgentLabel(agent) {
		_ = ws.WriteJSON(map[string]string{"type": "error", "error": "agent name must be 1-63 chars of a-z 0-9 . _ -"})
		return
	}
	c := newConn(agent, hello.Links)
	c.Owner = owner
	c.ws = ws
	ids, err := reg.RegisterAs(c, owner)
	if err != nil {
		log.Printf("link: agent %q rejected for %s: %v", agent, owner, err)
		_ = ws.WriteJSON(map[string]string{"type": "error", "error": err.Error()})
		return
	}
	log.Printf("link: agent %q registered %v (owner=%s)", agent, ids, owner)
	defer func() {
		reg.Unregister(agent, c)
		c.failAll(fmt.Errorf("link disconnected"))
		log.Printf("link: agent %q unregistered", agent)
	}()
	_ = c.sendJSON(welcomeMsg{Type: "welcome", LinkIDs: ids})

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			c.failAll(fmt.Errorf("link disconnected"))
			return
		}
		extend()
		if mt != websocket.BinaryMessage || len(data) < IDLen+1 {
			continue
		}
		id := binary.BigEndian.Uint64(data[8:IDLen])
		kind := data[IDLen]
		payload := data[IDLen+1:]
		_ = c.onFrame(id, kind, payload)
	}
}
