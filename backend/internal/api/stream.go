// Streaming (pipelined) upload: the browser pushes slices while the server
// forwards each one to Baidu (superfile2) immediately, so the browser->server
// and server->Baidu legs overlap instead of running serially. See
// docs/streaming-upload-sync.md.
//
// Flow: plan (slice size by node tier) -> client md5 pre-scan -> init
// (precreate, rapid-upload detection) -> slice xN (sliding window) -> finish
// (create). No full-file temp storage: peak memory per session is
// window x slice_size, bounded globally by StreamMaxConcurrent.

package api

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"gosocode/internal/baidu"
	"gosocode/internal/models"
)

// streamState is the in-process side of a session: its mutex serializes bitmap
// updates and (first phase, per design doc §9) the server->Baidu pushes of one
// session, while different sessions and body receives still run in parallel.
type streamState struct {
	mu sync.Mutex
}

func (s *Server) streamStateFor(id string) *streamState {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	st, ok := s.streamStates[id]
	if !ok {
		st = &streamState{}
		s.streamStates[id] = st
	}
	return st
}

func (s *Server) streamStateDrop(id string) {
	s.streamMu.Lock()
	delete(s.streamStates, id)
	s.streamMu.Unlock()
}

func (s *Server) streamActive() int {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return len(s.streamStates)
}

func sliceCount(size, sliceSize int64) int {
	return int((size + sliceSize - 1) / sliceSize)
}

func needSeqs(bitmap string) []int {
	out := []int{}
	for i := 0; i < len(bitmap); i++ {
		if bitmap[i] == '0' {
			out = append(out, i)
		}
	}
	return out
}

// streamRetry mirrors job.withRetry for the server->Baidu leg.
func streamRetry(attempts int, label string, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		log.Printf("[stream] %s attempt %d/%d failed: %v", label, i+1, attempts, err)
		if i < attempts-1 {
			time.Sleep(time.Duration(400*(1<<i)) * time.Millisecond)
		}
	}
	return err
}

// handleStreamPlan pre-selects a storage node so the client knows which slice
// size to hash with. Not a reservation: init re-picks and re-validates.
// GET /files/stream/plan?size=<bytes>&path=<logical>[&agent=1]
func (s *Server) handleStreamPlan(c *gin.Context) {
	size, _ := strconv.ParseInt(c.Query("size"), 10, 64)
	// Agent (CLI) sessions push slices to Baidu themselves, so the server-memory
	// concurrency cap does not apply to them.
	agent := c.Query("agent") == "1"
	// Empty files (and disabled/full stream) fall back to store-and-forward.
	if !s.cfg.StreamEnabled || size <= 0 || (!agent && s.streamActive() >= s.cfg.StreamMaxConcurrent) {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}
	var acc *models.BaiduAccount
	if p := c.Query("path"); p != "" {
		a, _, _, err := s.resolveMount(p)
		if err != nil || (a.QuotaTotal > 0 && a.QuotaTotal-a.QuotaUsed < size) {
			c.JSON(http.StatusOK, gin.H{"enabled": false})
			return
		}
		acc = a
	} else {
		var err error
		acc, err = s.pool.PeekForSize(c.Request.Context(), size)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"enabled": false})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":    true,
		"slice_size": baidu.SliceSizeForVip(acc.VipType),
		"node_id":    acc.ID,
		"node_tier":  acc.VipType,
		"window":     s.cfg.StreamWindow,
	})
}

// streamInitReq is the shared request body of the browser and agent init
// endpoints.
type streamInitReq struct {
	Name      string   `json:"name" binding:"required"`
	Path      string   `json:"path"`
	Size      int64    `json:"size" binding:"required,gt=0"`
	SliceSize int64    `json:"slice_size" binding:"required,gt=0"`
	BlockList []string `json:"block_list" binding:"required,min=1"`
}

