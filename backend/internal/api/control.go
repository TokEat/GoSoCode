package api

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"crypto/rand"

	"github.com/gin-gonic/gin"

	"gosocode/internal/crypto"
)

const (
	encryptedControlContextKey = "encrypted-control"
	controlCodecContextKey     = "control-codec"

	controlSessionTTL = 12 * time.Hour
)

// controlSession is one negotiated (ECDH) control-plane encryption session.
type controlSession struct {
	codec   *crypto.Codec
	expires time.Time
}

// handleAgentHandshake negotiates a per-session control-plane key: the
// logged-in client posts its X25519 public key, the server answers with its
// own public key and a session id; both sides derive the same AES-256-GCM key
// via ECDH+HKDF. Requires a valid login token, so only authenticated users
// can open control sessions.
func (s *Server) handleAgentHandshake(c *gin.Context) {
	if s.handshakePriv == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "服务端未能生成控制面密钥对"})
		return
	}
	var in struct {
		ClientPub string `json:"client_pub"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}
	clientPub, err := base64.StdEncoding.DecodeString(in.ClientPub)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的客户端公钥"})
		return
	}
	serverPub := s.handshakePriv.PublicKey().Bytes()
	codec, err := crypto.DeriveSessionCodec(s.handshakePriv, clientPub, serverPub, clientPub)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "密钥协商失败: " + err.Error()})
		return
	}
	sid := make([]byte, 16)
	if _, err := rand.Read(sid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成会话标识失败"})
		return
	}
	sessionID := hex.EncodeToString(sid)

	now := time.Now()
	s.ctrlSessMu.Lock()
	for id, ses := range s.ctrlSessions {
		if now.After(ses.expires) {
			delete(s.ctrlSessions, id)
		}
	}
	s.ctrlSessions[sessionID] = &controlSession{codec: codec, expires: now.Add(controlSessionTTL)}
	s.ctrlSessMu.Unlock()

	c.JSON(http.StatusOK, gin.H{
		"session_id": sessionID,
		"server_pub": base64.StdEncoding.EncodeToString(serverPub),
	})
}

// controlSessionCodec resolves a negotiated session codec; nil when the
// session is unknown or expired.
func (s *Server) controlSessionCodec(sessionID string) *crypto.Codec {
	if sessionID == "" {
		return nil
	}
	s.ctrlSessMu.Lock()
	defer s.ctrlSessMu.Unlock()
	ses, ok := s.ctrlSessions[sessionID]
	if !ok || time.Now().After(ses.expires) {
		delete(s.ctrlSessions, sessionID)
		return nil
	}
	return ses.codec
}

// controlCryptoMiddleware unwraps a CLI control-plane request before the auth
// middleware consumes its Authorization header. Browser API requests remain
// unchanged because they do not carry the protocol marker. Requests with a
// session header use the negotiated per-session key; without it the static
// GSC_AGENT_CONTROL_KEY codec applies.
func (s *Server) controlCryptoMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader(crypto.Header) == "" {
			c.Next()
			return
		}
		if c.GetHeader(crypto.Header) != crypto.Version {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "无效的客户端控制面加密配置"})
			return
		}
		var codec *crypto.Codec
		if sid := c.GetHeader(crypto.SessionHeader); sid != "" {
			codec = s.controlSessionCodec(sid)
			if codec == nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "控制面会话无效或已过期，请重新协商密钥"})
				return
			}
		} else {
			codec = s.control
			if codec == nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "服务端未配置 GSC_AGENT_CONTROL_KEY 静态密钥；请移除客户端 -control-key 使用自动密钥协商"})
				return
			}
		}
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "无法读取加密控制请求"})
			return
		}
		request, err := codec.OpenRequest(c.Request.Method, c.Request.URL.Path, raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "加密控制请求校验失败"})
			return
		}
		c.Request.Header.Set("Authorization", request.Authorization)
		if request.AccessPassword != "" {
			c.Request.Header.Set(accessPasswordHeader, request.AccessPassword)
		} else {
			c.Request.Header.Del(accessPasswordHeader)
		}
		c.Request.URL.RawQuery = request.Query
		c.Request.Body = io.NopCloser(bytes.NewReader(request.Body))
		c.Request.ContentLength = int64(len(request.Body))
		c.Set(encryptedControlContextKey, true)
		c.Set(controlCodecContextKey, codec)
		c.Next()
	}
}

// requireEncryptedAgentControl gates /api/agent/*: these endpoints hand out
// node access tokens, so they only accept encrypted control requests — either
// a negotiated session (default, zero config) or the static shared key.
func (s *Server) requireEncryptedAgentControl() gin.HandlerFunc {
	return func(c *gin.Context) {
		if encrypted, _ := c.Get(encryptedControlContextKey); encrypted != true {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "客户端直连需要加密控制通道：请先通过 /api/agent/handshake 协商密钥"})
			return
		}
		c.Next()
	}
}

func requestCodec(c *gin.Context) *crypto.Codec {
	if v, ok := c.Get(controlCodecContextKey); ok {
		if codec, ok := v.(*crypto.Codec); ok {
			return codec
		}
	}
	return nil
}

func (s *Server) agentJSON(c *gin.Context, status int, value any) {
	codec := requestCodec(c)
	if codec == nil {
		c.JSON(status, value)
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "编码加密控制响应失败"})
		return
	}
	encrypted, err := codec.SealResponse(c.Request.Method, c.Request.URL.Path, status, raw)
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
