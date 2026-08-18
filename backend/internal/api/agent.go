// Agent (CLI client) control plane. A trusted local client transfers file
// bytes directly against Baidu; the server keeps the control plane only:
// node placement, precreate/create, metadata, sessions and accounting.
//
// Upload:  agent init (returns uploadid + node token) -> agent pushes
//
//	superfile2 itself -> reports progress -> regular stream finish.
//
// Download: agent resolves {dlink, token} here, fetches from Baidu directly
//
//	(it can set UA pan.baidu.com), and reports transferred bytes.
//
// Security note: these endpoints hand a node's access_token to the caller,
// which equals full control of that Baidu account. They are meant for
// self-hosted / trusted deployments and can be disabled with GSC_AGENT_ENABLED=0.
package api

import (
	"encoding/hex"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gosocode/internal/baidu"
	"gosocode/internal/manifest"
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
	if s.systemConfig().EncryptStorage && in.EncKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "系统已启用加密存储，当前客户端版本不支持，请更新客户端"})
		return
	}
	logical := cleanLogical(in.Path)
	if logical == "/" {
		logical = "/" + in.Name
	}
	if s.temporaryAccessDenied(c, logical) {
		return
	}
	// No streamActive cap: agent sessions consume no server memory, so they are
	// not registered in streamStates either.
	sess, token, need, rapid, ok := s.openStreamSession(c, in)
	if !ok {
		return
	}
	s.agentJSON(c, http.StatusOK, gin.H{
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
	s.agentJSON(c, http.StatusOK, gin.H{
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
	// A client may idempotently re-send/report the same partseq after a lost
	// response. Count bytes only when the authoritative bitmap actually changes.
	if changed && in.Bytes > 0 {
		s.meter.AddUp(in.Bytes)
		s.logTransferEvent(&models.TransferEvent{
			UserID: uid, Kind: "upload", Stage: "progress", RefType: "agent",
			RefID: sess.ID, Name: sess.Name, Path: sess.LogicalPath,
			AccountID: sess.BaiduAccountID, Bytes: in.Bytes,
		})
	}
	s.agentJSON(c, http.StatusOK, gin.H{"done_count": sess.DoneCount, "total": sess.TotalSlices})
}

// handleAgentDownloadResolve hands the agent what it needs to download a file
// straight from Baidu: the temporary dlink plus the node token (dlink requires
// both the token and UA pan.baidu.com, which a CLI can set).
// GET /api/agent/download/resolve?path=<logical path>
// The path form is used for live Baidu listings, whose item ID is a remote
// fs_id rather than a row in the local files table. The id form remains for
// compatibility with older metadata-backed clients.
func (s *Server) handleAgentDownloadResolve(c *gin.Context) {
	ctx := c.Request.Context()
	var (
		acc          *models.BaiduAccount
		fsID         uint64
		fallbackName string
		fallbackSize int64
		fileID       uint
		resolvedPath string
		encFile      *models.File
		err          error
	)

	if logical := c.Query("path"); logical != "" {
		var remotePath, clean string
		acc, remotePath, clean, err = s.resolveMount(logical)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		resolvedPath = clean
		if path.Base(clean) == manifest.FileName {
			c.JSON(http.StatusForbidden, gin.H{"error": "系统文件不可访问"})
			return
		}
		if s.accessDenied(c, clean) {
			return
		}
		remotePath = s.resolveRemote(acc, clean, remotePath)
		var token string
		token, err = s.pool.Token(ctx, acc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var ref *baiduListRef
		refErr := streamRetry(5, "agent resolve list "+clean, func() error {
			var e error
			ref, e = s.remoteItemByPath(c, acc, token, remotePath)
			return e
		})
		if refErr != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
			return
		}
		if ref.IsDir {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不能下载文件夹"})
			return
		}
		fsID, fallbackName, fallbackSize = ref.FsID, ref.Name, ref.Size
		if fallbackName == "" {
			fallbackName = path.Base(clean)
		}
		encFile = s.fileByRemote(acc.ID, remotePath)
	} else {
		id, parseErr := strconv.ParseUint(c.Query("id"), 10, 64)
		if parseErr != nil || id == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请指定文件路径"})
			return
		}
		var f models.File
		// shared filesystem: any authenticated user may fetch any file.
		if err = s.db.First(&f, uint(id)).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if f.Status != "ready" {
			c.JSON(http.StatusConflict, gin.H{"error": "file not ready"})
			return
		}
		if s.accessDenied(c, f.LogicalPath) {
			return
		}
		acc, err = s.pool.Get(ctx, f.BaiduAccountID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		fsID, fallbackName, fallbackSize, fileID = f.FsID, path.Base(f.LogicalPath), f.Size, f.ID
		resolvedPath = f.LogicalPath
		ff := f
		encFile = &ff
	}

	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var metas []baidu.FileMeta
	err = streamRetry(5, "agent resolve metas "+fallbackName, func() error {
		if e := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); e != nil {
			return e
		}
		var e error
		metas, e = s.client.FileMetas(ctx, token, []uint64{fsID})
		return e
	})
	if err != nil || len(metas) == 0 || metas[0].Dlink == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法从云端获取下载地址: " + errString(err)})
		return
	}
	name := metas[0].Filename
	if encFile != nil && encFile.RemoteUUID != "" { // cloud-side name is an opaque uuid
		name = path.Base(encFile.LogicalPath)
	}
	if name == "" {
		name = fallbackName
	}
	if name == "" {
		name = "file-" + strconv.FormatUint(fsID, 10)
	}
	size := metas[0].Size
	if size == 0 {
		size = fallbackSize
	}
	out := gin.H{
		"dlink": metas[0].Dlink, "access_token": token,
		"name": name, "size": size, "ua": "pan.baidu.com",
		"file_id": fileID, "node_id": acc.ID, "vip_type": acc.VipType,
	}
	// Encrypted at rest: hand the per-file key to the (trusted) agent so it can
	// decrypt the ranged segments it fetches straight from Baidu.
	if encFile != nil && encFile.Encrypted {
		key, iv, kerr := s.fileEncKeys(encFile)
		if kerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取文件加密密钥"})
			return
		}
		out["encrypted"] = true
		out["enc_key"] = hex.EncodeToString(key)
		out["enc_iv"] = hex.EncodeToString(iv)
	}
	s.logTransferEvent(&models.TransferEvent{
		UserID: uidOf(c), Kind: "download", Stage: "start", RefType: "agent",
		RefID: strconv.FormatUint(uint64(fileID), 10), Name: name,
		Path: resolvedPath, AccountID: acc.ID, Bytes: size,
	})
	s.agentJSON(c, http.StatusOK, out)
}

