// Package realtime pushes job progress to platform users over WebSocket.
// Each user may have multiple connections (tabs); messages fan out to all.
package realtime

import (
	"sync"

	"github.com/gorilla/websocket"
)

type Hub struct {
	mu     sync.RWMutex
	conns  map[uint]map[*websocket.Conn]struct{}
	admins map[*websocket.Conn]struct{} // subset receiving admin broadcasts (live traffic)
}

func NewHub() *Hub {
	return &Hub{
		conns:  make(map[uint]map[*websocket.Conn]struct{}),
		admins: make(map[*websocket.Conn]struct{}),
	}
}

func (h *Hub) Add(userID uint, c *websocket.Conn, admin bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[userID] == nil {
		h.conns[userID] = make(map[*websocket.Conn]struct{})
	}
	h.conns[userID][c] = struct{}{}
	if admin {
		h.admins[c] = struct{}{}
	}
}

func (h *Hub) Remove(userID uint, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.conns[userID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.conns, userID)
		}
	}
	delete(h.admins, c)
	c.Close()
}

// BroadcastAdmins pushes a payload to every admin connection. Used for the
// live traffic feed so the monitoring page doesn't need to poll.
func (h *Hub) BroadcastAdmins(payload any) {
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.admins))
	for c := range h.admins {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		_ = c.WriteJSON(payload)
	}
}

// Publish sends a JSON-serializable payload to all of a user's connections.
func (h *Hub) Publish(userID uint, payload any) {
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.conns[userID]))
	for c := range h.conns[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		_ = c.WriteJSON(payload)
	}
}
