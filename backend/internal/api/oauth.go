package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gosocode/internal/baidu"
	"gosocode/internal/models"
)

// handleGetBaiduConfig returns the current Baidu app config for the admin form.
// The secret is never sent back; has_secret signals whether one is stored.
func (s *Server) handleGetBaiduConfig(c *gin.Context) {
	bc := s.baiduConfig()
	c.JSON(http.StatusOK, gin.H{
		"app_key":      bc.AppKey,
		"sign_key":     bc.SignKey,
		"redirect_uri": bc.RedirectURI,
		"has_secret":   bc.SecretKey != "",
		"configured":   bc.AppKey != "" && bc.SecretKey != "",
	})
}

// handleSaveBaiduConfig persists the app credentials entered in the UI and pushes
// them into the live client. An empty secret_key keeps the existing secret.
func (s *Server) handleSaveBaiduConfig(c *gin.Context) {
	var in struct {
		AppKey      string `json:"app_key" binding:"required"`
		SecretKey   string `json:"secret_key"`
		SignKey     string `json:"sign_key"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var bc models.BaiduAppConfig
	s.db.First(&bc, 1)
	bc.ID = 1
	bc.AppKey = in.AppKey
	if in.SecretKey != "" {
		bc.SecretKey = in.SecretKey
	}
	bc.SignKey = in.SignKey
	bc.RedirectURI = in.RedirectURI
	bc.UpdatedAt = time.Now()
	if err := s.db.Save(&bc).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	s.client.SetCreds(bc.AppKey, bc.SecretKey)
	s.audit(c, uidOf(c), usernameOf(c), "baidu.config", "app_key updated")
	c.JSON(http.StatusOK, gin.H{"ok": true, "configured": bc.AppKey != "" && bc.SecretKey != ""})
}

// handleOAuthURL returns the Baidu authorization URL. The platform admin opens
// it, logs into the Baidu account to be used as a storage node, and consents.
func (s *Server) handleOAuthURL(c *gin.Context) {
	bc := s.baiduConfig()
	if bc.AppKey == "" || bc.SecretKey == "" {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "尚未配置百度应用 AppKey/SecretKey，请先在「百度应用配置」中填写"})
		return
	}
	state := s.mintOAuthState("")
	c.JSON(http.StatusOK, gin.H{"url": s.client.AuthCodeURL(bc.RedirectURI, state), "oob": bc.RedirectURI == "oob", "state": state})
}

// handleOAuthExchange completes the "oob" (out-of-band) flow used when Baidu
// cannot call back a local/private address: the admin authorizes in a new tab,
// Baidu shows a one-time code, and the admin pastes it here to create the node.
func (s *Server) handleOAuthExchange(c *gin.Context) {
	var in struct {
		Code     string `json:"code" binding:"required"`
		Nickname string `json:"nickname"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	bc := s.baiduConfig()
	if bc.AppKey == "" || bc.SecretKey == "" {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "尚未配置百度应用 AppKey/SecretKey"})
		return
	}
	ctx := c.Request.Context()
	tr, err := s.client.ExchangeCode(ctx, in.Code, bc.RedirectURI)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	nickname := in.Nickname
	if nickname == "" {
		nickname = "node"
	}
	acc := models.BaiduAccount{
		Nickname:     nickname,
		MountPath:    "/",
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Status:       "active",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if err := s.db.Create(&acc).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = s.pool.RefreshQuota(ctx, &acc)
	requiresRename := s.disableDuplicateMountName(&acc)
	s.audit(c, uidOf(c), usernameOf(c), "node.add", "oob node="+nickname)
	c.JSON(http.StatusOK, gin.H{"node_id": acc.ID, "requires_rename": requiresRename})
}

