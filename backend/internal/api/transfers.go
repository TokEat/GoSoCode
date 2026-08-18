package api

import (
	"net/http"
	"strconv"
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
	s.logTransferEvent(&models.TransferEvent{
		UserID: uid, Kind: in.Kind, Stage: "finish", RefType: "client",
		Name: in.Name, Path: in.Path, Bytes: in.Size,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// logTransferEvent writes a fine-grained transfer stage event. It is best-effort
// and never fails the request.
func (s *Server) logTransferEvent(ev *models.TransferEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s.db.Create(ev)
}

// handleListTransferLog returns one page of the caller's completed-transfer
// history, newest first.
func (s *Server) handleListTransferLog(c *gin.Context) {
	uid := uidOf(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if size < 1 || size > 200 {
		size = 20
	}
	var total int64
	s.db.Model(&models.TransferLog{}).Where("user_id = ?", uid).Count(&total)
	var logs []models.TransferLog
	s.db.Where("user_id = ?", uid).Order("id desc").
		Offset((page - 1) * size).Limit(size).Find(&logs)
	c.JSON(http.StatusOK, gin.H{"logs": logs, "total": total, "page": page, "page_size": size})
}

// handleClearTransferLog deletes the caller's entire transfer history.
func (s *Server) handleClearTransferLog(c *gin.Context) {
	uid := uidOf(c)
	res := s.db.Where("user_id = ?", uid).Delete(&models.TransferLog{})
	s.audit(c, uid, usernameOf(c), "transfers.clear", "deleted="+strconv.FormatInt(res.RowsAffected, 10))
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted": res.RowsAffected})
}
