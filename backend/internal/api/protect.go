// Password protection ("私密") for logical paths. An AccessRule on a file or
// directory means: only the rule owner passes freely; everyone else — admins
// included — must present the password to list the directory, download the
// file, or modify/delete the item. Owner and admins may view/manage the
// password itself.
package api

import (
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gosocode/internal/crypto"
	"gosocode/internal/models"
)

func isAdmin(c *gin.Context) bool {
	role, _ := c.Get("role")
	return role == "admin"
}

func isTemporary(c *gin.Context) bool {
	role, _ := c.Get("role")
	return role == "temporary"
}

const accessPasswordHeader = "X-Access-Password"

// presentedPassword returns the path-access password supplied by the client.
// It is only accepted via the X-Access-Password header so the secret never
// appears in URLs, proxy logs, or Referer headers.
func presentedPassword(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader(accessPasswordHeader))
}

// ruleFor returns the most specific AccessRule covering the path (the path
// itself or its nearest protected ancestor directory), or nil.
func (s *Server) ruleFor(logical string) *models.AccessRule {
	logical = cleanLogical(logical)
	if logical == "/" {
		return nil
	}
	paths := []string{logical}
	for p := path.Dir(logical); p != "/" && p != "."; p = path.Dir(p) {
		paths = append(paths, p)
	}
	var rules []models.AccessRule
	if err := s.db.Where("path IN ?", paths).Find(&rules).Error; err != nil || len(rules) == 0 {
		return nil
	}
	best := &rules[0]
	for i := range rules {
		if len(rules[i].Path) > len(best.Path) {
			best = &rules[i]
		}
	}
	return best
}

// temporaryAccessDenied checks whether a temporary user is authorized for the
// given logical path. Access is allowed when the path itself or any recursive
// parent directory has an active AccessGrant for this user.
func (s *Server) temporaryAccessDenied(c *gin.Context, logical string) bool {
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
	if count > 0 {
		return false
	}

	c.JSON(http.StatusForbidden, gin.H{
		"error":            "未授权访问：请联系管理员授权该路径",
		"temporary_denied": true,
	})
	return true
}

// accessDenied enforces an AccessRule on the path. When access is denied it
// writes the 403 response (with protected:true so the UI prompts for the
// password) and returns true. The password is taken from the `pwd` query
// parameter or the X-Access-Password header.
func (s *Server) accessDenied(c *gin.Context, logical string) bool {
	if s.temporaryAccessDenied(c, logical) {
		return true
	}
	rule := s.ruleFor(logical)
	if rule == nil {
		return false
	}
	// Only the owner is exempt. Admins can look the password up, but still have
	// to present it like everyone else.
	if rule.OwnerUserID == uidOf(c) {
		return false
	}
	pwd := presentedPassword(c)
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

// handleProtect creates or updates a password rule on a path.
// POST /api/files/protect {path, is_dir, password}
func (s *Server) handleProtect(c *gin.Context) {
	if isTemporary(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "临时用户不能设置访问密码"})
		return
	}
	uid := uidOf(c)
	var in struct {
		Path     string `json:"path" binding:"required"`
		IsDir    bool   `json:"is_dir"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.Password) < 4 || len(in.Password) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "访问密码长度需为 4-64 个字符"})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能对根目录设置密码"})
		return
	}
	enc, err := crypto.Wrap(s.systemConfig().MasterKey, in.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法保存访问密码"})
		return
	}

	var rule models.AccessRule
	if s.db.Where("path = ?", p).First(&rule).Error == nil {
		if rule.OwnerUserID != uid && !isAdmin(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": "只有设置者本人或管理员可以修改该密码"})
			return
		}
		rule.PasswordEnc, rule.IsDir, rule.UpdatedAt = enc, in.IsDir, time.Now()
		if err := s.db.Save(&rule).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		rule = models.AccessRule{
			Path: p, IsDir: in.IsDir, OwnerUserID: uid, OwnerName: usernameOf(c),
			PasswordEnc: enc, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := s.db.Create(&rule).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	s.audit(c, uid, usernameOf(c), "file.protect", p)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleUnprotect removes a password rule. Owner or admin only.
// POST /api/files/unprotect {path}
func (s *Server) handleUnprotect(c *gin.Context) {
	if isTemporary(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "临时用户不能取消访问密码"})
		return
	}
	uid := uidOf(c)
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	var rule models.AccessRule
	if err := s.db.Where("path = ?", p).First(&rule).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "该路径未设置密码"})
		return
	}
	if rule.OwnerUserID != uid && !isAdmin(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "只有设置者本人或管理员可以取消私密"})
		return
	}
	if err := s.db.Delete(&rule).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, uid, usernameOf(c), "file.unprotect", p)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleGetProtect reports a path's protection status. Owner/admin also get
// the plaintext password.
// GET /api/files/protect?path=
func (s *Server) handleGetProtect(c *gin.Context) {
	p := cleanLogical(c.Query("path"))
	if s.temporaryAccessDenied(c, p) {
		return
	}
	var rule models.AccessRule
	if err := s.db.Where("path = ?", p).First(&rule).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"protected": false})
		return
	}
	out := gin.H{"protected": true, "owner": rule.OwnerName, "is_dir": rule.IsDir}
	if rule.OwnerUserID == uidOf(c) || isAdmin(c) {
		plain, err := crypto.Unwrap(s.systemConfig().MasterKey, rule.PasswordEnc)
		if err == nil {
			out["password"] = plain
		}
	}
	c.JSON(http.StatusOK, out)
}

// moveAccessRules keeps protection attached to items when they are moved or
// renamed: the rule on src and any rules under src/ are rewritten to dst.
func (s *Server) moveAccessRules(src, dst string) {
	s.db.Model(&models.AccessRule{}).Where("path = ?", src).Update("path", dst)
	var rules []models.AccessRule
	s.db.Where("path LIKE ?", src+"/%").Find(&rules)
	for _, r := range rules {
		s.db.Model(&models.AccessRule{}).Where("id = ?", r.ID).
			Update("path", dst+r.Path[len(src):])
	}
}

// dropAccessRules removes rules for a deleted path and its subtree.
func (s *Server) dropAccessRules(p string) {
	s.db.Where("path = ? OR path LIKE ?", p, p+"/%").Delete(&models.AccessRule{})
}