// handleOAuthCallback receives ?code=, exchanges it for tokens, registers the
// Baidu account as an active storage node, and refreshes its quota. It always
// responds with an HTML page that notifies the opener window (the Nodes page)
// via postMessage and then closes itself, so the whole flow is driven from the
// frontend with no manual token entry.
//
// Public route: the browser is redirected here by Baidu so we cannot require a
// JWT. Instead we require the `state` parameter to be a single-use token
// minted by handleOAuthURL — see Server.mintOAuthState / consumeOAuthState.
func (s *Server) handleOAuthCallback(c *gin.Context) {
	if _, ok := s.consumeOAuthState(c.Query("state")); !ok {
		s.oauthResult(c, false, 0, "授权已失效或 state 不合法，请回到接入页面重新发起")
		return
	}
	code := c.Query("code")
	if code == "" {
		s.oauthResult(c, false, 0, "缺少授权码 code")
		return
	}
	ctx := c.Request.Context()
	tr, err := s.client.ExchangeCode(ctx, code, s.baiduConfig().RedirectURI)
	if err != nil {
		s.oauthResult(c, false, 0, err.Error())
		return
	}
	acc := models.BaiduAccount{
		Nickname:     "node",
		MountPath:    "/",
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Status:       "active",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if err := s.db.Create(&acc).Error; err != nil {
		s.oauthResult(c, false, 0, err.Error())
		return
	}
	// Best-effort quota fetch so placement has real capacity numbers.
	_ = s.pool.RefreshQuota(ctx, &acc)
	warning := ""
	if s.disableDuplicateMountName(&acc) {
		warning = "挂载名称与已有节点重复，新节点已停用，请先重命名后启用"
	}
	s.oauthResult(c, true, acc.ID, warning)
}

func (s *Server) disableDuplicateMountName(acc *models.BaiduAccount) bool {
	if err := s.db.First(acc, acc.ID).Error; err != nil {
		return false
	}
	conflict, err := s.mountNameConflict(*acc)
	if err != nil || !conflict {
		return false
	}
	acc.Status = "disabled"
	_ = s.db.Model(acc).Update("status", "disabled").Error
	return true
}

// oauthResult renders the popup-closing page. json.Marshal HTML-escapes <,>,&
// so the payload is safe to embed inside <script>.
func (s *Server) oauthResult(c *gin.Context, ok bool, nodeID uint, errMsg string) {
	payload, _ := json.Marshal(gin.H{"source": "gsc-oauth", "ok": ok, "node_id": nodeID, "error": errMsg})
	title := "接入成功"
	if !ok {
		title = "接入失败"
	}
	html := `<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>` + title + `</title></head>
<body style="font-family:system-ui,sans-serif;text-align:center;padding-top:90px;color:#1e293b">
<script>
  var msg = ` + string(payload) + `;
  if (window.opener) { window.opener.postMessage(msg, "*"); setTimeout(function(){ window.close(); }, 600); }
</script>
<h2>` + title + `</h2>
<p style="color:#64748b">此窗口将自动关闭,如未关闭请手动关闭并返回。</p>
</body></html>`
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html)
}

// handleListNodes exposes storage-node health/capacity for an ops dashboard.
func (s *Server) handleListNodes(c *gin.Context) {
	var nodes []models.BaiduAccount
	s.db.Order("id asc").Find(&nodes)
	out := make([]gin.H, 0, len(nodes))
	for _, n := range nodes {
		free := n.QuotaTotal - n.QuotaUsed
		out = append(out, gin.H{
			"id": n.ID, "nickname": n.Nickname, "mount_name": mountLabel(n),
			"mount_segment": mountName(n), "mount_path": nodeMountPath(n), "remark": n.Remark, "status": n.Status,
			"quota_total": n.QuotaTotal, "quota_used": n.QuotaUsed, "quota_free": free,
			"current_load": n.CurrentLoad, "bytes_up": n.BytesUp, "bytes_down": n.BytesDown,
			"vip_type": n.VipType, "slice_size": baidu.SliceSizeForVip(n.VipType),
		})
	}
	c.JSON(http.StatusOK, gin.H{"nodes": out})
}
