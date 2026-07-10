package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"gosocode/internal/models"
)

// handleMe returns the current user's profile and personal usage.
func (s *Server) handleMe(c *gin.Context) {
	var u models.User
	if err := s.db.First(&u, uidOf(c)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var fileCount int64
	s.db.Model(&models.File{}).Where("owner_user_id = ? AND status != ?", u.ID, "deleted").Count(&fileCount)
	c.JSON(http.StatusOK, gin.H{
		"id": u.ID, "username": u.Username, "role": u.Role, "status": u.Status,
		"bytes_up": u.BytesUp, "bytes_down": u.BytesDown,
		"file_count": fileCount, "created_at": u.CreatedAt,
	})
}

func (s *Server) handleChangePassword(c *gin.Context) {
	var in struct {
		Old string `json:"old_password" binding:"required"`
		New string `json:"new_password" binding:"required,min=6"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var u models.User
	s.db.First(&u, uidOf(c))
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Old)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "old password incorrect"})
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	s.db.Model(&u).Update("password_hash", string(hash))
	s.audit(c, u.ID, u.Username, "password.change", "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
