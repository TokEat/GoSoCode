package api

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gosocode/internal/crypto"
	"gosocode/internal/models"
)

// shareLinksEnabled reports whether users may create public share links.
func (s *Server) shareLinksEnabled() bool {
	return s.systemConfig().ShareLinksEnabled
}

// randHex returns n random bytes encoded as lowercase hex.
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func parseExpiresIn(v string) (time.Duration, bool) {
	switch v {
	case "1h":
		return time.Hour, true
	case "1d":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "never":
		return 0, true
	default:
		return 0, false
	}
}

// shareAccessDenied enforces AccessRule on a public share visitor. The creator
// (owner) is never the visitor here, so a password is always required when the
// path or any ancestor is protected.
func (s *Server) shareAccessDenied(c *gin.Context, logical string) bool {
	rule := s.ruleFor(logical)
	if rule == nil {
		return false
	}
	pwd := c.Query("pwd")
	if pwd == "" {
		pwd = c.GetHeader("X-Access-Password")
	}
	if pwd != "" {
		plain, err := crypto.Unwrap(s.systemConfig().MasterKey, rule.PasswordEnc)
		if err == nil && plain == pwd {
			return false
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "访问密码错误", "protected": true, "owner": rule.OwnerName})
		return true
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "该内容为私密内容，需要访问密码", "protected": true, "owner": rule.OwnerName})
	return true
}