// handleAgentDownloadReport records bytes the agent moved directly from Baidu,
// keeping traffic accounting (live meter + lifetime totals) in sync even though
// the bytes never touched the server.
// POST /api/agent/download/report {node_id, bytes}
// file_id is retained as a fallback for older clients.
func (s *Server) handleAgentDownloadReport(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		FileID uint  `json:"file_id"`
		NodeID uint  `json:"node_id"`
		Bytes  int64 `json:"bytes" binding:"required,gt=0"`
		Done   bool  `json:"done"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	nodeID := in.NodeID
	var f models.File
	if nodeID == 0 && in.FileID != 0 {
		if err := s.db.First(&f, in.FileID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		nodeID = f.BaiduAccountID
	}
	if nodeID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing node_id"})
		return
	}
	var acc models.BaiduAccount
	if err := s.db.First(&acc, nodeID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "storage node not found"})
		return
	}
	s.meter.AddDown(in.Bytes)
	s.db.Model(&models.User{}).Where("id = ?", uid).
		UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", in.Bytes))
	s.db.Model(&models.BaiduAccount{}).Where("id = ?", nodeID).
		UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", in.Bytes))

	name := path.Base(f.LogicalPath)
	if name == "" || name == "." {
		name = "unknown"
	}
	if in.Done {
		s.db.Create(&models.TransferLog{
			UserID: uid, Kind: "download", Name: name,
			Path: f.LogicalPath, Size: in.Bytes, At: time.Now(),
		})
		s.logTransferEvent(&models.TransferEvent{
			UserID: uid, Kind: "download", Stage: "finish", RefType: "agent",
			RefID: strconv.FormatUint(uint64(in.FileID), 10), Name: name,
			Path: f.LogicalPath, AccountID: nodeID, Bytes: in.Bytes,
		})
	} else {
		s.logTransferEvent(&models.TransferEvent{
			UserID: uid, Kind: "download", Stage: "progress", RefType: "agent",
			RefID: strconv.FormatUint(uint64(in.FileID), 10), Name: name,
			Path: f.LogicalPath, AccountID: nodeID, Bytes: in.Bytes,
		})
	}
	s.agentJSON(c, http.StatusOK, gin.H{"ok": true})
}
