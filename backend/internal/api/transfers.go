package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gosocode/internal/models"
)

// handleLogTransfer records a completed transfer. Downloads report here from the
// client; uploads are logged server-side by the worker on success.
func (s *Server) handleLogTransfer(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Kind string `json:"kind" binding:"required,oneof=upload download"`
		Name string `json:"name"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.db.Create(&models.TransferLog{
		UserID: uid, Kind: in.Kind, Name: in.Name, Path: in.Path, Size: in.Size, At: time.Now(),
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleListTransferLog returns the caller's completed-transfer history.
func (s *Server) handleListTransferLog(c *gin.Context) {
	uid := uidOf(c)
	var logs []models.TransferLog
	s.db.Where("user_id = ?", uid).Order("id desc").Limit(200).Find(&logs)
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}
