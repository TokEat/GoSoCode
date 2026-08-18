package api

import (
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"gosocode/internal/manifest"
	"gosocode/internal/metrics"
	"gosocode/internal/models"
)

// ---- system user management ------------------------------------------------

func (s *Server) handleListUsers(c *gin.Context) {
	var users []models.User
	s.db.Order("id asc").Find(&users)
	out := make([]gin.H, 0, len(users))
	for _, u := range users {
		out = append(out, renderUser(u))
	}
	c.JSON(http.StatusOK, gin.H{"users": out})
}

type adminUserPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

func cleanUserPayload(in adminUserPayload) adminUserPayload {
	in.Username = strings.TrimSpace(in.Username)
	in.Role = strings.TrimSpace(in.Role)
	in.Status = strings.TrimSpace(in.Status)
	return in
}

func validUserRole(role string) bool {
	return role == "admin" || role == "user" || role == "temporary"
}

func validUserStatus(status string) bool {
	return status == "active" || status == "disabled"
}

func renderUser(u models.User) gin.H {
	return gin.H{
		"id": u.ID, "username": u.Username, "role": u.Role, "status": u.Status,
		"bytes_up": u.BytesUp, "bytes_down": u.BytesDown, "created_at": u.CreatedAt,
		"sso": u.OIDCSubject != "",
	}
}

func (s *Server) handleCreateUser(c *gin.Context) {
	var in adminUserPayload
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in = cleanUserPayload(in)
	if len(in.Username) < 3 || len(in.Username) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "用户名长度需为 3-64 个字符"})
		return
	}
	if len(in.Password) < 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "密码至少 6 个字符"})
		return
	}
	if in.Role == "" {
		in.Role = "user"
	}
	if in.Status == "" {
		in.Status = "active"
	}
	if !validUserRole(in.Role) || !validUserStatus(in.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "角色或状态无效"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	u := models.User{
		Username: in.Username, PasswordHash: string(hash), Role: in.Role, Status: in.Status,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.db.Create(&u).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "用户名已存在"})
		return
	}
	s.audit(c, uidOf(c), usernameOf(c), "user.create", "id="+itoa(u.ID)+" username="+u.Username)
	c.JSON(http.StatusOK, gin.H{"user": renderUser(u)})
}

func (s *Server) handleUpdateUser(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var u models.User
	if err := s.db.First(&u, id).Error; err != nil {
		status := http.StatusInternalServerError
		if err == gorm.ErrRecordNotFound {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": "用户不存在"})
		return
	}
	var in adminUserPayload
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in = cleanUserPayload(in)
	if in.Username != "" {
		if len(in.Username) < 3 || len(in.Username) > 64 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "用户名长度需为 3-64 个字符"})
			return
		}
		u.Username = in.Username
	}
	if in.Password != "" {
		if len(in.Password) < 6 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "密码至少 6 个字符"})
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		u.PasswordHash = string(hash)
	}
	if in.Role != "" {
		if !validUserRole(in.Role) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "角色无效"})
			return
		}
		if u.ID == uidOf(c) && in.Role != u.Role {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不能修改自己的角色"})
			return
		}
		u.Role = in.Role
	}
	if in.Status != "" {
		if !validUserStatus(in.Status) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "状态无效"})
			return
		}
		if u.ID == uidOf(c) && in.Status != u.Status {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不能修改自己的状态"})
			return
		}
		u.Status = in.Status
	}
	u.UpdatedAt = time.Now()
	if err := s.db.Save(&u).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "用户名已存在"})
		return
	}
	s.audit(c, uidOf(c), usernameOf(c), "user.update", "id="+c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"user": renderUser(u)})
}

