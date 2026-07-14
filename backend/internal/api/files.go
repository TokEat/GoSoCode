package api

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gosocode/internal/models"
)

// cleanLogical normalizes a user-supplied logical path to a canonical absolute
// form (defeats "../" and duplicate slashes). Root resolves to "/".
func cleanLogical(p string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.TrimSpace(p), "/"))
}

func errString(err error) string {
	if err == nil {
		return "直链为空"
	}
	return err.Error()
}

func mountLabel(acc models.BaiduAccount) string {
	name := strings.TrimSpace(acc.MountName)
	if name == "" {
		name = strings.TrimSpace(acc.Nickname)
	}
	if name == "" {
		name = "百度网盘"
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", "\x00", "")
	name = strings.TrimSpace(replacer.Replace(name))
	if name == "" {
		name = "百度网盘"
	}
	return name
}

func mountName(acc models.BaiduAccount) string {
	return mountLabel(acc)
}

func normalizeMountPath(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" {
		return "/", nil
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", fmt.Errorf("挂载目录包含无效字符")
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", fmt.Errorf("挂载目录不能包含 ..")
		}
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	clean := path.Clean(raw)
	if len(clean) > 1024 {
		return "", fmt.Errorf("挂载目录不能超过 1024 个字符")
	}
	return clean, nil
}

func nodeMountPath(acc models.BaiduAccount) string {
	p, err := normalizeMountPath(acc.MountPath)
	if err != nil {
		return "/"
	}
	return p
}

func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func (s *Server) activeNodes() ([]models.BaiduAccount, error) {
	var nodes []models.BaiduAccount
	err := s.db.Where("status = ?", "active").Order("id asc").Find(&nodes).Error
	return nodes, err
}

