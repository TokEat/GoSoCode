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
	"gosocode/internal/crypto"
	"gosocode/internal/keystore"
	"gosocode/internal/models"
)

// streamState is the in-process side of a session: its mutex serializes bitmap
// updates and (first phase, per design doc §9) the server->Baidu pushes of one
// session, while different sessions and body receives still run in parallel.
type streamState struct {
	mu sync.Mutex

	// partial slice assemblies for the sub-chunked browser->server leg
	// (?off=). Guarded by its own mutex so receives never wait behind mu,
	// which is held for the duration of a server->Baidu push.
	pmu     sync.Mutex
	partial map[int]*slicePartial
}

// slicePartial is one slice being assembled from sequential sub-chunks.
type slicePartial struct {
	buf     []byte
	next    int64 // bytes received so far; the only offset accepted next
	lastUse time.Time
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

// streamPartialJanitor drops slice assembly buffers that stopped receiving
// sub-chunks (browser gone mid-slice), so abandoned sessions don't pin
// window x slice_size of memory until the 24h session janitor runs.
func (s *Server) streamPartialJanitor() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-20 * time.Minute)
		s.streamMu.Lock()
		states := make([]*streamState, 0, len(s.streamStates))
		for _, st := range s.streamStates {
			states = append(states, st)
		}
		s.streamMu.Unlock()
		for _, st := range states {
			st.pmu.Lock()
			for seq, p := range st.partial {
				if p.lastUse.Before(cutoff) {
					delete(st.partial, seq)
				}
			}
			st.pmu.Unlock()
		}
	}
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
	sc := s.systemConfig()
	// Empty files (and disabled/full stream) fall back to store-and-forward.
	// Encrypted storage also forces the browser onto store-and-forward: the
	// streaming path hashes plaintext slices, but the server must only ever
	// push ciphertext to Baidu. Agents encrypt locally, so they may stream.
	if !s.cfg.StreamEnabled || size <= 0 ||
		(!agent && sc.EncryptStorage) ||
		(!agent && s.streamActive() >= s.cfg.StreamMaxConcurrent) {
		s.controlJSON(c, http.StatusOK, gin.H{"enabled": false})
		return
	}
	var acc *models.BaiduAccount
	if p := c.Query("path"); p != "" {
		a, _, _, err := s.resolveMount(p)
		if err != nil || (a.QuotaTotal > 0 && a.QuotaTotal-a.QuotaUsed < size) {
			s.controlJSON(c, http.StatusOK, gin.H{"enabled": false})
			return
		}
		acc = a
	} else {
		var err error
		acc, err = s.pool.PeekForSize(c.Request.Context(), size)
		if err != nil {
			s.controlJSON(c, http.StatusOK, gin.H{"enabled": false})
			return
		}
	}
	out := gin.H{
		"enabled":    true,
		"slice_size": baidu.SliceSizeForVip(acc.VipType),
		"node_id":    acc.ID,
		"node_tier":  acc.VipType,
		"window":     s.cfg.StreamWindow,
		"encrypt":    sc.EncryptStorage, // agents must encrypt slices locally
	}
	if agent && sc.EncryptStorage {
		// One key per storage node (double-stored server-side); the IV is fresh
		// per file. The agent encrypts with these before hashing.
		key, err := keystore.NodeKey(s.db, sc.MasterKey, s.cfg.KeysDir, acc.ID)
		if err != nil {
			s.controlJSON(c, http.StatusInternalServerError, gin.H{"error": "无法获取节点加密密钥: " + err.Error()})
			return
		}
		iv, err := crypto.NewIV()
		if err != nil {
			s.controlJSON(c, http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out["enc_key"] = hex.EncodeToString(key)
		out["enc_iv"] = hex.EncodeToString(iv)
	}
	s.controlJSON(c, http.StatusOK, out)
}

// streamInitReq is the shared request body of the browser and agent init
// endpoints.
type streamInitReq struct {
	Name      string   `json:"name" binding:"required"`
	Path      string   `json:"path"`
	Size      int64    `json:"size" binding:"required,gt=0"`
	SliceSize int64    `json:"slice_size" binding:"required,gt=0"`
	BlockList []string `json:"block_list" binding:"required,min=1"`
	// Agent-only: the per-file key/IV (hex) the client encrypted with. Stored
	// wrapped on the File record so server-proxied downloads can decrypt.
	EncKey string `json:"enc_key"`
	EncIV  string `json:"enc_iv"`
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
	if s.systemConfig().EncryptStorage {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "已启用加密存储，请使用普通上传", "fallback": true})
		return
	}
	if s.streamActive() >= s.cfg.StreamMaxConcurrent {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "流式上传并发已满", "fallback": true})
		return
	}
	in.EncKey, in.EncIV = "", "" // browser sessions never carry key material

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
	if in.EncKey != "" || in.EncIV != "" {
		key, kerr := hex.DecodeString(in.EncKey)
		ivb, verr := hex.DecodeString(in.EncIV)
		if kerr != nil || verr != nil || len(key) != 32 || len(ivb) != 16 {
			release()
			c.JSON(http.StatusBadRequest, gin.H{"error": "加密密钥格式无效"})
			return
		}
		mk := s.systemConfig().MasterKey
		wk, e1 := crypto.Wrap(mk, in.EncKey)
		wiv, e2 := crypto.Wrap(mk, in.EncIV)
		if e1 != nil || e2 != nil {
			release()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法保存加密密钥"})
			return
		}
		f.Encrypted, f.EncKey, f.EncIV = true, wk, wiv
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
	// With ?off= the slice arrives as small sequential sub-chunks assembled here:
	// one huge request rarely survives a flaky link, small ones are cheap to
	// retry, and a reconnect resumes from the acknowledged offset (409 expected).
	var data []byte
	if offStr := c.Query("off"); offStr != "" {
		off, err := strconv.ParseInt(offStr, 10, 64)
		if err != nil || off < 0 || off >= want {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid off"})
			return
		}
		if sess.DoneBitmap[seq] == '1' { // retry of an already-synced slice
			c.JSON(http.StatusOK, gin.H{"seq": seq, "done_count": sess.DoneCount, "total": sess.TotalSlices})
			return
		}
		chunk, err := io.ReadAll(io.LimitReader(c.Request.Body, want-off+1))
		if err != nil {
			log.Printf("[stream] slice read error session=%s seq=%d off=%d: %v", id, seq, off, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "读取分片失败: " + err.Error()})
			return
		}
		if off+int64(len(chunk)) > want {
			c.JSON(http.StatusBadRequest, gin.H{"error": "分片数据超长"})
			return
		}
		st := s.streamStateFor(sess.ID)
		st.pmu.Lock()
		if st.partial == nil {
			st.partial = map[int]*slicePartial{}
		}
		p := st.partial[seq]
		if p == nil {
			p = &slicePartial{buf: make([]byte, want)}
			st.partial[seq] = p
		}
		if off != p.next {
			expected := p.next
			st.pmu.Unlock()
			c.JSON(http.StatusConflict, gin.H{"error": "offset mismatch", "expected": expected})
			return
		}
		copy(p.buf[off:], chunk)
		p.next += int64(len(chunk))
		p.lastUse = time.Now()
		if p.next < want {
			received := p.next
			st.pmu.Unlock()
			c.JSON(http.StatusOK, gin.H{"seq": seq, "received": received, "partial": true})
			return
		}
		delete(st.partial, seq)
		st.pmu.Unlock()
		data = p.buf
	} else {
		data, err = io.ReadAll(io.LimitReader(c.Request.Body, sess.SliceSize+1))
		if err != nil {
			log.Printf("[stream] slice read error session=%s seq=%d: %v", id, seq, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "读取分片失败: " + err.Error()})
			return
		}
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
	if sess.Completed {
		c.JSON(http.StatusOK, gin.H{"file_id": sess.FileID, "already_completed": true})
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
	// Another finish request may have completed while this request waited for
	// the distributed path lock. Re-read to keep accounting exactly once.
	if err := s.db.First(&sess, "id = ?", sess.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream session not found"})
		return
	}
	if sess.Completed {
		c.JSON(http.StatusOK, gin.H{"file_id": sess.FileID, "already_completed": true})
		return
	}

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
		// create may have reached Baidu even when its response was lost or returned
		// an ambiguous errno. Reconcile against a newly modified file before
		// declaring failure so a successful upload is not retried from scratch.
		var ref *baiduListRef
		reconcileErr := streamRetry(3, "reconcile create "+sess.RemotePath, func() error {
			var e error
			ref, e = s.remoteItemByPath(c, acc, token, sess.RemotePath)
			return e
		})
		if reconcileErr == nil && ref != nil && !ref.IsDir && ref.Size == sess.Size &&
			ref.MTime >= sess.CreatedAt.Unix()-5 {
			fsID = ref.FsID
			log.Printf("[stream] create response ambiguous but remote file verified session=%s path=%s fsid=%d", sess.ID, sess.LogicalPath, fsID)
			err = nil
		}
	}
	if err != nil {
		log.Printf("[stream] finish create failed session=%s path=%s: %v", sess.ID, sess.LogicalPath, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	now := time.Now()
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.File{}).Where("id = ?", sess.FileID).
			Updates(map[string]any{"fs_id": fsID, "size": sess.Size, "status": "ready", "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.User{}).Where("id = ?", uid).
			UpdateColumn("bytes_up", gorm.Expr("bytes_up + ?", sess.Size)).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).Updates(map[string]any{
			"quota_used":   gorm.Expr("quota_used + ?", sess.Size),
			"bytes_up":     gorm.Expr("bytes_up + ?", sess.Size),
			"current_load": gorm.Expr("CASE WHEN current_load > 0 THEN current_load - 1 ELSE 0 END"),
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&models.TransferLog{
			UserID: uid, Kind: "upload", Name: path.Base(sess.LogicalPath),
			Path: sess.LogicalPath, Size: sess.Size, At: now,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&models.StreamSession{}).Where("id = ?", sess.ID).
			Updates(map[string]any{"completed": true, "updated_at": now}).Error
	}); err != nil {
		log.Printf("[stream] finish accounting transaction failed session=%s: %v", sess.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上传已到达云端，但本地记账失败，请重试"})
		return
	}
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
		if !sess.Completed {
			s.pool.Release(sess.BaiduAccountID)
			s.db.Delete(&models.File{}, sess.FileID)
		}
		s.db.Delete(&models.StreamSession{}, "id = ?", sess.ID)
		s.streamStateDrop(sess.ID)
		log.Printf("[stream] cancel session=%s path=%s", sess.ID, sess.LogicalPath)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