func (s *Server) handleSetUserStatus(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if uint(id) == uidOf(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot change your own status"})
		return
	}
	var in struct {
		Status string `json:"status" binding:"required,oneof=active disabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.db.Model(&models.User{}).Where("id = ?", id).Update("status", in.Status)
	s.audit(c, uidOf(c), usernameOf(c), "user.status", "id="+c.Param("id")+" -> "+in.Status)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleSetUserRole(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if uint(id) == uidOf(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot change your own role"})
		return
	}
	var in struct {
		Role string `json:"role" binding:"required,oneof=admin user temporary"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.db.Model(&models.User{}).Where("id = ?", id).Update("role", in.Role)
	s.audit(c, uidOf(c), usernameOf(c), "user.role", "id="+c.Param("id")+" -> "+in.Role)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleDeleteUser(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if uint(id) == uidOf(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete yourself"})
		return
	}
	s.db.Delete(&models.User{}, id)
	var accIDs []uint
	s.db.Model(&models.File{}).Where("owner_user_id = ? AND status <> ?", id, "deleted").
		Distinct().Pluck("baidu_account_id", &accIDs)
	s.db.Model(&models.File{}).Where("owner_user_id = ?", id).Update("status", "deleted")
	for _, a := range accIDs {
		s.msync.Mark(a)
	}
	s.audit(c, uidOf(c), usernameOf(c), "user.delete", "id="+c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- storage node management ----------------------------------------------

func (s *Server) mountNameConflict(acc models.BaiduAccount) (bool, error) {
	var accounts []models.BaiduAccount
	if err := s.db.Where("id != ?", acc.ID).Find(&accounts).Error; err != nil {
		return false, err
	}
	wantedName := mountLabel(acc)
	for _, other := range accounts {
		if mountLabel(other) == wantedName {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) handleUpdateNode(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var acc models.BaiduAccount
	if err := s.db.First(&acc, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "挂载节点不存在"})
		return
	}
	var in struct {
		MountName string `json:"mount_name"`
		Remark    string `json:"remark"`
		MountPath string `json:"mount_path"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.MountName = strings.TrimSpace(in.MountName)
	in.Remark = strings.TrimSpace(in.Remark)
	if strings.ContainsAny(in.MountName, "/\\\x00") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "挂载名称不能包含斜杠或空字符"})
		return
	}
	if len([]rune(in.MountName)) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "挂载名称不能超过 128 个字符"})
		return
	}
	if len([]rune(in.Remark)) > 512 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注不能超过 512 个字符"})
		return
	}
	candidate := acc
	candidate.MountName = in.MountName
	conflict, err := s.mountNameConflict(candidate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if conflict {
		c.JSON(http.StatusConflict, gin.H{"error": "挂载名称已被其他节点使用"})
		return
	}
	mountPath, err := normalizeMountPath(in.MountPath)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if mountPath != nodeMountPath(acc) {
		token, err := s.pool.Token(c.Request.Context(), &acc)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "验证挂载目录失败: " + err.Error()})
			return
		}
		if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		if _, err := s.client.ListDir(c.Request.Context(), token, mountPath); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "挂载目录不可访问: " + err.Error()})
			return
		}
	}
	mountPathChanged := mountPath != nodeMountPath(acc)
	acc.MountName = in.MountName
	acc.Remark = in.Remark
	acc.MountPath = mountPath
	acc.UpdatedAt = time.Now()
	if err := s.db.Save(&acc).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if mountPathChanged {
		// the manifest lives at the mount root: write one at the new root
		s.msync.Mark(acc.ID)
	}
	s.audit(c, uidOf(c), usernameOf(c), "node.update", "id="+c.Param("id")+" mount="+mountName(acc)+" path="+mountPath)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleListNodeDirectories(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var acc models.BaiduAccount
	if err := s.db.First(&acc, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "挂载节点不存在"})
		return
	}
	current, err := normalizeMountPath(c.DefaultQuery("path", "/"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token, err := s.pool.Token(c.Request.Context(), &acc)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	items, err := s.client.ListDir(c.Request.Context(), token, current)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取网盘目录失败: " + err.Error()})
		return
	}
	directories := make([]gin.H, 0)
	for _, item := range items {
		if item.IsDir != 1 {
			continue
		}
		name := item.ServerFilename
		if name == "" {
			name = path.Base(item.Path)
		}
		directories = append(directories, gin.H{"name": name, "path": item.Path})
	}
	parent := path.Dir(current)
	if current == "/" || parent == "." {
		parent = "/"
	}
	c.JSON(http.StatusOK, gin.H{"current": current, "parent": parent, "directories": directories})
}

func (s *Server) handleSetNodeStatus(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var in struct {
		Status string `json:"status" binding:"required,oneof=active disabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Status == "active" {
		var acc models.BaiduAccount
		if err := s.db.First(&acc, id).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "挂载节点不存在"})
			return
		}
		conflict, err := s.mountNameConflict(acc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if conflict {
			c.JSON(http.StatusConflict, gin.H{"error": "挂载名称已被其他节点使用，请先重命名"})
			return
		}
	}
	s.db.Model(&models.BaiduAccount{}).Where("id = ?", id).Update("status", in.Status)
	s.audit(c, uidOf(c), usernameOf(c), "node.status", "id="+c.Param("id")+" -> "+in.Status)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleRefreshNode(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	acc, err := s.pool.Get(c.Request.Context(), uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		return
	}
	if err := s.pool.RefreshQuota(c.Request.Context(), acc); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleSyncNodeManifest imports the node's on-netdisk metadata manifest into
// the local File table (cross-deployment migration/sync).
func (s *Server) handleSyncNodeManifest(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	acc, err := s.pool.Get(c.Request.Context(), uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "挂载节点不存在"})
		return
	}
	doc, err := s.msync.Fetch(c.Request.Context(), acc)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	stats, err := manifest.Import(s.db, acc, mountName(*acc), doc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, uidOf(c), usernameOf(c), "node.manifest_sync",
		fmt.Sprintf("id=%d imported=%d updated=%d skipped=%d", id, stats.Imported, stats.Updated, stats.Skipped))
	c.JSON(http.StatusOK, gin.H{"ok": true, "imported": stats.Imported, "updated": stats.Updated, "skipped": stats.Skipped})
}

func (s *Server) handleDeleteNode(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var n int64
	s.db.Model(&models.File{}).Where("baidu_account_id = ? AND status != ?", id, "deleted").Count(&n)
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "node still holds files; disable it instead"})
		return
	}
	s.db.Delete(&models.BaiduAccount{}, id)
	s.audit(c, uidOf(c), usernameOf(c), "node.delete", "id="+c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- traffic & logs --------------------------------------------------------

func (s *Server) handleTraffic(c *gin.Context) {
	live := s.meter.Snapshot()

	type row struct {
		ID        uint   `json:"id"`
		Label     string `json:"label"`
		BytesUp   int64  `json:"bytes_up"`
		BytesDown int64  `json:"bytes_down"`
	}
	var nodes []models.BaiduAccount
	s.db.Find(&nodes)
	nodeRows := make([]row, 0, len(nodes))
	for _, n := range nodes {
		nodeRows = append(nodeRows, row{n.ID, n.Nickname, n.BytesUp, n.BytesDown})
	}
	var users []models.User
	s.db.Find(&users)
	userRows := make([]row, 0, len(users))
	for _, u := range users {
		userRows = append(userRows, row{u.ID, u.Username, u.BytesUp, u.BytesDown})
	}

	// Recent statistics chart: last 24h aggregated per hour from TransferLog,
	// which is DB-persisted so it survives restarts (unlike the live meter).
	type hourPoint struct {
		T    int64 `json:"t"` // hour start, unix seconds
		Up   int64 `json:"up"`
		Down int64 `json:"down"`
	}
	nowHour := time.Now().Truncate(time.Hour)
	since := nowHour.Add(-23 * time.Hour)
	var logs []models.TransferLog
	s.db.Where("at >= ?", since).Find(&logs)
	upByHour := map[int64]int64{}
	downByHour := map[int64]int64{}
	for _, l := range logs {
		h := l.At.Truncate(time.Hour).Unix()
		if l.Kind == "upload" {
			upByHour[h] += l.Size
		} else {
			downByHour[h] += l.Size
		}
	}
	hourly := make([]hourPoint, 0, 24)
	for i := 23; i >= 0; i-- {
		h := nowHour.Add(-time.Duration(i) * time.Hour).Unix()
		hourly = append(hourly, hourPoint{T: h, Up: upByHour[h], Down: downByHour[h]})
	}

	// Live chart: per-second samples aggregated into `interval`-second buckets
	// (selectable from the frontend, default 30s). Values are average bytes/sec
	// over each bucket; the chart spans up to 60 buckets (max 1h of history).
	interval, _ := strconv.Atoi(c.DefaultQuery("interval", "30"))
	if interval < 1 {
		interval = 30
	}
	if interval > 900 {
		interval = 900
	}
	span := interval * 60
	if span > 3600 {
		span = 3600
	}
	raw := s.meter.Series(span)
	points := len(raw) / interval
	series := make([]metrics.Point, 0, points)
	for i := 0; i < points; i++ {
		p := metrics.Point{Sec: raw[i*interval].Sec}
		for j := 0; j < interval; j++ {
			p.Up += raw[i*interval+j].Up
			p.Down += raw[i*interval+j].Down
		}
		p.Up /= int64(interval) // -> average B/s over the bucket
		p.Down /= int64(interval)
		series = append(series, p)
	}

	c.JSON(http.StatusOK, gin.H{
		"live": live, "nodes": nodeRows, "users": userRows,
		"series": series, "interval": interval,
		"hourly": hourly,
	})
}

func (s *Server) handleListLogs(c *gin.Context) {
	page := 1
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		page = v
	}
	pageSize := 20
	if v, err := strconv.Atoi(c.Query("page_size")); err == nil && v > 0 && v <= 100 {
		pageSize = v
	}
	q := s.db.Model(&models.AuditLog{})
	if action := c.Query("action"); action != "" {
		q = q.Where("action = ?", action)
	}
	var total int64
	q.Count(&total)
	var logs []models.AuditLog
	q.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&logs)
	c.JSON(http.StatusOK, gin.H{"logs": logs, "total": total, "page": page, "page_size": pageSize})
}

