// Agent (CLI client) control plane. A trusted local client transfers file
// bytes directly against Baidu; the server keeps the control plane only:
// node placement, precreate/create, metadata, sessions and accounting.
//
// Upload:  agent init (returns uploadid + node token) -> agent pushes
//          superfile2 itself -> reports progress -> regular stream finish.
// Download: agent resolves {dlink, token} here, fetches from Baidu directly
//          (it can set UA pan.baidu.com), and reports transferred bytes.
//
// Security note: these endpoints hand a node's access_token to the caller,
// which equals full control of that Baidu account. They are meant for
// self-hosted / trusted deployments and can be disabled with GSC_AGENT_ENABLED=0.
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gosocode/internal/models"
)

// agentGuard rejects agent calls when the feature is disabled.
func (s *Server) agentGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.cfg.AgentEnabled {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "客户端直连已被管理员禁用"})
			return
		}
		c.Next()
	}
}

// handleAgentUploadInit opens a streaming session for a direct-to-Baidu upload.
// Same body as /files/stream/init; additionally returns the node access token
// and remote path so the agent can call superfile2 itself.
// POST /api/agent/upload/init
func (s *Server) handleAgentUploadInit(c *gin.Context) {
	var in streamInitReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !s.cfg.StreamEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "流式上传未启用", "fallback": true})
		return
	}
	// No streamActive cap: agent sessions consume no server memory, so they are
	// not registered in streamStates either.
	sess, token, need, rapid, ok := s.openStreamSession(c, in)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session_id": sess.ID, "upload_id": sess.UploadID,
		"need_seqs": need, "rapid": rapid,
		"slice_size": sess.SliceSize, "total": sess.TotalSlices, "window": s.cfg.AgentWindow,
		"access_token": token, "remote_path": sess.RemotePath,
	})
}

// handleAgentUploadSession rebuilds an agent's local state after a client
// restart: everything needed to keep pushing slices for an open session.
// GET /api/agent/upload/session?session_id=<id>
func (s *Server) handleAgentUploadSession(c *gin.Context) {
	uid := uidOf(c)
	var sess models.StreamSession
	if err := s.db.Where("id = ? AND user_id = ?", c.Query("session_id"), uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream session not found"})
		return
	}
	ctx := c.Request.Context()
	acc, err := s.pool.Get(ctx, sess.BaiduAccountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session_id": sess.ID, "upload_id": sess.UploadID,
		"need_seqs": needSeqs(sess.DoneBitmap), "slice_size": sess.SliceSize,
		"total": sess.TotalSlices, "size": sess.Size, "block_list": sess.BlockList,
		"access_token": token, "remote_path": sess.RemotePath,
	})
}

// handleAgentUploadProgress marks slices the agent has pushed to Baidu as
// cloud-synced, keeping the server-side bitmap authoritative for finish/resume,
// and feeds the live traffic meter.
// POST /api/agent/upload/progress {session_id, seqs, bytes}
func (s *Server) handleAgentUploadProgress(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		SessionID string `json:"session_id" binding:"required"`
		Seqs      []int  `json:"seqs" binding:"required,min=1"`
		Bytes     int64  `json:"bytes"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Serialize the read-modify-write of the bitmap across concurrent slice
	// reports of the same or different sessions (cheap: tiny critical section).
	s.agentMu.Lock()
	defer s.agentMu.Unlock()

	var sess models.StreamSession
	if err := s.db.Where("id = ? AND user_id = ?", in.SessionID, uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream session not found"})
		return
	}
	bm := []byte(sess.DoneBitmap)
	changed := false
	for _, seq := range in.Seqs {
		if seq < 0 || seq >= sess.TotalSlices {
			c.JSON(http.StatusBadRequest, gin.H{"error": "seq out of range"})
			return
		}
		if bm[seq] == '0' {
			bm[seq] = '1'
			sess.DoneCount++
			changed = true
		}
	}
	if changed {
		s.db.Model(&models.StreamSession{}).Where("id = ?", sess.ID).Updates(map[string]any{
			"done_bitmap": string(bm), "done_count": sess.DoneCount, "updated_at": time.Now(),
		})
	}
	if in.Bytes > 0 {
		s.meter.AddUp(in.Bytes)
	}
	c.JSON(http.StatusOK, gin.H{"done_count": sess.DoneCount, "total": sess.TotalSlices})
}

// handleAgentDownloadResolve hands the agent what it needs to download a file
// straight from Baidu: the temporary dlink plus the node token (dlink requires
// both the token and UA pan.baidu.com, which a CLI can set).
// GET /api/agent/download/resolve?id=<file id>
func (s *Server) handleAgentDownloadResolve(c *gin.Context) {
	id, _ := strconv.Atoi(c.Query("id"))
	var f models.File
	// shared filesystem: any authenticated user may fetch any file.
	if err := s.db.First(&f, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if f.Status != "ready" {
		c.JSON(http.StatusConflict, gin.H{"error": "file not ready"})
		return
	}
	ctx := c.Request.Context()
	acc, err := s.pool.Get(ctx, f.BaiduAccountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	metas, err := s.client.FileMetas(ctx, token, []uint64{f.FsID})
	if err != nil || len(metas) == 0 || metas[0].Dlink == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法从云端获取下载地址: " + errString(err)})
		return
	}
	name := metas[0].Filename
	if name == "" {
		name = "file-" + itoa(f.ID)
	}
	size := metas[0].Size
	if size == 0 {
		size = f.Size
	}
	c.JSON(http.StatusOK, gin.H{
		"dlink": metas[0].Dlink, "access_token": token,
		"name": name, "size": size, "ua": "pan.baidu.com", "file_id": f.ID,
	})
}

// handleAgentDownloadReport records bytes the agent moved directly from Baidu,
// keeping traffic accounting (live meter + lifetime totals) in sync even though
// the bytes never touched the server.
// POST /api/agent/download/report {file_id, bytes}
func (s *Server) handleAgentDownloadReport(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		FileID uint  `json:"file_id" binding:"required"`
		Bytes  int64 `json:"bytes" binding:"required,gt=0"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var f models.File
	if err := s.db.First(&f, in.FileID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	s.meter.AddDown(in.Bytes)
	s.db.Model(&models.User{}).Where("id = ?", uid).
		UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", in.Bytes))
	s.db.Model(&models.BaiduAccount{}).Where("id = ?", f.BaiduAccountID).
		UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", in.Bytes))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
