package api

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"gosocode/internal/crypto"
	"gosocode/internal/models"
)

const (
	maxNoteBodyBytes   = 256 * 1024
	maxNoteImageBytes  = 8 << 20
	maxImagesPerNote   = 40
	noteImageURLPrefix = "/api/notes/images/"
)

var noteImageRef = regexp.MustCompile(`\]\(/api/notes/images/(\d+)\)`)

func (s *Server) notesDir() string {
	if s.cfg != nil && strings.TrimSpace(s.cfg.NotesDir) != "" {
		return s.cfg.NotesDir
	}
	return "gsc-notes"
}

func (s *Server) handleGetNote(c *gin.Context) {
	p := cleanLogical(c.Query("path"))
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能为根目录添加备注"})
		return
	}
	if s.accessDenied(c, p) {
		return
	}
	var n models.PathNote
	err := s.db.Where("path = ?", p).First(&n).Error
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"path":    p,
			"is_dir":  false,
			"body":    "",
			"exists":  false,
			"updated": "",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path":       n.Path,
		"is_dir":     n.IsDir,
		"body":       n.Body,
		"exists":     true,
		"updated":    n.UpdatedAt,
		"updated_by": n.UpdatedBy,
	})
}

func (s *Server) handleIndexNotes(c *gin.Context) {
	dir := cleanLogical(c.Query("dir"))
	if s.accessDenied(c, dir) {
		return
	}
	var notes []models.PathNote
	if dir == "/" {
		s.db.Where("path LIKE ? AND path NOT LIKE ?", "/%", "/%/%").Find(&notes)
	} else {
		s.db.Where("path = ? OR path LIKE ?", dir, dir+"/%").Find(&notes)
	}
	paths := make([]string, 0, len(notes))
	items := make([]gin.H, 0, len(notes))
	for _, n := range notes {
		if n.Path == dir || path.Dir(n.Path) == dir {
			if s.accessDeniedNoWrite(c, n.Path) {
				continue
			}
			paths = append(paths, n.Path)
			items = append(items, gin.H{"path": n.Path, "body": n.Body})
		}
	}
	c.JSON(http.StatusOK, gin.H{"paths": paths, "notes": items})
}