// ---- system settings ---------------------------------------------------------

func renderSystemConfig(sc models.SystemConfig) gin.H {
	return gin.H{
		"encrypt_storage": sc.EncryptStorage, "uuid_storage": sc.UUIDStorage, "log_max_entries": sc.LogMaxEntries,
		"share_import_enabled": sc.ShareImportEnabled,
		"oidc_enabled": sc.OIDCEnabled, "oidc_issuer": sc.OIDCIssuer,
		"oidc_client_id": sc.OIDCClientID, "oidc_redirect_url": sc.OIDCRedirectURL,
		"oidc_has_secret": sc.OIDCClientSecret != "",
	}
}

func (s *Server) handleGetSystemConfig(c *gin.Context) {
	c.JSON(http.StatusOK, renderSystemConfig(s.systemConfig()))
}

func (s *Server) handleSaveSystemConfig(c *gin.Context) {
	var in struct {
		EncryptStorage     *bool   `json:"encrypt_storage"`
		UUIDStorage        *bool   `json:"uuid_storage"`
		LogMaxEntries      *int    `json:"log_max_entries"`
		ShareImportEnabled *bool   `json:"share_import_enabled"`
		OIDCEnabled      *bool   `json:"oidc_enabled"`
		OIDCIssuer       *string `json:"oidc_issuer"`
		OIDCClientID     *string `json:"oidc_client_id"`
		OIDCClientSecret *string `json:"oidc_client_secret"`
		OIDCRedirectURL  *string `json:"oidc_redirect_url"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	sc := s.systemConfig()
	if in.EncryptStorage != nil {
		sc.EncryptStorage = *in.EncryptStorage
	}
	if in.UUIDStorage != nil {
		sc.UUIDStorage = *in.UUIDStorage
	}
	if in.ShareImportEnabled != nil {
		sc.ShareImportEnabled = *in.ShareImportEnabled
	}
	if in.LogMaxEntries != nil {
		if *in.LogMaxEntries < 50 || *in.LogMaxEntries > 100000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "日志保留条数需在 50-100000 之间"})
			return
		}
		sc.LogMaxEntries = *in.LogMaxEntries
	}
	if in.OIDCIssuer != nil {
		sc.OIDCIssuer = strings.TrimRight(strings.TrimSpace(*in.OIDCIssuer), "/")
	}
	if in.OIDCClientID != nil {
		sc.OIDCClientID = strings.TrimSpace(*in.OIDCClientID)
	}
	// empty secret in the payload means "keep the stored one" (it is never echoed back)
	if in.OIDCClientSecret != nil && strings.TrimSpace(*in.OIDCClientSecret) != "" {
		sc.OIDCClientSecret = strings.TrimSpace(*in.OIDCClientSecret)
	}
	if in.OIDCRedirectURL != nil {
		sc.OIDCRedirectURL = strings.TrimSpace(*in.OIDCRedirectURL)
	}
	if in.OIDCEnabled != nil {
		sc.OIDCEnabled = *in.OIDCEnabled
	}
	if sc.OIDCEnabled && (sc.OIDCIssuer == "" || sc.OIDCClientID == "" || sc.OIDCClientSecret == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "启用统一认证前需填写 Issuer 地址、Client ID 和 Client Secret"})
		return
	}
	// Pin the callback when enabling: capture the origin the admin is using
	// right now (a working server address) so every later flow — including
	// CLI-client-proxied SSO, whose proxied Host rarely matches the provider
	// whitelist — sends this exact, single whitelisted redirect_uri.
	if sc.OIDCEnabled && sc.OIDCRedirectURL == "" {
		sc.OIDCRedirectURL = s.oidcRedirectURI(c, sc)
	}
	sc.ID = 1
	sc.UpdatedAt = time.Now()
	if err := s.db.Save(&sc).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, uidOf(c), usernameOf(c), "system.config",
		"encrypt_storage="+strconv.FormatBool(sc.EncryptStorage)+" uuid_storage="+strconv.FormatBool(sc.UUIDStorage)+
			" log_max_entries="+strconv.Itoa(sc.LogMaxEntries)+
			" share_import_enabled="+strconv.FormatBool(sc.ShareImportEnabled)+
			" oidc_enabled="+strconv.FormatBool(sc.OIDCEnabled))
	c.JSON(http.StatusOK, renderSystemConfig(sc))
}
