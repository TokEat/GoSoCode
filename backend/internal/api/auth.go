package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"gosocode/internal/models"
)

type credentials struct {
	Username string `json:"username" binding:"required,min=3,max=64"`
	Password string `json:"password" binding:"required,min=6"`
}

func (s *Server) handleLogin(c *gin.Context) {
	var in credentials
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var u models.User
	if err := s.db.Where("username = ?", in.Username).First(&u).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if u.Status == "disabled" {
		c.JSON(http.StatusForbidden, gin.H{"error": "account disabled"})
		return
	}
	// With unified SSO on, local passwords remain valid only for admins (the
	// escape hatch if the identity provider is down or misconfigured).
	if sc := s.systemConfig(); sc.OIDCEnabled && u.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "系统已启用统一认证，请使用统一认证登录"})
		return
	}
	s.audit(c, u.ID, u.Username, "login", "")
	c.JSON(http.StatusOK, gin.H{"token": s.issueToken(u.ID), "user_id": u.ID, "role": u.Role})
}

func (s *Server) issueToken(userID uint) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(7 * 24 * time.Hour).Unix(),
	})
	str, _ := tok.SignedString([]byte(s.cfg.JWTSecret))
	return str
}

func (s *Server) parseToken(raw string) (uint, bool) {
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	})
	if err != nil || !tok.Valid {
		return 0, false
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return 0, false
	}
	sub, ok := claims["sub"].(float64)
	if !ok {
		return 0, false
	}
	return uint(sub), true
}

func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		raw := strings.TrimPrefix(h, "Bearer ")
		uid, ok := s.parseToken(raw)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var u models.User
		if err := s.db.First(&u, uid).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unknown user"})
			return
		}
		if u.Status == "disabled" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "account disabled"})
			return
		}
		c.Set("uid", uid)
		c.Set("role", u.Role)
		c.Set("username", u.Username)
		c.Next()
	}
}

func uidOf(c *gin.Context) uint        { return c.MustGet("uid").(uint) }
func usernameOf(c *gin.Context) string { return c.MustGet("username").(string) }
