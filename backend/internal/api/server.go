// Package api exposes the HTTP + WebSocket surface: auth, file ops, jobs.
package api

import (
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/config"
	"gosocode/internal/coord"
	"gosocode/internal/job"
	"gosocode/internal/metrics"
	"gosocode/internal/models"
	"gosocode/internal/realtime"
)

type Server struct {
	cfg    *config.Config
	db     *gorm.DB
	pool   *account.Pool
	client *baidu.Client
	coord  *coord.Coordinator
	jobs   *job.Manager
	hub    *realtime.Hub
	meter  *metrics.Meter

	// active streaming-upload sessions (per-session serialization + concurrency cap)
	streamMu     sync.Mutex
	streamStates map[string]*streamState

	// serializes agent progress bitmap updates (read-modify-write)
	agentMu sync.Mutex
}

func NewServer(cfg *config.Config, db *gorm.DB, pool *account.Pool, client *baidu.Client,
	co *coord.Coordinator, jobs *job.Manager, hub *realtime.Hub, meter *metrics.Meter) *Server {
	return &Server{
		cfg: cfg, db: db, pool: pool, client: client, coord: co, jobs: jobs, hub: hub, meter: meter,
		streamStates: map[string]*streamState{},
	}
}

func (s *Server) Router() *gin.Engine {
	r := gin.Default()
	r.Use(cors.New(cors.Config{
		AllowAllOrigins:  true,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: false,
	}))
	r.MaxMultipartMemory = 32 << 20

	r.GET("/api/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	// mode probe: a CLI client intercepts this same path and answers mode=client,
	// which is how the web UI auto-detects what it is bound to.
	r.GET("/api/client/info", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"mode": "server"}) })

	r.POST("/api/register", s.handleRegister)
	r.POST("/api/login", s.handleLogin)
	r.GET("/api/oauth/callback", s.handleOAuthCallback) // Baidu redirects here

	auth := r.Group("/api", s.authMiddleware())
	{
		// profile
		auth.GET("/me", s.handleMe)
		auth.PUT("/me/password", s.handleChangePassword)
		// files
		auth.GET("/storage", s.handleStorage)
		auth.GET("/files", s.handleListFiles)
		auth.POST("/files/upload/init", s.handleUploadInit)
		auth.PUT("/files/upload/chunk", s.handleUploadChunk)
		auth.GET("/files/upload/status", s.handleUploadStatus)
		auth.POST("/files/upload/finish", s.handleUploadFinish)
		auth.POST("/files/upload/cancel", s.handleUploadCancel)
		// streaming (pipelined) upload: browser slices go straight to Baidu
		auth.GET("/files/stream/plan", s.handleStreamPlan)
		auth.POST("/files/stream/init", s.handleStreamInit)
		auth.PUT("/files/stream/slice", s.handleStreamSlice)
		auth.GET("/files/stream/status", s.handleStreamStatus)
		auth.POST("/files/stream/finish", s.handleStreamFinish)
		auth.POST("/files/stream/cancel", s.handleStreamCancel)
		auth.POST("/files/folder", s.handleCreateFolder)
		auth.POST("/files/move", s.handleMove)
		auth.POST("/files/delete-path", s.handleDeletePath)
		auth.POST("/files/delete-folder", s.handleDeleteFolder)
		auth.GET("/files/remote-download", s.handleRemoteDownload)
		auth.GET("/files/:id/download", s.handleDownload)
		auth.DELETE("/files/:id", s.handleDeleteFile)
		auth.POST("/files/:id/rename", s.handleRenameFile)
		// transfers
		auth.GET("/jobs", s.handleListJobs)
		auth.POST("/jobs/:id/cancel", s.handleCancelJob)
		auth.POST("/transfers/log", s.handleLogTransfer)
		auth.GET("/transfers/log", s.handleListTransferLog)
	}

	// control plane for CLI clients that transfer directly against Baidu
	agent := r.Group("/api/agent", s.authMiddleware(), s.agentGuard())
	{
		agent.POST("/upload/init", s.handleAgentUploadInit)
		agent.GET("/upload/session", s.handleAgentUploadSession)
		agent.POST("/upload/progress", s.handleAgentUploadProgress)
		agent.GET("/download/resolve", s.handleAgentDownloadResolve)
		agent.POST("/download/report", s.handleAgentDownloadReport)
	}

	admin := r.Group("/api/admin", s.authMiddleware(), s.adminMiddleware())
	{
		admin.GET("/users", s.handleListUsers)
		admin.POST("/users", s.handleCreateUser)
		admin.PUT("/users/:id", s.handleUpdateUser)
		admin.POST("/users/:id/status", s.handleSetUserStatus)
		admin.POST("/users/:id/role", s.handleSetUserRole)
		admin.DELETE("/users/:id", s.handleDeleteUser)

		admin.GET("/baidu-config", s.handleGetBaiduConfig)
		admin.PUT("/baidu-config", s.handleSaveBaiduConfig)
		admin.GET("/oauth/url", s.handleOAuthURL)            // begin Baidu authorization
		admin.POST("/oauth/exchange", s.handleOAuthExchange) // oob: paste code -> node
		admin.GET("/nodes", s.handleListNodes)
		admin.PUT("/nodes/:id", s.handleUpdateNode)
		admin.GET("/nodes/:id/directories", s.handleListNodeDirectories)
		admin.POST("/nodes/:id/status", s.handleSetNodeStatus)
		admin.POST("/nodes/:id/refresh", s.handleRefreshNode)
		admin.DELETE("/nodes/:id", s.handleDeleteNode)

		admin.GET("/traffic", s.handleTraffic)
		admin.GET("/logs", s.handleListLogs)
	}

	r.GET("/api/ws", s.handleWS) // token passed as query param

	// Serve the built web UI (frontend/dist copied to cfg.WebUIDir) when present,
	// with SPA fallback to index.html. Both server and CLI client bind the same UI.
	if dir := s.cfg.WebUIDir; dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			r.NoRoute(func(c *gin.Context) {
				p := c.Request.URL.Path
				if strings.HasPrefix(p, "/api/") || c.Request.Method != http.MethodGet {
					c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
					return
				}
				fp := filepath.Join(dir, filepath.FromSlash(path.Clean("/"+p)))
				if st, err := os.Stat(fp); err == nil && !st.IsDir() {
					c.File(fp)
					return
				}
				c.File(filepath.Join(dir, "index.html"))
			})
			log.Printf("web ui: serving %s", dir)
		}
	}
	return r
}

// adminMiddleware rejects non-admin users. Must run after authMiddleware,
// which loads the role into the context.
func (s *Server) adminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if role, _ := c.Get("role"); role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin only"})
			return
		}
		c.Next()
	}
}

// audit records a notable action for the ops log view (best-effort).
func (s *Server) audit(c *gin.Context, userID uint, username, action, detail string) {
	s.db.Create(&models.AuditLog{
		At: time.Now(), UserID: userID, Username: username,
		Action: action, Detail: detail, IP: c.ClientIP(),
	})
}

// baiduConfig returns the effective Baidu app config: the DB singleton, with
// empty fields falling back to env-based defaults.
func (s *Server) baiduConfig() models.BaiduAppConfig {
	var bc models.BaiduAppConfig
	s.db.First(&bc, 1)
	if bc.RedirectURI == "" {
		bc.RedirectURI = s.cfg.RedirectURI
	}
	return bc
}

func itoa(u uint) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
