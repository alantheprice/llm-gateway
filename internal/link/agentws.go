package link

import (
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// ServeAgent: WebSocket upgrade + read loop for one agent connection.
// Blocks until the socket dies; on death every in-flight relay fails and
// the registry drops the link's backends. owner is the authenticated
// link token's user (fallback agent label).
func ServeAgent(reg *Registry, w http.ResponseWriter, r *http.Request, owner string) {
	log.Printf("link: agent connection from %s (upgrade)", r.RemoteAddr)
	up := websocket.Upgrader{ReadBufferSize: 64 * 1024, WriteBufferSize: 64 * 1024}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	ws.SetReadLimit(64 << 20) // single relay frame ceiling
	var hello helloMsg
	if err := ws.ReadJSON(&hello); err != nil {
		return
	}
	if hello.Type != "hello" || len(hello.Links) == 0 {
		_ = ws.WriteJSON(map[string]string{"type": "error", "error": "first message must be hello with links"})
		return
	}
	agent := strings.TrimSpace(hello.Agent)
	if agent == "" {
		agent = owner
	}
	c := newConn(agent, hello.Links)
	c.ws = ws
	ids := reg.Register(c)
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
		if mt != websocket.BinaryMessage || len(data) < IDLen+1 {
			continue
		}
		id := binary.BigEndian.Uint64(data[8:IDLen])
		kind := data[IDLen]
		payload := data[IDLen+1:]
		_ = c.onFrame(id, kind, payload)
	}
}
