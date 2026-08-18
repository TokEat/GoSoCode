package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"gosocode/internal/models"
)

// handleListAccessGrants lists temporary-user access grants, optionally filtered
// by user_id or exact path. It joins the User table to provide usernames.
func (s *Server) handleListAccessGrants(c *gin.Context) {
	query := s.db.Model(&models.AccessGrant{}).
		Select("access_grants.*, users.username as username").
		Joins("LEFT JOIN users ON users.id = access_grants.user_id")

	if uid := c.Query("user_id"); uid != "" {
		query = query.Where("access_grants.user_id = ?", uid)
	}
	if p := c.Query("path"); p != "" {
		query = query.Where("access_grants.path = ?", cleanLogical(p))
	}

	type grantRow struct {
		models.AccessGrant
		Username string
	}
	var rows []grantRow
	query.Order("access_grants.id desc").Find(&rows)

	grants := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		grants = append(grants, renderAccessGrant(r.AccessGrant, r.Username))
	}
	c.JSON(http.StatusOK, gin.H{"grants": grants})
}

// handleCreateAccessGrants creates a path access grant for a temporary user.
func (s *Server) handleCreateAccessGrant(c *gin.Context) {
	var in struct {
		UserID    uint    `json:"user_id" binding:"required"`
		Path      string  `json:"path" binding:"required"`
		ExpiresAt *string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var u models.User
	if err := s.db.First(&u, in.UserID).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "用户不存在"})
		return
	}
	if u.Role != "temporary" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只能为临时用户授权"})
		return
	}

	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能对根目录授权"})
		return
	}

	grant := models.AccessGrant{
		UserID:    in.UserID,
		Path:      p,
		GrantedBy: uidOf(c),
		CreatedAt: time.Now(),
	}
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *in.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "过期时间格式无效，请使用 RFC3339"})
			return
		}
		grant.ExpiresAt = &t
	}

	if err := s.db.Create(&grant).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "该路径已授权给此用户"})
		return
	}

	s.audit(c, uidOf(c), usernameOf(c), "access_grant.create", "user_id="+itoa(grant.UserID)+" path="+grant.Path)
	c.JSON(http.StatusOK, gin.H{"grant": renderAccessGrant(grant, u.Username)})
}

// handleDeleteAccessGrant revokes a grant by id.
func (s *Server) handleDeleteAccessGrant(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if err := s.db.Delete(&models.AccessGrant{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, uidOf(c), usernameOf(c), "access_grant.delete", "id="+c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func renderAccessGrant(g models.AccessGrant, username string) gin.H {
	out := gin.H{
		"id":         g.ID,
		"user_id":    g.UserID,
		"username":   username,
		"path":       g.Path,
		"granted_by": g.GrantedBy,
		"created_at": g.CreatedAt,
	}
	if g.ExpiresAt != nil {
		out["expires_at"] = *g.ExpiresAt
	} else {
		out["expires_at"] = nil
	}
	return out
}

