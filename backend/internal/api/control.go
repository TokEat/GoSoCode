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
		if s.control == nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "服务端未配置 GSC_AGENT_CONTROL_KEY，无法解密控制请求；请移除客户端 -control-key 或在服务端配置相同密钥"})
			return
		}
		if c.GetHeader(crypto.Header) != crypto.Version {
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

// requireEncryptedAgentControl enforces control-plane encryption only when
// the server has a key configured; without GSC_AGENT_CONTROL_KEY the agent
// control plane runs in plaintext (trusted deployments, matching a client
// started without -control-key).
func (s *Server) requireEncryptedAgentControl() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.control == nil {
			c.Next()
			return
		}
		if encrypted, _ := c.Get(encryptedControlContextKey); encrypted != true {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "服务端已启用控制面加密，客户端需用相同的 GSC_AGENT_CONTROL_KEY 启动（-control-key）"})
			return
		}
		c.Next()
	}
}

func (s *Server) agentJSON(c *gin.Context, status int, value any) {
	if s.control == nil {
		c.JSON(status, value)
		return
	}
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