// handleStreamInit picks the node for real, runs precreate (which also detects
// rapid-upload), and opens the session.
// POST /files/stream/init {name,path,size,slice_size,block_list}
func (s *Server) handleStreamInit(c *gin.Context) {
	var in streamInitReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !s.cfg.StreamEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "流式上传未启用", "fallback": true})
		return
	}
	if s.streamActive() >= s.cfg.StreamMaxConcurrent {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "流式上传并发已满", "fallback": true})
		return
	}

	sess, _, need, rapid, ok := s.openStreamSession(c, in)
	if !ok {
		return
	}
	s.streamStateFor(sess.ID) // register as active (counts toward the concurrency cap)
	c.JSON(http.StatusOK, gin.H{
		"session_id": sess.ID, "upload_id": sess.UploadID,
		"need_seqs": need, "rapid": rapid,
		"slice_size": sess.SliceSize, "total": sess.TotalSlices, "window": s.cfg.StreamWindow,
	})
}

// openStreamSession is the shared init core: node placement, precreate
// (rapid-upload detection), File + StreamSession records. On failure it writes
// the error response itself and returns ok=false.
func (s *Server) openStreamSession(c *gin.Context, in streamInitReq) (sess *models.StreamSession, token string, need []int, rapid bool, ok bool) {
	uid := uidOf(c)
	ctx := c.Request.Context()
	logical := cleanLogical(in.Path)
	if logical == "/" {
		logical = "/" + in.Name
	}
	acc, remote, logical, release, err := s.resolveUploadTarget(c, logical, in.Size)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}

	// The node chosen now may differ from plan's; if its tier implies another
	// slice size the client's md5 list is useless — ask it to re-hash (§8).
	sliceSize := baidu.SliceSizeForVip(acc.VipType)
	if sliceSize != in.SliceSize {
		release()
		c.JSON(http.StatusConflict, gin.H{"error": "分片大小已变化，请按新分片重新计算", "slice_size": sliceSize})
		return
	}
	total := sliceCount(in.Size, sliceSize)
	if len(in.BlockList) != total {
		release()
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("block_list 数量不符: got %d want %d", len(in.BlockList), total)})
		return
	}

	token, err = s.pool.Token(ctx, acc)
	if err != nil {
		release()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	acctKey := "acct:" + itoa(acc.ID)

	var returnType int
	var uploadID string
	var missing []int
	err = streamRetry(3, "precreate "+remote, func() error {
		if e := s.coord.Limiter.Wait(ctx, acctKey); e != nil {
			return e
		}
		pre, e := s.client.Precreate(ctx, token, remote, in.Size, in.BlockList)
		if e != nil {
			return e
		}
		returnType, uploadID, missing = pre.ReturnType, pre.UploadID, pre.BlockList
		return nil
	})
	if err != nil {
		release()
		log.Printf("[stream] init precreate failed user=%d path=%s: %v", uid, logical, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	rapid = returnType == 2
	bitmap := strings.Repeat("0", total)
	doneCount := 0
	if rapid {
		bitmap = strings.Repeat("1", total)
		doneCount = total
	}

	f := models.File{
		OwnerUserID: uid, BaiduAccountID: acc.ID,
		LogicalPath: logical, RemotePath: remote,
		Size: in.Size, Status: "uploading",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.db.Create(&f).Error; err != nil {
		release()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	blocks, _ := json.Marshal(in.BlockList)
	sess = &models.StreamSession{
		ID: uuid.NewString(), UserID: uid, BaiduAccountID: acc.ID, FileID: f.ID,
		Name: in.Name, LogicalPath: logical, RemotePath: remote,
		Size: in.Size, SliceSize: sliceSize, TotalSlices: total,
		UploadID: uploadID, BlockList: string(blocks),
		DoneBitmap: bitmap, DoneCount: doneCount,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.db.Create(sess).Error; err != nil {
		release()
		s.db.Delete(&models.File{}, f.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	need = needSeqs(bitmap)
	if !rapid && len(missing) > 0 && len(missing) < total {
		need = missing // Baidu told us a subset is already there
	}
	log.Printf("[stream] init session=%s user=%d path=%s size=%d slices=%d slice_size=%d rapid=%v",
		sess.ID, uid, logical, in.Size, total, sliceSize, rapid)
	return sess, token, need, rapid, true
}

// handleStreamSlice receives one raw slice and synchronously forwards it to
// Baidu. Responding 200 therefore means "this slice is cloud-synced", which is
// what lets the client display a real cloud-sync speed.
// PUT /files/stream/slice?session=<id>&seq=<n>  (body: raw slice bytes)
func (s *Server) handleStreamSlice(c *gin.Context) {
	uid := uidOf(c)
	id := c.Query("session")
	seq, err := strconv.Atoi(c.Query("seq"))
	if err != nil || seq < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid seq"})
		return
	}

	var sess models.StreamSession
	if err := s.db.Where("id = ? AND user_id = ?", id, uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream session not found"})
		return
	}
	if seq >= sess.TotalSlices {
		c.JSON(http.StatusBadRequest, gin.H{"error": "seq out of range"})
		return
	}

	// Expected byte length of this slice (only the last one may be shorter).
	want := sess.SliceSize
	if seq == sess.TotalSlices-1 {
		want = sess.Size - int64(sess.TotalSlices-1)*sess.SliceSize
	}

	// Receiving the body is the browser->server leg; it runs outside the session
	// lock so it overlaps with another slice's server->Baidu push (the pipeline).
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, sess.SliceSize+1))
	if err != nil {
		log.Printf("[stream] slice read error session=%s seq=%d: %v", id, seq, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取分片失败: " + err.Error()})
		return
	}
	if int64(len(data)) != want {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("分片大小不符: got %d want %d", len(data), want)})
		return
	}
	var blockList []string
	_ = json.Unmarshal([]byte(sess.BlockList), &blockList)
	sum := md5.Sum(data)
	if got := hex.EncodeToString(sum[:]); got != blockList[seq] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据校验未通过，请重试"})
		return
	}

	st := s.streamStateFor(sess.ID)
	st.mu.Lock()
	defer st.mu.Unlock()

	// Re-read under the lock: another in-flight request may have advanced state.
	if err := s.db.First(&sess, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream session not found"})
		return
	}
	if sess.DoneBitmap[seq] == '1' { // duplicate (retry landed twice): idempotent OK
		c.JSON(http.StatusOK, gin.H{"seq": seq, "done_count": sess.DoneCount, "total": sess.TotalSlices})
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
	acctKey := "acct:" + itoa(acc.ID)
	err = streamRetry(4, fmt.Sprintf("slice seq=%d path=%s", seq, sess.RemotePath), func() error {
		if e := s.coord.Limiter.Wait(ctx, acctKey); e != nil {
			return e
		}
		return s.client.UploadChunk(ctx, token, sess.RemotePath, sess.UploadID, seq, data)
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "云端同步失败: " + err.Error()})
		return
	}
	// live meter counts as bytes actually move, so the realtime chart shows
	// throughput during the transfer (lifetime totals stay on finish/DB).
	s.meter.AddUp(int64(len(data)))

	bm := []byte(sess.DoneBitmap)
	bm[seq] = '1'
	sess.DoneBitmap = string(bm)
	sess.DoneCount++
	s.db.Model(&models.StreamSession{}).Where("id = ?", sess.ID).Updates(map[string]any{
		"done_bitmap": sess.DoneBitmap, "done_count": sess.DoneCount, "updated_at": time.Now(),
	})
	c.JSON(http.StatusOK, gin.H{"seq": seq, "done_count": sess.DoneCount, "total": sess.TotalSlices})
}

// handleStreamStatus reports which slices are still missing (resume support).
// GET /files/stream/status?session=<id>
func (s *Server) handleStreamStatus(c *gin.Context) {
	uid := uidOf(c)
	var sess models.StreamSession
	if err := s.db.Where("id = ? AND user_id = ?", c.Query("session"), uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream session not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"need_seqs": needSeqs(sess.DoneBitmap), "done_count": sess.DoneCount,
		"total": sess.TotalSlices, "slice_size": sess.SliceSize, "size": sess.Size,
	})
}

// handleStreamFinish merges the slices (create). By the time this returns the
// file is fully live in the cloud — no separate "云端合并中" phase.
// POST /files/stream/finish {session_id}
func (s *Server) handleStreamFinish(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		SessionID string `json:"session_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var sess models.StreamSession
	if err := s.db.Where("id = ? AND user_id = ?", in.SessionID, uid).First(&sess).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream session not found"})
		return
	}
	if sess.DoneCount < sess.TotalSlices {
		c.JSON(http.StatusConflict, gin.H{"error": "仍有分片未同步", "need_seqs": needSeqs(sess.DoneBitmap)})
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

	// Serialize create() on the same node path across instances (same rule as
	// the store-and-forward worker).
	unlock, err := s.coord.Locker.Lock(ctx, "path:"+sess.RemotePath, 5*time.Minute)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	defer unlock()

	var blockList []string
	_ = json.Unmarshal([]byte(sess.BlockList), &blockList)
	acctKey := "acct:" + itoa(acc.ID)
	var fsID uint64
	err = streamRetry(3, "create "+sess.RemotePath, func() error {
		if e := s.coord.Limiter.Wait(ctx, acctKey); e != nil {
			return e
		}
		res, e := s.client.Create(ctx, token, sess.RemotePath, sess.Size, sess.UploadID, blockList)
		if e != nil {
			return e
		}
		fsID = res.FsID
		return nil
	})
	if err != nil {
		log.Printf("[stream] finish create failed session=%s path=%s: %v", sess.ID, sess.LogicalPath, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	s.db.Model(&models.File{}).Where("id = ?", sess.FileID).
		Updates(map[string]any{"fs_id": fsID, "size": sess.Size, "status": "ready", "updated_at": time.Now()})
	s.pool.AddUsed(acc.ID, sess.Size)

	s.db.Model(&models.User{}).Where("id = ?", uid).UpdateColumn("bytes_up", gorm.Expr("bytes_up + ?", sess.Size))
	s.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).UpdateColumn("bytes_up", gorm.Expr("bytes_up + ?", sess.Size))
	s.db.Create(&models.TransferLog{
		UserID: uid, Kind: "upload", Name: path.Base(sess.LogicalPath),
		Path: sess.LogicalPath, Size: sess.Size, At: time.Now(),
	})

	s.pool.Release(acc.ID)
	s.db.Delete(&models.StreamSession{}, "id = ?", sess.ID)
	s.streamStateDrop(sess.ID)

	log.Printf("[stream] finish ok session=%s path=%s bytes=%d fsid=%d", sess.ID, sess.LogicalPath, sess.Size, fsID)
	c.JSON(http.StatusOK, gin.H{"file_id": sess.FileID})
}

// handleStreamCancel aborts the session: frees the node's load slot and drops
// the placeholder File record. Already-pushed slices simply expire at Baidu
// with the upload_id.
// POST /files/stream/cancel {session_id}
func (s *Server) handleStreamCancel(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		SessionID string `json:"session_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var sess models.StreamSession
	if err := s.db.Where("id = ? AND user_id = ?", in.SessionID, uid).First(&sess).Error; err == nil {
		s.pool.Release(sess.BaiduAccountID)
		s.db.Delete(&models.File{}, sess.FileID)
		s.db.Delete(&models.StreamSession{}, "id = ?", sess.ID)
		s.streamStateDrop(sess.ID)
		log.Printf("[stream] cancel session=%s path=%s", sess.ID, sess.LogicalPath)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