func (s *Server) resolveMount(logical string) (*models.BaiduAccount, string, string, error) {
	p := cleanLogical(logical)
	if p == "/" {
		return nil, "", p, fmt.Errorf("请选择一个挂载节点")
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	nodes, err := s.activeNodes()
	if err != nil {
		return nil, "", p, err
	}
	var matched *models.BaiduAccount
	for _, n := range nodes {
		mount := mountName(n)
		if parts[0] != mount {
			continue
		}
		if matched != nil {
			return nil, "", p, fmt.Errorf("挂载名称重复，请由管理员重命名节点")
		}
		acc := n
		matched = &acc
	}
	if matched != nil {
		remote := nodeMountPath(*matched)
		if len(parts) > 1 {
			remote = path.Join(remote, strings.Join(parts[1:], "/"))
			base := nodeMountPath(*matched)
			if base != "/" && remote != base && !strings.HasPrefix(remote, base+"/") {
				return nil, "", p, fmt.Errorf("路径超出挂载目录")
			}
		}
		return matched, remote, p, nil
	}
	return nil, "", p, fmt.Errorf("挂载节点不存在")
}

func (s *Server) resolveUploadTarget(ctx *gin.Context, logical string, size int64) (*models.BaiduAccount, string, string, func(), error) {
	acc, remote, clean, err := s.resolveMount(logical)
	if err != nil {
		return nil, "", clean, nil, err
	}
	reserved, err := s.pool.PickByIDForSize(ctx.Request.Context(), acc.ID, size)
	if err != nil {
		return nil, "", clean, nil, err
	}
	return reserved, remote, clean, func() { s.pool.Release(reserved.ID) }, nil
}

// handleListFiles lists the current logical directory. The logical root exposes
// mounted Baidu accounts as folders; inside a mount, listing is read live from
// that Baidu account's netdisk root instead of the local metadata table.
func (s *Server) handleListFiles(c *gin.Context) {
	logical := cleanLogical(c.DefaultQuery("path", "/"))
	if logical == "/" {
		nodes, err := s.activeNodes()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out := make([]gin.H, 0, len(nodes))
		for _, n := range nodes {
			m := mountName(n)
			out = append(out, gin.H{
				"id": 0, "path": "/" + m, "size": 0, "status": "ready",
				"created_at": n.CreatedAt, "modified": n.UpdatedAt, "is_dir": true,
				"owner": "百度网盘", "node_id": n.ID, "mount": true,
			})
		}
		c.JSON(http.StatusOK, gin.H{"files": out})
		return
	}

	acc, remoteDir, _, err := s.resolveMount(logical)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	items, err := s.client.ListDir(ctx, token, remoteDir)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	mount := strings.TrimSuffix(logical, "/")
	out := make([]gin.H, 0, len(items))
	for _, it := range items {
		name := it.ServerFilename
		if name == "" {
			name = path.Base(it.Path)
		}
		childPath := mount + "/" + name
		mt := unixTime(it.ServerMTime)
		ct := unixTime(it.ServerCTime)
		out = append(out, gin.H{
			"id": it.FsID, "path": childPath, "size": it.Size,
			"status": "ready", "created_at": ct, "modified": mt,
			"is_dir": it.IsDir == 1, "owner": acc.Nickname,
			"node_id": acc.ID, "remote_path": it.Path,
			"download_url": fmt.Sprintf("/api/files/remote-download?path=%s", pathEscape(childPath)),
		})
	}
	c.JSON(http.StatusOK, gin.H{"files": out})
}

func pathEscape(p string) string {
	return url.QueryEscape(p)
}

func (s *Server) remoteItemByPath(ctx *gin.Context, acc *models.BaiduAccount, token, remotePath string) (*baiduListRef, error) {
	dir, name := path.Dir(remotePath), path.Base(remotePath)
	if dir == "." {
		dir = "/"
	}
	if err := s.coord.Limiter.Wait(ctx.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
		return nil, err
	}
	items, err := s.client.ListDir(ctx.Request.Context(), token, dir)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.Path == remotePath || it.ServerFilename == name {
			return &baiduListRef{
				FsID: it.FsID, Name: it.ServerFilename, Size: it.Size,
				IsDir: it.IsDir == 1, MTime: it.ServerMTime,
			}, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

type baiduListRef struct {
	FsID  uint64
	Name  string
	Size  int64
	IsDir bool
	MTime int64
}

func (s *Server) proxyDownload(c *gin.Context, acc *models.BaiduAccount, token string, fsID uint64, fallbackName, logLabel string) {
	ctx := c.Request.Context()
	var resp *http.Response
	filename := fallbackName
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		metas, merr := s.client.FileMetas(ctx, token, []uint64{fsID})
		if merr != nil || len(metas) == 0 || metas[0].Dlink == "" {
			log.Printf("[download] resolve dlink failed %s node=%d: %v", logLabel, acc.ID, merr)
			c.JSON(http.StatusBadGateway, gin.H{"error": "无法从云端获取下载地址: " + errString(merr)})
			return
		}
		if metas[0].Filename != "" {
			filename = metas[0].Filename
		}
		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		resp, err = s.client.OpenDownload(ctx, token, metas[0].Dlink, c.GetHeader("Range"))
		if err != nil {
			log.Printf("[download] open upstream failed %s node=%d: %v", logLabel, acc.ID, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		if resp.StatusCode < 400 {
			break
		}
		resp.Body.Close()
		resp = nil
		log.Printf("[download] upstream HTTP error %s node=%d attempt=%d, re-resolving dlink", logLabel, acc.ID, attempt+1)
	}
	if resp == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "云端拒绝下载（直链失效），请重试"})
		return
	}
	defer resp.Body.Close()

	if filename == "" {
		filename = fmt.Sprintf("file-%d", fsID)
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Accept-Ranges", "bytes")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		c.Header("Content-Range", cr)
	}
	c.Status(resp.StatusCode)

	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 256*1024)
	var n int64
	var cerr error
	for {
		rn, rerr := resp.Body.Read(buf)
		if rn > 0 {
			if _, werr := c.Writer.Write(buf[:rn]); werr != nil {
				cerr = werr
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			n += int64(rn)
			s.meter.AddDown(int64(rn))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cerr = rerr
			break
		}
	}
	if cerr != nil {
		log.Printf("[download] stream interrupted %s node=%d after %d bytes: %v", logLabel, acc.ID, n, cerr)
	}
	if n > 0 {
		s.db.Model(&models.User{}).Where("id = ?", uidOf(c)).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
		s.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
	}
}

func (s *Server) handleRemoteDownload(c *gin.Context) {
	logical := c.Query("path")
	acc, remotePath, clean, err := s.resolveMount(logical)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	token, err := s.pool.Token(c.Request.Context(), acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ref, err := s.remoteItemByPath(c, acc, token, remotePath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		return
	}
	if ref.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能下载文件夹"})
		return
	}
	s.proxyDownload(c, acc, token, ref.FsID, ref.Name, "path="+clean)
}

// handleDownload streams a file's bytes through the platform (proxy mode), so
// the raw Baidu dlink/token never reaches the client. Range is forwarded to
// support resumable/multi-threaded downloads.
func (s *Server) handleDownload(c *gin.Context) {
	uid := uidOf(c)
	id, _ := strconv.Atoi(c.Param("id"))

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

	// Resolve the dlink and open the upstream stream. dlinks are temporary
	// (~8h) and can also be invalidated early; on a 4xx from the CDN we
	// re-resolve once and retry, so a stale cached dlink never fails the user.
	var resp *http.Response
	filename := ""
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		metas, merr := s.client.FileMetas(ctx, token, []uint64{f.FsID})
		if merr != nil || len(metas) == 0 || metas[0].Dlink == "" {
			log.Printf("[download] resolve dlink failed file=%d node=%d: %v", f.ID, acc.ID, merr)
			c.JSON(http.StatusBadGateway, gin.H{"error": "无法从云端获取下载地址: " + errString(merr)})
			return
		}
		filename = metas[0].Filename

		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		resp, err = s.client.OpenDownload(ctx, token, metas[0].Dlink, c.GetHeader("Range"))
		if err != nil {
			log.Printf("[download] open upstream failed file=%d node=%d: %v", f.ID, acc.ID, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		if resp.StatusCode < 400 {
			break
		}
		resp.Body.Close()
		resp = nil
		log.Printf("[download] upstream HTTP error file=%d node=%d attempt=%d, re-resolving dlink", f.ID, acc.ID, attempt+1)
	}
	if resp == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "云端拒绝下载（直链失效），请重试"})
		return
	}
	defer resp.Body.Close()

	if filename == "" {
		filename = fmt.Sprintf("file-%d", f.ID)
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Accept-Ranges", "bytes")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		c.Header("Content-Range", cr)
	}
	c.Status(resp.StatusCode)

	// Relay cloud->client with a small buffer and an explicit Flush after every
	// write: nothing accumulates server-side, so the client receives bytes at
	// the same pace the cloud delivers them, and a slow client back-pressures
	// the cloud read (read and write alternate on the same buffer).
	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 256*1024)
	var n int64
	var cerr error
	for {
		rn, rerr := resp.Body.Read(buf)
		if rn > 0 {
			if _, werr := c.Writer.Write(buf[:rn]); werr != nil {
				cerr = werr
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			n += int64(rn)
			s.meter.AddDown(int64(rn)) // live meter: realtime chart sees the flow
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cerr = rerr
			break
		}
	}
	if cerr != nil {
		// Client disconnect or upstream drop mid-stream; the client will resume
		// via Range. Log for visibility.
		log.Printf("[download] stream interrupted file=%d node=%d after %d bytes: %v", f.ID, acc.ID, n, cerr)
	}

	// Traffic accounting for the download (live meter already fed in the loop).
	if n > 0 {
		s.db.Model(&models.User{}).Where("id = ?", uid).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
		s.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
	}
}

