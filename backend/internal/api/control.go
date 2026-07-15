package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"gosocode/internal/crypto"
)

const encryptedControlContextKey = "encrypted-control"

// controlCryptoMiddleware unwraps a CLI control-plane request before the auth
// middleware consumes its Authorization header. Browser API requests remain
// unchanged because they do not carry the protocol marker.
func (s *Server) controlCryptoMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader(crypto.Header) == "" {
			c.Next()
			return
		}
		if c.GetHeader(crypto.Header) != crypto.Version || s.control == nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "无效的客户端控制面加密配置"})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "无法读取加密控制请求"})
			return
		}
		request, err := s.control.OpenRequest(c.Request.Method, c.Request.URL.Path, raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "加密控制请求校验失败"})
			return
		}
		c.Request.Header.Set("Authorization", request.Authorization)
		c.Request.URL.RawQuery = request.Query
		c.Request.Body = io.NopCloser(bytes.NewReader(request.Body))
		c.Request.ContentLength = int64(len(request.Body))
		c.Set(encryptedControlContextKey, true)
		c.Next()
	}
}

func (s *Server) requireEncryptedAgentControl() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.control == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "客户端直连需要配置 GSC_AGENT_CONTROL_KEY"})
			return
		}
		if encrypted, _ := c.Get(encryptedControlContextKey); encrypted != true {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "客户端直连必须使用加密控制协议"})
			return
		}
		c.Next()
	}
}

func (s *Server) agentJSON(c *gin.Context, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "编码加密控制响应失败"})
		return
	}
	encrypted, err := s.control.SealResponse(c.Request.Method, c.Request.URL.Path, status, raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "加密控制响应失败"})
		return
	}
	c.Header(crypto.Header, crypto.Version)
	c.Data(status, "application/json; charset=utf-8", encrypted)
}

func (s *Server) controlJSON(c *gin.Context, status int, value any) {
	if encrypted, _ := c.Get(encryptedControlContextKey); encrypted == true {
		s.agentJSON(c, status, value)
		return
	}
	c.JSON(status, value)
}
