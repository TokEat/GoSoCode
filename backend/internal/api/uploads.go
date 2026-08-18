package api

import (
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"gosocode/internal/models"
)

// handleUploadInit opens a resumable upload session: allocates a temp file and
// records the target logical path + expected size.
func (s *Server) handleUploadInit(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Name string `json:"name" binding:"required"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logical := in.Path
	if logical == "" {
		logical = "/" + in.Name
	}
	if s.temporaryAccessDenied(c, logical) {
		return
	}
	tmp, err := os.CreateTemp("", "gsc-rsu-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmp.Close()
	sess := models.UploadSession{
		ID: uuid.NewString(), UserID: uid, Name: in.Name,
		LogicalPath: logical, Size: in.Size, Offset: 0, TmpPath: tmp.Name(),
	}
	if err := s.db.Create(&sess).Error; err != nil {
		os.Remove(tmp.Name())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.logTransferEvent(&models.TransferEvent{
		UserID: uid, Kind: "upload", Stage: "init", RefType: "upload_session",
		RefID: sess.ID, Name: in.Name, Path: logical, Bytes: in.Size,
	})
	c.JSON(http.StatusOK, gin.H{"upload_id": sess.ID, "offset": 0})
}

// handleUploadChunk appends one chunk at the given byte offset. WriteAt makes
// re-sent chunks idempotent (safe on resume/retry). Returns the new offset.
func (s *Server) handleUploadChunk(c *gin.Context) {
	uid := uidOf(c)
	id := c.Query("upload_id")
	offset, _ := strconv.ParseInt(c.Query("offset"), 10, 64)

	var sess models.UploadSession
	if err := s.db.Where("id = ? AND user_id = ?", id, uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	if offset > sess.Offset {
		c.JSON(http.StatusConflict, gin.H{"error": "offset gap", "offset": sess.Offset})
		return
	}
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Printf("[upload] chunk read error session=%s offset=%d: %v", id, offset, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取分片失败: " + err.Error()})
		return
	}
	f, err := os.OpenFile(sess.TmpPath, os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[upload] open temp failed session=%s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if _, err := f.WriteAt(data, offset); err != nil {
		f.Close()
		log.Printf("[upload] write temp failed session=%s offset=%d: %v", id, offset, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	f.Close()
	if end := offset + int64(len(data)); end > sess.Offset {
		sess.Offset = end
		s.db.Model(&sess).Update("offset", end)
	}
	s.logTransferEvent(&models.TransferEvent{
		UserID: uid, Kind: "upload", Stage: "chunk", RefType: "upload_session",
		RefID: sess.ID, Name: sess.Name, Path: sess.LogicalPath,
		OffsetStart: offset, OffsetEnd: sess.Offset, Bytes: int64(len(data)),
	})
	c.JSON(http.StatusOK, gin.H{"offset": sess.Offset})
}

// handleUploadStatus reports how many bytes have arrived, so a client can resume.
func (s *Server) handleUploadStatus(c *gin.Context) {
	uid := uidOf(c)
	id := c.Query("upload_id")
	var sess models.UploadSession
	if err := s.db.Where("id = ? AND user_id = ?", id, uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"offset": sess.Offset, "size": sess.Size})
}

// handleUploadFinish validates completeness and hands the temp file to the async
// job that pushes it to a Baidu storage node (server->Baidu leg).
func (s *Server) handleUploadFinish(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		UploadID string `json:"upload_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var sess models.UploadSession
	if err := s.db.Where("id = ? AND user_id = ?", in.UploadID, uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	if sess.Size > 0 && sess.Offset != sess.Size {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upload incomplete"})
		return
	}
	if s.temporaryAccessDenied(c, sess.LogicalPath) {
		return
	}
	acc, remote, remoteUUID, logical, release, err := s.resolveUploadTarget(c, sess.LogicalPath, sess.Offset)
	if err != nil {
		if release != nil {
			release()
		}
		log.Printf("[upload] finish resolve target failed session=%s path=%s: %v", in.UploadID, sess.LogicalPath, err)
		os.Remove(sess.TmpPath)
		s.db.Delete(&sess)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	jobID, err := s.jobs.SubmitUploadTo(uid, sess.TmpPath, logical, remote, remoteUUID, sess.Offset, acc)
	if err != nil {
		log.Printf("[upload] finish->submit failed session=%s path=%s: %v", in.UploadID, sess.LogicalPath, err)
		os.Remove(sess.TmpPath)
		s.db.Delete(&sess)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	log.Printf("[upload] finish ok session=%s path=%s job=%s bytes=%d", in.UploadID, sess.LogicalPath, jobID, sess.Offset)
	// The worker now owns the temp file; drop the session record.
	s.db.Delete(&sess)
	c.JSON(http.StatusOK, gin.H{"job_id": jobID})
}

// handleUploadCancel discards an in-progress session and its temp file.
func (s *Server) handleUploadCancel(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		UploadID string `json:"upload_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var sess models.UploadSession
	if err := s.db.Where("id = ? AND user_id = ?", in.UploadID, uid).First(&sess).Error; err == nil {
		os.Remove(sess.TmpPath)
		s.db.Delete(&sess)
		s.logTransferEvent(&models.TransferEvent{
			UserID: uid, Kind: "upload", Stage: "cancel", RefType: "upload_session",
			RefID: sess.ID, Name: sess.Name, Path: sess.LogicalPath,
		})
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