// handleDeleteFile removes a file from its storage node and marks it deleted.
func (s *Server) handleDeleteFile(c *gin.Context) {
	uid := uidOf(c)
	id, _ := strconv.Atoi(c.Param("id"))
	var f models.File
	if err := s.db.First(&f, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	ctx := c.Request.Context()
	acc, err := s.pool.Get(ctx, f.BaiduAccountID)
	if err == nil {
		if token, terr := s.pool.Token(ctx, acc); terr == nil {
			_ = s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID))
			// best-effort remote delete; metadata is authoritative either way
			_ = s.client.Delete(ctx, token, []string{f.RemotePath})
			s.pool.AddUsed(acc.ID, -f.Size)
		}
	}
	s.db.Model(&f).Update("status", "deleted")
	s.audit(c, uid, usernameOf(c), "file.delete", f.LogicalPath)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleRenameFile changes only the logical (user-facing) path; the bytes stay
// on the same node. Keeps the metadata layer authoritative.
func (s *Server) handleRenameFile(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var f models.File
	if err := s.db.First(&f, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	s.db.Model(&f).Update("logical_path", in.Path)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleDeletePath(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能删除根目录"})
		return
	}
	acc, remotePath, _, err := s.resolveMount(p)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	token, err := s.pool.Token(c.Request.Context(), acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if err := s.client.Delete(c.Request.Context(), token, []string{remotePath}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	s.db.Model(&models.File{}).Where("(logical_path = ? OR logical_path LIKE ?) AND status != ?", p, p+"/%", "deleted").
		Update("status", "deleted")
	s.audit(c, uid, usernameOf(c), "file.delete", p)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleCancelJob marks a still-pending job cancelled. Running transfers are
// left to finish (the worker owns them); this covers queued items.
func (s *Server) handleCancelJob(c *gin.Context) {
	uid := uidOf(c)
	id := c.Param("id")
	res := s.db.Model(&models.Job{}).
		Where("id = ? AND user_id = ? AND status = ?", id, uid, "pending").
		Updates(map[string]any{"status": "failed", "error": "cancelled by user"})
	if res.RowsAffected == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "job not cancellable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleListJobs(c *gin.Context) {
	uid := uidOf(c)
	var jobs []models.Job
	s.db.Where("user_id = ?", uid).Order("created_at desc").Limit(100).Find(&jobs)
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}

// handleStorage reports the aggregate capacity of the shared filesystem across
// all active storage nodes. Used for the capacity bar and pre-upload checks.
func (s *Server) handleStorage(c *gin.Context) {
	var res struct{ Total, Used int64 }
	s.db.Model(&models.BaiduAccount{}).Where("status = ?", "active").
		Select("COALESCE(SUM(quota_total),0) as total, COALESCE(SUM(quota_used),0) as used").Scan(&res)
	free := res.Total - res.Used
	if free < 0 {
		free = 0
	}
	c.JSON(http.StatusOK, gin.H{"total": res.Total, "used": res.Used, "free": free})
}

// handleCreateFolder creates a metadata-only (empty) directory record so folders
// can exist before any file is placed in them.
func (s *Server) handleCreateFolder(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先进入一个挂载节点"})
		return
	}
	acc, remotePath, _, err := s.resolveMount(p)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	token, err := s.pool.Token(c.Request.Context(), acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if err := s.client.MakeDir(c.Request.Context(), token, remotePath); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, uid, usernameOf(c), "folder.create", p)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleMove relocates a file or a whole folder subtree by rewriting logical
// paths (metadata-only; bytes stay on their nodes). Powers cut/paste + rename.
func (s *Server) handleMove(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Src string `json:"src" binding:"required"`
		Dst string `json:"dst" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, dst := cleanLogical(in.Src), cleanLogical(in.Dst)
	if src == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
		return
	}
	if dst == src {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	if strings.HasPrefix(dst+"/", src+"/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能移动到自身或子目录"})
		return
	}
	var n int64
	s.db.Model(&models.File{}).Where("logical_path = ? AND status != ?", dst, "deleted").Count(&n)
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "目标已存在同名项"})
		return
	}
	srcAcc, srcRemote, _, err := s.resolveMount(src)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	dstAcc, dstRemote, _, err := s.resolveMount(dst)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if srcAcc.ID != dstAcc.ID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "暂不支持跨挂载节点移动"})
		return
	}
	token, err := s.pool.Token(c.Request.Context(), srcAcc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(srcAcc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if err := s.client.Move(c.Request.Context(), token, srcRemote, dstRemote); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// Move the item itself (file or folder record).
	s.db.Model(&models.File{}).Where("logical_path = ? AND status != ?", src, "deleted").
		Update("logical_path", dst)
	// Move descendants by prefix rewrite.
	var kids []models.File
	s.db.Where("logical_path LIKE ? AND status != ?", src+"/%", "deleted").Find(&kids)
	for _, k := range kids {
		np := dst + strings.TrimPrefix(k.LogicalPath, src)
		s.db.Model(&models.File{}).Where("id = ?", k.ID).Update("logical_path", np)
	}
	s.audit(c, uid, usernameOf(c), "move", src+" -> "+dst)
	c.JSON(http.StatusOK, gin.H{"ok": true, "moved": len(kids) + 1})
}

// handleDeleteFolder removes a folder and everything under it (best-effort remote
// deletes for the real files), marking all affected records deleted.
func (s *Server) handleDeleteFolder(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}
	if acc, remotePath, _, err := s.resolveMount(p); err == nil {
		token, err := s.pool.Token(c.Request.Context(), acc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		if err := s.client.Delete(c.Request.Context(), token, []string{remotePath}); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		s.db.Model(&models.File{}).Where("(logical_path = ? OR logical_path LIKE ?) AND status != ?", p, p+"/%", "deleted").
			Update("status", "deleted")
		s.audit(c, uid, usernameOf(c), "folder.delete", p)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	var items []models.File
	s.db.Where("(logical_path = ? OR logical_path LIKE ?) AND status != ?", p, p+"/%", "deleted").Find(&items)
	ctx := c.Request.Context()
	for _, f := range items {
		if !f.IsDir && f.RemotePath != "" {
			if acc, err := s.pool.Get(ctx, f.BaiduAccountID); err == nil {
				if token, terr := s.pool.Token(ctx, acc); terr == nil {
					_ = s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID))
					_ = s.client.Delete(ctx, token, []string{f.RemotePath})
					s.pool.AddUsed(acc.ID, -f.Size)
				}
			}
		}
		s.db.Model(&models.File{}).Where("id = ?", f.ID).Update("status", "deleted")
	}
	s.audit(c, uid, usernameOf(c), "folder.delete", p)
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(items)})
}
