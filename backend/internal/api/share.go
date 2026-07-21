package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/job"
)

func (s *Server) shareImportGuard(c *gin.Context) bool {
	if !s.systemConfig().ShareImportEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": "分享转存功能已被管理员关闭"})
		return false
	}
	if !s.client.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "尚未配置百度应用"})
		return false
	}
	return true
}

// handleShareResolve parses an external share link, verifies the extraction
// code and returns the share's root file list so the user can pick what to
// import.
func (s *Server) handleShareResolve(c *gin.Context) {
	if !s.shareImportGuard(c) {
		return
	}
	var in struct {
		Link string `json:"link"`
		Pwd  string `json:"pwd"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	surl, parsedPwd := baidu.ParseShareURL(in.Link)
	if surl == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无法识别分享链接，请粘贴完整的百度网盘分享链接"})
		return
	}
	pwd := strings.TrimSpace(in.Pwd)
	if pwd == "" {
		pwd = parsedPwd
	}

	nodes, err := s.activeNodes()
	if err != nil || len(nodes) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂无可用存储节点"})
		return
	}
	acc := &nodes[0]
	ctx := c.Request.Context()
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	acctKey := "acct:" + itoa(acc.ID)

	sekey := ""
	if pwd != "" {
		if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		sekey, err = s.client.ShareVerify(ctx, token, surl, pwd)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": baidu.ShareErrMsg(err)})
			return
		}
	}
	if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	res, err := s.client.ShareList(ctx, token, surl, sekey, "")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": baidu.ShareErrMsg(err)})
		return
	}
	// fs_id/share_id/uk may exceed JS Number.MAX_SAFE_INTEGER: ship as strings.
	files := make([]gin.H, 0, len(res.List))
	for _, it := range res.List {
		files = append(files, gin.H{
			"fs_id": strconv.FormatUint(it.FsID, 10), "name": it.ServerFilename,
			"size": it.Size, "is_dir": it.IsDir == 1,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"surl": surl, "sekey": sekey,
		"share_id": strconv.FormatUint(res.ShareID, 10),
		"uk":       strconv.FormatUint(res.UK, 10),
		"files":    files,
	})
}

// handleShareImport starts an async 转存 job copying the selected shared files
// into the node that owns the target directory's mount.
func (s *Server) handleShareImport(c *gin.Context) {
	if !s.shareImportGuard(c) {
		return
	}
	var in struct {
		SURL      string   `json:"surl"`
		SeKey     string   `json:"sekey"`
		ShareID   string   `json:"share_id"`
		UK        string   `json:"uk"`
		FsIDs     []string `json:"fs_ids"`
		TotalSize int64    `json:"total_size"`
		TargetDir string   `json:"target_dir"`
		Encrypt   bool     `json:"encrypt"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	shareID, _ := strconv.ParseUint(in.ShareID, 10, 64)
	uk, _ := strconv.ParseUint(in.UK, 10, 64)
	fsids := make([]uint64, 0, len(in.FsIDs))
	for _, s := range in.FsIDs {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil && v != 0 {
			fsids = append(fsids, v)
		}
	}
	if in.SURL == "" || shareID == 0 || uk == 0 || len(fsids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先解析分享链接并选择要转存的文件"})
		return
	}
	acc, _, clean, err := s.resolveMount(in.TargetDir)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if s.accessDenied(c, clean) {
		return
	}
	if in.TotalSize < 0 {
		in.TotalSize = 0
	}
	reserved, err := s.pool.PickByIDForSize(c.Request.Context(), acc.ID, in.TotalSize)
	if err != nil {
		if errors.Is(err, account.ErrNoAccount) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "目标节点容量不足或不可用"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	jobID, err := s.jobs.SubmitShareImport(job.ShareImportSpec{
		UserID: uidOf(c), AccountID: reserved.ID,
		SURL: in.SURL, SeKey: in.SeKey, ShareID: shareID, UK: uk,
		FsIDs: fsids, Target: clean, MountRoot: nodeMountPath(*reserved),
		Encrypt: in.Encrypt,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, uidOf(c), usernameOf(c), "share.import",
		"target="+clean+" files="+strconv.Itoa(len(in.FsIDs))+
			" bytes="+strconv.FormatInt(in.TotalSize, 10)+
			" encrypt="+strconv.FormatBool(in.Encrypt))
	c.JSON(http.StatusOK, gin.H{"job_id": jobID})
}
