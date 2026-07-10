package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"gosocode/internal/models"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // tighten in prod
}

// handleWS upgrades to a WebSocket that receives this user's job progress
// (plus the live traffic feed for admins).
// The JWT is passed as ?token= because browsers can't set headers on WS.
func (s *Server) handleWS(c *gin.Context) {
	uid, ok := s.parseToken(c.Query("token"))
	if !ok {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	var u models.User
	if err := s.db.First(&u, uid).Error; err != nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	s.hub.Add(uid, conn, u.Role == "admin")
	defer s.hub.Remove(uid, conn)

	// Block reading until the client disconnects; we only push, never read.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