// handleCreateShare creates a public share link for a file or directory.
// POST /api/shares { id?, path?, expires_in: "1h"|"1d"|"7d"|"never" }
func (s *Server) handleCreateShare(c *gin.Context) {
	if !s.shareLinksEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "分享链接功能已被管理员关闭"})
		return
	}
	var in struct {
		ID        uint   `json:"id"`
		Path      string `json:"path"`
		ExpiresIn string `json:"expires_in"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	dur, ok := parseExpiresIn(in.ExpiresIn)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的过期时间"})
		return
	}

	var f models.File
	if in.ID != 0 {
		if err := s.db.First(&f, in.ID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
			return
		}
		in.Path = f.LogicalPath
	} else if in.Path != "" {
		clean := cleanLogical(in.Path)
		if err := s.db.Where("logical_path = ? AND status = ?", clean, "ready").First(&f).Error; err != nil {
			// Directory shares may not have a File row yet; accept directories
			// that resolve to a mount and are not the root.
			_, _, clean2, err := s.resolveMount(clean)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "目标不存在"})
				return
			}
			in.Path = clean2
			// access check below uses in.Path
		}
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请指定 id 或 path"})
		return
	}

	target := cleanLogical(in.Path)
	if target == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能分享根目录"})
		return
	}

	// Creator must be able to access the target themselves. Reuse the normal
	// access check so owners are exempt and non-owners must provide the password.
	if s.accessDenied(c, target) {
		return
	}

	isDir := f.IsDir
	if f.ID == 0 {
		// path-only share: treat as directory unless a file row exists
		isDir = true
	}

	token, err := randHex(32)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法生成分享令牌"})
		return
	}

	now := time.Now()
	var expires *time.Time
	if dur > 0 {
		t := now.Add(dur)
		expires = &t
	}

	link := models.ShareLink{
		Token:     token,
		FileID:    f.ID,
		Path:      target,
		IsDir:     isDir,
		CreatedBy: uidOf(c),
		ExpiresAt: expires,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.Create(&link).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存分享链接失败"})
		return
	}

	s.audit(c, uidOf(c), usernameOf(c), "share.create", "path="+target+" token="+token)

	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	url := scheme + "://" + c.Request.Host + "/s/" + token
	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"url":        url,
		"expires_at": expires,
		"is_dir":     isDir,
	})
}

// handleGetShare returns metadata for a public share link.
// GET /api/shares/:token
func (s *Server) handleGetShare(c *gin.Context) {
	link, ok := s.loadShareLink(c)
	if !ok {
		return
	}
	var size int64
	var name string
	if link.IsDir {
		name = path.Base(link.Path)
		var total int64
		s.db.Model(&models.File{}).
			Where("logical_path LIKE ? AND status = ? AND is_dir = ?", link.Path+"/%", "ready", false).
			Select("COALESCE(SUM(size),0)").Scan(&total)
		size = total
	} else {
		var f models.File
		if err := s.db.First(&f, link.FileID).Error; err == nil {
			name = path.Base(f.LogicalPath)
			size = f.Size
		} else {
			name = path.Base(link.Path)
		}
	}

	protected := s.ruleFor(link.Path) != nil
	out := gin.H{
		"name":       name,
		"size":       size,
		"is_dir":     link.IsDir,
		"expires_at": link.ExpiresAt,
		"protected":  protected,
	}
	if protected {
		rule := s.ruleFor(link.Path)
		if rule != nil {
			out["owner"] = rule.OwnerName
		}
	}
	c.JSON(http.StatusOK, out)
}

// handleDownloadShare downloads the file or directory behind a public share.
// GET /api/shares/:token/download?pwd=...
func (s *Server) handleDownloadShare(c *gin.Context) {
	link, ok := s.loadShareLink(c)
	if !ok {
		return
	}
	if s.shareAccessDenied(c, link.Path) {
		return
	}

	if link.IsDir {
		s.serveZip(c, link.Path, &link)
		return
	}

	var f models.File
	if err := s.db.First(&f, link.FileID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		return
	}
	if f.Status != "ready" {
		c.JSON(http.StatusConflict, gin.H{"error": "文件未就绪"})
		return
	}

	s.logTransferEvent(&models.TransferEvent{
		UserID: link.CreatedBy, Kind: "download", Stage: "start", RefType: "share",
		RefID: strconv.FormatUint(uint64(link.ID), 10), Name: path.Base(f.LogicalPath), Path: f.LogicalPath,
		AccountID: f.BaiduAccountID, Bytes: f.Size,
	})
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
	n := s.proxyDownload(c, link.CreatedBy, acc, token, f.FsID, path.Base(f.LogicalPath), "share="+link.Token, &f)
	s.logTransferEvent(&models.TransferEvent{
		UserID: link.CreatedBy, Kind: "download", Stage: "finish", RefType: "share",
		RefID: strconv.FormatUint(uint64(link.ID), 10), Name: path.Base(f.LogicalPath), Path: f.LogicalPath,
		AccountID: f.BaiduAccountID, Bytes: n,
	})
	if n > 0 {
		s.db.Model(&models.ShareLink{}).Where("id = ?", link.ID).
			UpdateColumn("downloads", gorm.Expr("downloads + ?", 1))
	}
}

// handleZipDownload lets an authenticated user download a directory as zip.
// GET /api/files/zip?path=<logical>&pwd=...
func (s *Server) handleZipDownload(c *gin.Context) {
	target := cleanLogical(c.Query("path"))
	if target == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能下载根目录"})
		return
	}
	if s.accessDenied(c, target) {
		return
	}
	s.serveZip(c, target, nil)
}

// loadShareLink validates and returns a non-expired, non-revoked share link.
func (s *Server) loadShareLink(c *gin.Context) (models.ShareLink, bool) {
	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "分享令牌缺失"})
		return models.ShareLink{}, false
	}
	var link models.ShareLink
	if err := s.db.Where("token = ?", token).First(&link).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "分享链接不存在或已失效"})
		return models.ShareLink{}, false
	}
	if link.Revoked {
		c.JSON(http.StatusGone, gin.H{"error": "分享链接已撤销"})
		return models.ShareLink{}, false
	}
	if link.ExpiresAt != nil && link.ExpiresAt.Before(time.Now()) {
		c.JSON(http.StatusGone, gin.H{"error": "分享链接已过期"})
		return models.ShareLink{}, false
	}
	return link, true
}

// serveZip streams a zip archive of all ready files under targetPath. When share
// is non-nil the download is attributed to the share creator; otherwise it is
// attributed to the authenticated caller (JWT downloads).
func (s *Server) serveZip(c *gin.Context, targetPath string, share *models.ShareLink) {
	uid := uint(0)
	if share != nil {
		uid = share.CreatedBy
	} else {
		uid = uidOf(c)
	}

	// Resolve the mount so we know the target is valid; the actual files may
	// live on multiple accounts if they were moved across mounts metadata-only,
	// so we fetch rows and group by BaiduAccountID.
	_, _, clean, err := s.resolveMount(targetPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	targetPath = clean

	var files []models.File
	if err := s.db.Where("logical_path LIKE ? AND status = ? AND is_dir = ?", targetPath+"/%", "ready", false).
		Order("logical_path asc").Find(&files).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法枚举目录内容"})
		return
	}
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目录为空"})
		return
	}

	// Validate that visitor has access to every file. Because ruleFor returns the
	// nearest protected ancestor, providing the target directory password covers
	// the whole subtree.
	for i := range files {
		if share != nil {
			if s.shareAccessDenied(c, files[i].LogicalPath) {
				return
			}
		} else {
			if s.accessDenied(c, files[i].LogicalPath) {
				return
			}
		}
		if err := s.ensureDecryptable(&files[i]); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
	}

	s.logTransferEvent(&models.TransferEvent{
		UserID: uid, Kind: "download", Stage: "start", RefType: "share",
		Name: path.Base(targetPath), Path: targetPath,
		Bytes: totalFileSize(files),
	})

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, path.Base(targetPath)))
	c.Status(http.StatusOK)

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	flusher, _ := c.Writer.(http.Flusher)
	var totalBytes int64
	tokens := make(map[uint]string)

	for i := range files {
		f := files[i]
		entryName := strings.TrimPrefix(f.LogicalPath, targetPath+"/")
		if entryName == "" {
			continue
		}

		ctx := c.Request.Context()
		acc, err := s.pool.Get(ctx, f.BaiduAccountID)
		if err != nil {
			log.Printf("[zip] pool get failed file=%d node=%d: %v", f.ID, f.BaiduAccountID, err)
			continue
		}
		token, ok := tokens[f.BaiduAccountID]
		if !ok {
			token, err = s.pool.Token(ctx, acc)
			if err != nil {
				log.Printf("[zip] token failed node=%d: %v", f.BaiduAccountID, err)
				continue
			}
			tokens[f.BaiduAccountID] = token
		}

		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			log.Printf("[zip] limiter failed file=%d: %v", f.ID, err)
			continue
		}
		metas, merr := s.client.FileMetas(ctx, token, []uint64{f.FsID})
		if merr != nil || len(metas) == 0 || metas[0].Dlink == "" {
			log.Printf("[zip] dlink resolve failed file=%d: %v", f.ID, merr)
			continue
		}
		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			log.Printf("[zip] limiter failed file=%d: %v", f.ID, err)
			continue
		}
		resp, err := s.client.OpenDownload(ctx, token, metas[0].Dlink, "")
		if err != nil {
			log.Printf("[zip] open download failed file=%d: %v", f.ID, err)
			continue
		}
		if resp.StatusCode >= 400 {
			log.Printf("[zip] upstream error file=%d status=%d", f.ID, resp.StatusCode)
			resp.Body.Close()
			continue
		}

		body, err := s.decryptedBody(resp, &f)
		if err != nil {
			log.Printf("[zip] decrypt init failed file=%d: %v", f.ID, err)
			resp.Body.Close()
			continue
		}
		if body, err = s.guardedDownloadBody(body, &f, 0); err != nil {
			log.Printf("[zip] ciphertext refused file=%d: %v", f.ID, err)
			resp.Body.Close()
			return
		}

		header := &zip.FileHeader{
			Name:   entryName,
			Method: zip.Deflate,
		}
		header.SetModTime(time.Now())
		w, err := zw.CreateHeader(header)
		if err != nil {
			log.Printf("[zip] create header failed file=%d: %v", f.ID, err)
			resp.Body.Close()
			continue
		}
		n, err := io.Copy(w, body)
		resp.Body.Close()
		if err != nil {
			log.Printf("[zip] copy failed file=%d after %d bytes: %v", f.ID, n, err)
		}
		if n > 0 {
			totalBytes += n
			s.meter.AddDown(n)
			s.db.Model(&models.User{}).Where("id = ?", uid).
				UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
			s.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).
				UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	if err := zw.Close(); err != nil {
		log.Printf("[zip] close writer failed: %v", err)
	}

	s.logTransferEvent(&models.TransferEvent{
		UserID: uid, Kind: "download", Stage: "finish", RefType: "share",
		Name: path.Base(targetPath), Path: targetPath,
		Bytes: totalBytes,
	})
	if share != nil && totalBytes > 0 {
		s.db.Model(&models.ShareLink{}).Where("id = ?", share.ID).
			UpdateColumn("downloads", gorm.Expr("downloads + ?", 1))
	}
}

func totalFileSize(files []models.File) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}