func (s *Server) handlePutNote(c *gin.Context) {
	var in struct {
		Path  string `json:"path" binding:"required"`
		IsDir bool   `json:"is_dir"`
		Body  string `json:"body"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能为根目录添加备注"})
		return
	}
	if s.accessDenied(c, p) {
		return
	}
	if strings.ContainsRune(in.Body, 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注内容不合法"})
		return
	}
	if len(in.Body) > maxNoteBodyBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注过长，请控制在 256 KB 以内"})
		return
	}
	if !utf8.ValidString(in.Body) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注不是合法文本"})
		return
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		s.dropPathNotes(p, false)
		c.JSON(http.StatusOK, gin.H{"ok": true, "exists": false, "path": p})
		return
	}
	var n models.PathNote
	err := s.db.Where("path = ?", p).First(&n).Error
	n.Path = p
	n.IsDir = in.IsDir
	n.Body = in.Body
	n.UpdatedBy = uidOf(c)
	if err != nil {
		if err := s.db.Create(&n).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else if err := s.db.Save(&n).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.gcNoteImages(p, in.Body)
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"exists":     true,
		"path":       n.Path,
		"is_dir":     n.IsDir,
		"body":       n.Body,
		"updated":    n.UpdatedAt,
		"updated_by": n.UpdatedBy,
	})
}

func (s *Server) handleDeleteNote(c *gin.Context) {
	p := cleanLogical(c.Query("path"))
	if p == "/" || p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效路径"})
		return
	}
	if s.accessDenied(c, p) {
		return
	}
	s.dropPathNotes(p, false)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleUploadNoteImage(c *gin.Context) {
	p := cleanLogical(c.PostForm("path"))
	if p == "/" || p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效路径"})
		return
	}
	if s.accessDenied(c, p) {
		return
	}
	var existing int64
	s.db.Model(&models.NoteImage{}).Where("note_path = ?", p).Count(&existing)
	if existing >= maxImagesPerNote {
		c.JSON(http.StatusBadRequest, gin.H{"error": "单条备注最多 40 张图片"})
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择图片"})
		return
	}
	if fh.Size > maxNoteImageBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "图片不能超过 8 MB"})
		return
	}
	src, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer src.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(src, head)
	head = head[:n]
	mime, ext, ok := sniffImage(head)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持 JPEG、PNG、GIF、WebP 图片"})
		return
	}
	if err := os.MkdirAll(s.notesDir(), 0o700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	disk := uuid.NewString() + "." + ext
	dstPath := filepath.Join(s.notesDir(), disk)
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	written, copyErr := io.Copy(dst, io.MultiReader(bytes.NewReader(head), src))
	_ = dst.Close()
	if copyErr != nil {
		_ = os.Remove(dstPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": copyErr.Error()})
		return
	}
	if written > maxNoteImageBytes {
		_ = os.Remove(dstPath)
		c.JSON(http.StatusBadRequest, gin.H{"error": "图片不能超过 8 MB"})
		return
	}
	img := models.NoteImage{
		NotePath:  p,
		Name:      path.Base(fh.Filename),
		MIME:      mime,
		Size:      written,
		DiskName:  disk,
		CreatedBy: uidOf(c),
	}
	if err := s.db.Create(&img).Error; err != nil {
		_ = os.Remove(dstPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":   img.ID,
		"url":  noteImageURLPrefix + strconv.FormatUint(uint64(img.ID), 10),
		"name": img.Name,
		"mime": img.MIME,
		"size": img.Size,
	})
}

func (s *Server) handleGetNoteImage(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效图片"})
		return
	}
	var img models.NoteImage
	if err := s.db.First(&img, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "图片不存在"})
		return
	}
	if s.accessDenied(c, img.NotePath) {
		return
	}
	fp := filepath.Join(s.notesDir(), img.DiskName)
	// Prevent path escape if DiskName were ever tainted.
	if filepath.Base(img.DiskName) != img.DiskName {
		c.JSON(http.StatusNotFound, gin.H{"error": "图片不存在"})
		return
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "图片文件缺失"})
		return
	}
	c.Header("Cache-Control", "private, max-age=86400")
	c.Data(http.StatusOK, img.MIME, data)
}

func (s *Server) handleDeleteNoteImage(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效图片"})
		return
	}
	var img models.NoteImage
	if err := s.db.First(&img, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "图片不存在"})
		return
	}
	if s.accessDenied(c, img.NotePath) {
		return
	}
	s.removeNoteImage(&img)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func sniffImage(head []byte) (mime, ext string, ok bool) {
	if len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff {
		return "image/jpeg", "jpg", true
	}
	if len(head) >= 8 && string(head[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png", "png", true
	}
	if len(head) >= 6 {
		sig := string(head[:6])
		if sig == "GIF87a" || sig == "GIF89a" {
			return "image/gif", "gif", true
		}
	}
	if len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		return "image/webp", "webp", true
	}
	return "", "", false
}

func referencedImageIDs(body string) map[uint]bool {
	out := map[uint]bool{}
	for _, m := range noteImageRef.FindAllStringSubmatch(body, -1) {
		id, err := strconv.Atoi(m[1])
		if err == nil && id > 0 {
			out[uint(id)] = true
		}
	}
	return out
}

func (s *Server) gcNoteImages(notePath, body string) {
	keep := referencedImageIDs(body)
	var imgs []models.NoteImage
	s.db.Where("note_path = ?", notePath).Find(&imgs)
	for i := range imgs {
		if !keep[imgs[i].ID] {
			s.removeNoteImage(&imgs[i])
		}
	}
}

func (s *Server) removeNoteImage(img *models.NoteImage) {
	if img.DiskName != "" && filepath.Base(img.DiskName) == img.DiskName {
		_ = os.Remove(filepath.Join(s.notesDir(), img.DiskName))
	}
	s.db.Delete(&models.NoteImage{}, img.ID)
}

// movePathNotes rewrites note and image paths when a file or folder is moved
// or renamed, including every descendant note under a moved directory.
func (s *Server) movePathNotes(src, dst string) {
	src, dst = cleanLogical(src), cleanLogical(dst)
	if src == "" || dst == "" || src == dst {
		return
	}
	s.db.Model(&models.PathNote{}).Where("path = ?", src).Update("path", dst)
	s.db.Model(&models.NoteImage{}).Where("note_path = ?", src).Update("note_path", dst)
	var notes []models.PathNote
	s.db.Where("path LIKE ?", src+"/%").Find(&notes)
	for _, n := range notes {
		np := dst + n.Path[len(src):]
		s.db.Model(&models.PathNote{}).Where("id = ?", n.ID).Update("path", np)
	}
	var imgs []models.NoteImage
	s.db.Where("note_path LIKE ?", src+"/%").Find(&imgs)
	for _, img := range imgs {
		np := dst + img.NotePath[len(src):]
		s.db.Model(&models.NoteImage{}).Where("id = ?", img.ID).Update("note_path", np)
	}
}

// dropPathNotes removes the note on p. When recursive is true (folder
// delete), every descendant note and its images are removed as well.
func (s *Server) dropPathNotes(p string, recursive bool) {
	p = cleanLogical(p)
	if p == "" || p == "/" {
		return
	}
	var notes []models.PathNote
	var imgs []models.NoteImage
	if recursive {
		s.db.Where("path = ? OR path LIKE ?", p, p+"/%").Find(&notes)
		s.db.Where("note_path = ? OR note_path LIKE ?", p, p+"/%").Find(&imgs)
	} else {
		s.db.Where("path = ?", p).Find(&notes)
		s.db.Where("note_path = ?", p).Find(&imgs)
	}
	for i := range imgs {
		s.removeNoteImage(&imgs[i])
	}
	if recursive {
		s.db.Where("path = ? OR path LIKE ?", p, p+"/%").Delete(&models.PathNote{})
	} else {
		s.db.Where("path = ?", p).Delete(&models.PathNote{})
	}
}

func (s *Server) temporaryAccessDeniedNoWrite(c *gin.Context, logical string) bool {
	role, _ := c.Get("role")
	if role != "temporary" {
		return false
	}
	logical = cleanLogical(logical)
	paths := []string{logical}
	for p := path.Dir(logical); p != "/" && p != "."; p = path.Dir(p) {
		paths = append(paths, p)
	}
	var count int64
	now := time.Now()
	s.db.Model(&models.AccessGrant{}).
		Where("user_id = ? AND path IN ? AND (expires_at IS NULL OR expires_at > ?)", uidOf(c), paths, now).
		Count(&count)
	return count == 0
}

func presentedPassword(c *gin.Context) string {
	if p := strings.TrimSpace(c.Query("pwd")); p != "" {
		return p
	}
	return strings.TrimSpace(c.GetHeader("X-Access-Password"))
}

func (s *Server) accessDeniedNoWrite(c *gin.Context, logical string) bool {
	logical = cleanLogical(logical)
	if s.temporaryAccessDeniedNoWrite(c, logical) {
		return true
	}
	rule := s.ruleFor(logical)
	if rule == nil {
		return false
	}
	if rule.OwnerUserID == uidOf(c) {
		return false
	}
	pw := presentedPassword(c)
	if pw == "" {
		return true
	}
	plain, err := crypto.Unwrap(s.systemConfig().MasterKey, rule.PasswordEnc)
	return err != nil || plain != pw
}
