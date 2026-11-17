package api

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"gosocode/internal/crypto"
	"gosocode/internal/manifest"
	"gosocode/internal/models"
)

// cleanLogical normalizes a user-supplied logical path to a canonical absolute
// form (defeats "../" and duplicate slashes). Root resolves to "/".
func cleanLogical(p string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.TrimSpace(p), "/"))
}

func errString(err error) string {
	if err == nil {
		return "直链为空"
	}
	return err.Error()
}

func mountLabel(acc models.BaiduAccount) string {
	name := strings.TrimSpace(acc.MountName)
	if name == "" {
		name = strings.TrimSpace(acc.Nickname)
	}
	if name == "" {
		name = "百度网盘"
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", "\x00", "")
	name = strings.TrimSpace(replacer.Replace(name))
	if name == "" {
		name = "百度网盘"
	}
	return name
}

func mountName(acc models.BaiduAccount) string {
	return mountLabel(acc)
}

func normalizeMountPath(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" {
		return "/", nil
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", fmt.Errorf("挂载目录包含无效字符")
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", fmt.Errorf("挂载目录不能包含 ..")
		}
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	clean := path.Clean(raw)
	if len(clean) > 1024 {
		return "", fmt.Errorf("挂载目录不能超过 1024 个字符")
	}
	return clean, nil
}

func nodeMountPath(acc models.BaiduAccount) string {
	p, err := normalizeMountPath(acc.MountPath)
	if err != nil {
		return "/"
	}
	return p
}

func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func (s *Server) activeNodes() ([]models.BaiduAccount, error) {
	var nodes []models.BaiduAccount
	err := s.db.Where("status = ?", "active").Order("id asc").Find(&nodes).Error
	return nodes, err
}

func (s *Server) resolveMount(logical string) (*models.BaiduAccount, string, string, error) {
	p := cleanLogical(logical)
	if p == "/" {
		return nil, "", p, fmt.Errorf("请选择一个挂载节点")
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	nodes, err := s.activeNodes()
	if err != nil {
		return nil, "", p, err
	}
	var matched *models.BaiduAccount
	for _, n := range nodes {
		mount := mountName(n)
		if parts[0] != mount {
			continue
		}
		if matched != nil {
			return nil, "", p, fmt.Errorf("挂载名称重复，请由管理员重命名节点")
		}
		acc := n
		matched = &acc
	}
	if matched != nil {
		remote := nodeMountPath(*matched)
		if len(parts) > 1 {
			remote = path.Join(remote, strings.Join(parts[1:], "/"))
			base := nodeMountPath(*matched)
			if base != "/" && remote != base && !strings.HasPrefix(remote, base+"/") {
				return nil, "", p, fmt.Errorf("路径超出挂载目录")
			}
		}
		return matched, remote, p, nil
	}
	return nil, "", p, fmt.Errorf("挂载节点不存在")
}

// resolveRemote maps a logical path to its real remote path, honoring UUID
// mappings recorded in the File table: exact row first, then the deepest
// mapped directory ancestor with the remainder appended plainly. plainRemote
// (the resolveMount concatenation) is the fallback for fully unmapped paths,
// so legacy plain trees and mixed trees keep working.
func (s *Server) resolveRemote(acc *models.BaiduAccount, logical, plainRemote string) string {
	logical = cleanLogical(logical)
	var f models.File
	if err := s.db.Where("baidu_account_id = ? AND logical_path = ? AND status <> ?", acc.ID, logical, "deleted").
		Order("id desc").First(&f).Error; err == nil && f.RemotePath != "" {
		return f.RemotePath
	}
	ancestors := []string{}
	for p := path.Dir(logical); strings.Count(p, "/") >= 2; p = path.Dir(p) {
		ancestors = append(ancestors, p)
	}
	if len(ancestors) == 0 {
		return plainRemote
	}
	var dirs []models.File
	s.db.Where("baidu_account_id = ? AND is_dir = ? AND status <> ? AND logical_path IN ?",
		acc.ID, true, "deleted", ancestors).Find(&dirs)
	var best *models.File
	for i := range dirs {
		if dirs[i].RemotePath != "" && (best == nil || len(dirs[i].LogicalPath) > len(best.LogicalPath)) {
			best = &dirs[i]
		}
	}
	if best == nil {
		return plainRemote
	}
	return path.Join(best.RemotePath, strings.TrimPrefix(logical, best.LogicalPath))
}

// ensureUUIDDirs walks the logical directory chain below the mount root and
// get-or-creates a UUID directory mapping per missing level, returning the
// remote path of the deepest directory. Directories that already hold plain
// (non-UUID) platform children stay plain so their legacy content remains
// visible next to new uploads.
func (s *Server) ensureUUIDDirs(c *gin.Context, acc *models.BaiduAccount, logicalDir string) (string, error) {
	logicalDir = cleanLogical(logicalDir)
	if strings.Count(logicalDir, "/") < 2 { // the mount root itself
		return nodeMountPath(*acc), nil
	}
	parent, err := s.ensureUUIDDirs(c, acc, path.Dir(logicalDir))
	if err != nil {
		return "", err
	}
	ctx := c.Request.Context()
	unlock, err := s.coord.Locker.Lock(ctx, "dirmap:"+itoa(acc.ID)+":"+logicalDir, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer unlock()
	var d models.File
	if err := s.db.Where("baidu_account_id = ? AND logical_path = ? AND is_dir = ? AND status <> ?",
		acc.ID, logicalDir, true, "deleted").Order("id desc").First(&d).Error; err == nil && d.RemotePath != "" {
		return d.RemotePath, nil
	}
	var plainKids int64
	s.db.Model(&models.File{}).
		Where("baidu_account_id = ? AND status <> ? AND remote_uuid = ? AND logical_path LIKE ?",
			acc.ID, "deleted", "", logicalDir+"/%").Count(&plainKids)
	if plainKids > 0 {
		return path.Join(parent, path.Base(logicalDir)), nil
	}
	id := uuid.NewString()
	now := time.Now()
	d = models.File{
		OwnerUserID: uidOf(c), BaiduAccountID: acc.ID,
		LogicalPath: logicalDir, RemotePath: path.Join(parent, id), RemoteUUID: id,
		IsDir: true, Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.Create(&d).Error; err != nil {
		return "", err
	}
	// New directory mapping exists now (even before any file finishes): put it
	// in the cloud manifest right away.
	s.msync.Mark(acc.ID)
	return d.RemotePath, nil
}

func (s *Server) resolveUploadTarget(ctx *gin.Context, logical string, size int64) (*models.BaiduAccount, string, string, string, func(), error) {
	acc, remote, clean, err := s.resolveMount(logical)
	if err != nil {
		return nil, "", "", clean, nil, err
	}
	remoteUUID := ""
	if s.systemConfig().UUIDStorage {
		// Overwrite of an existing UUID file keeps its remote name (rtype=3
		// replaces in place, no orphan blob).
		var prev models.File
		if err := s.db.Where("baidu_account_id = ? AND logical_path = ? AND is_dir = ? AND status <> ? AND remote_uuid <> ?",
			acc.ID, clean, false, "deleted", "").Order("id desc").First(&prev).Error; err == nil {
			remote, remoteUUID = prev.RemotePath, prev.RemoteUUID
		} else {
			parentRemote, derr := s.ensureUUIDDirs(ctx, acc, path.Dir(clean))
			if derr != nil {
				return nil, "", "", clean, nil, derr
			}
			remoteUUID = uuid.NewString()
			remote = path.Join(parentRemote, remoteUUID)
		}
	} else {
		// Toggle off: still respect UUID-mapped parents from earlier.
		remote = s.resolveRemote(acc, clean, remote)
	}
	reserved, err := s.pool.PickByIDForSize(ctx.Request.Context(), acc.ID, size)
	if err != nil {
		return nil, "", "", clean, nil, err
	}
	return reserved, remote, remoteUUID, clean, func() { s.pool.Release(reserved.ID) }, nil
}

// handleListFiles lists the current logical directory. The logical root exposes
// mounted Baidu accounts as folders; inside a mount, listing is read live from
// that Baidu account's netdisk root instead of the local metadata table.
func (s *Server) handleListFiles(c *gin.Context) {
	logical := cleanLogical(c.DefaultQuery("path", "/"))
	if logical == "/" {
		nodes, err := s.activeNodes()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out := make([]gin.H, 0, len(nodes))
		for _, n := range nodes {
			m := mountName(n)
			out = append(out, gin.H{
				"id": 0, "path": "/" + m, "size": 0, "status": "ready",
				"created_at": n.CreatedAt, "modified": n.UpdatedAt, "is_dir": true,
				"owner": "百度网盘", "node_id": n.ID, "mount": true,
			})
		}
		c.JSON(http.StatusOK, gin.H{"files": out})
		return
	}

	acc, remoteDir, _, err := s.resolveMount(logical)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if s.accessDenied(c, logical) {
		return
	}
	ctx := c.Request.Context()
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	remoteDir = s.resolveRemote(acc, logical, remoteDir)
	items, err := s.client.ListDir(ctx, token, remoteDir)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	mount := strings.TrimSuffix(logical, "/")

	// Reverse-map UUID-named remote items to their logical names in one query.
	remotePaths := make([]string, 0, len(items))
	for _, it := range items {
		remotePaths = append(remotePaths, it.Path)
	}
	rowByRemote := map[string]models.File{}
	if len(remotePaths) > 0 {
		var rows []models.File
		s.db.Where("baidu_account_id = ? AND status <> ? AND remote_path IN ?", acc.ID, "deleted", remotePaths).
			Order("id asc").Find(&rows)
		for _, r := range rows {
			rowByRemote[r.RemotePath] = r // newest row wins on duplicates
		}
	}

	type listEntry struct {
		id        uint64
		childPath string
		size      int64
		isDir     bool
		remote    string
		created   time.Time
		modified  time.Time
		encrypted bool
	}
	entries := make([]listEntry, 0, len(items))
	seenRemote := map[string]bool{}
	seenChild := map[string]bool{}
	for _, it := range items {
		name := it.ServerFilename
		if name == "" {
			name = path.Base(it.Path)
		}
		if name == manifest.FileName { // system metadata file: never listed
			continue
		}
		if name == models.ImportsDirName { // share-import staging dir: hidden
			continue
		}
		childPath := mount + "/" + name
		enc := false
		if r, ok := rowByRemote[it.Path]; ok {
			childPath = r.LogicalPath
			enc = r.Encrypted
		}
		entries = append(entries, listEntry{
			id: it.FsID, childPath: childPath, size: it.Size,
			isDir: it.IsDir == 1, remote: it.Path,
			created: unixTime(it.ServerCTime), modified: unixTime(it.ServerMTime),
			encrypted: enc,
		})
		seenRemote[it.Path] = true
		seenChild[childPath] = true
	}

	// Overlay platform entries whose bytes live outside this directory's
	// resolved remote dir (mixed legacy/UUID trees), e.g. UUID children of a
	// plain directory.
	var extra []models.File
	s.db.Where("baidu_account_id = ? AND status <> ? AND logical_path LIKE ? AND logical_path NOT LIKE ?",
		acc.ID, "deleted", mount+"/%", mount+"/%/%").Order("id asc").Find(&extra)
	for _, f := range extra {
		if seenRemote[f.RemotePath] || seenChild[f.LogicalPath] {
			continue
		}
		seenChild[f.LogicalPath] = true
		entries = append(entries, listEntry{
			id: uint64(f.ID), childPath: f.LogicalPath, size: f.Size,
			isDir: f.IsDir, remote: f.RemotePath,
			created: f.CreatedAt, modified: f.UpdatedAt, encrypted: f.Encrypted,
		})
	}

	// One query marks the direct children that carry their own password rule,
	// so the UI can show lock badges without N lookups.
	childPaths := make([]string, 0, len(entries))
	for _, e := range entries {
		childPaths = append(childPaths, e.childPath)
	}
	ruleByPath := map[string]models.AccessRule{}
	if len(childPaths) > 0 {
		var rules []models.AccessRule
		s.db.Where("path IN ?", childPaths).Find(&rules)
		for _, r := range rules {
			ruleByPath[r.Path] = r
		}
	}
	// Same trick for at-rest encryption: the flag lives in the local File
	// metadata, so one IN query marks encrypted children for the UI badge.
	encByPath := map[string]bool{}
	if len(childPaths) > 0 {
		var encPaths []string
		s.db.Model(&models.File{}).
			Where("logical_path IN ? AND encrypted = ? AND status <> ?", childPaths, true, "deleted").
			Pluck("logical_path", &encPaths)
		for _, p := range encPaths {
			encByPath[p] = true
		}
	}
	uid := uidOf(c)

	out := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		row := gin.H{
			"id": e.id, "path": e.childPath, "size": e.size,
			"status": "ready", "created_at": e.created, "modified": e.modified,
			"is_dir": e.isDir, "owner": acc.Nickname,
			"node_id": acc.ID, "remote_path": e.remote,
			"download_url": fmt.Sprintf("/api/files/remote-download?path=%s", pathEscape(e.childPath)),
		}
		if r, ok := ruleByPath[e.childPath]; ok {
			row["protected"] = true
			// free (passwordless) access is owner-only; admins manage via role
			row["protected_own"] = r.OwnerUserID == uid
			row["protected_by"] = r.OwnerName
		}
		if e.encrypted || encByPath[e.childPath] {
			row["encrypted"] = true
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"files": out})
}

func pathEscape(p string) string {
	return url.QueryEscape(p)
}

func likeContains(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, `%`, `\%`)
	q = strings.ReplaceAll(q, `_`, `\_`)
	return "%" + q + "%"
}

func hiddenLogical(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == models.ImportsDirName || seg == manifest.FileName {
			return true
		}
	}
	return false
}

func searchScore(logical, q string) int {
	name := strings.ToLower(path.Base(logical))
	ql := strings.ToLower(q)
	switch {
	case name == ql:
		return 0
	case strings.HasPrefix(name, ql):
		return 1
	case strings.Contains(name, ql):
		return 2
	case strings.Contains(strings.ToLower(logical), ql):
		return 3
	default:
		return 4
	}
}

func pathCoveredByGrant(logical string, grants []string) bool {
	for _, g := range grants {
		if logical == g || strings.HasPrefix(logical, g+"/") {
			return true
		}
	}
	return false
}

func coveringRule(logical string, rules []models.AccessRule) *models.AccessRule {
	var best *models.AccessRule
	for i := range rules {
		r := &rules[i]
		if logical == r.Path || strings.HasPrefix(logical, r.Path+"/") {
			if best == nil || len(r.Path) > len(best.Path) {
				best = r
			}
		}
	}
	return best
}

func queryTokens(q string) []string {
	return strings.Fields(strings.TrimSpace(q))
}

// matchQuery is a case-insensitive substring ("fuzzy") match: every
// whitespace-separated token must appear in the logical path (file or directory names).
func matchQuery(logical, q string) bool {
	toks := queryTokens(q)
	if len(toks) == 0 {
		return false
	}
	target := strings.ToLower(logical)
	for _, tok := range toks {
		if !strings.Contains(target, strings.ToLower(tok)) {
			return false
		}
	}
	return true
}

func primarySearchKey(q string) string {
	toks := queryTokens(q)
	if len(toks) == 0 {
		return strings.TrimSpace(q)
	}
	best := toks[0]
	for _, t := range toks[1:] {
		if len([]rune(t)) > len([]rune(best)) {
			best = t
		}
	}
	return best
}

// remoteToLogical maps a netdisk path under a node's mount root to the
// user-facing logical path ("/<mount>/..."). Empty means the remote path
// is outside this mount.
func remoteToLogical(mount, root, remotePath string) string {
	remotePath = path.Clean(remotePath)
	root = path.Clean(root)
	if root == "/" {
		if remotePath == "/" {
			return mount
		}
		return path.Clean(mount + remotePath)
	}
	if remotePath == root {
		return mount
	}
	if strings.HasPrefix(remotePath, root+"/") {
		return path.Clean(mount + strings.TrimPrefix(remotePath, root))
	}
	return ""
}

type searchCand struct {
	path      string
	id        uint
	size      int64
	isDir     bool
	created   time.Time
	modified  time.Time
	encrypted bool
	remote    string
	acctID    uint
	status    string
	mount     bool
	fsID      uint64
}

// mergeRemoteSearch runs a recursive Baidu search on one node and adds hits
// that are not already present. Failures are logged and skipped so a single
// node outage does not empty the whole result set.
func (s *Server) mergeRemoteSearch(c *gin.Context, n models.BaiduAccount, q string, temporary bool, grantPaths []string, seen map[string]bool, add func(searchCand)) {
	if s.client == nil || s.pool == nil || s.coord == nil {
		return
	}
	ctx := c.Request.Context()
	token, err := s.pool.Token(ctx, &n)
	if err != nil {
		log.Printf("search: token for node %d: %v", n.ID, err)
		return
	}
	if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(n.ID)); err != nil {
		log.Printf("search: limiter for node %d: %v", n.ID, err)
		return
	}
	items, err := s.client.Search(ctx, token, nodeMountPath(n), primarySearchKey(q))
	if err != nil {
		log.Printf("search: baidu node %d: %v", n.ID, err)
		return
	}
	remotePaths := make([]string, 0, len(items))
	for _, it := range items {
		remotePaths = append(remotePaths, it.Path)
	}
	rowByRemote := map[string]models.File{}
	if len(remotePaths) > 0 {
		var mapped []models.File
		s.db.Where("baidu_account_id = ? AND status <> ? AND remote_path IN ?", n.ID, "deleted", remotePaths).
			Order("id asc").Find(&mapped)
		for _, r := range mapped {
			rowByRemote[r.RemotePath] = r
		}
	}
	mount := "/" + mountName(n)
	root := nodeMountPath(n)
	for _, it := range items {
		name := it.ServerFilename
		if name == "" {
			name = path.Base(it.Path)
		}
		if name == manifest.FileName || name == models.ImportsDirName || hiddenLogical(it.Path) {
			continue
		}
		logical := ""
		enc := false
		var fileID uint
		var remote string
		if r, ok := rowByRemote[it.Path]; ok {
			logical = r.LogicalPath
			enc = r.Encrypted
			fileID = r.ID
			remote = r.RemotePath
		} else {
			logical = remoteToLogical(mount, root, it.Path)
			remote = it.Path
		}
		logical = cleanLogical(logical)
		if logical == "/" || logical == "" || seen[logical] || hiddenLogical(logical) {
			continue
		}
		// Prefer the logical (user-facing) name. A UUID remote name should
		// not surface unless the logical path itself matches the query.
		if !matchQuery(logical, q) {
			continue
		}
		if temporary && !pathCoveredByGrant(logical, grantPaths) {
			continue
		}
		seen[logical] = true
		add(searchCand{
			path: logical, id: fileID, size: it.Size, isDir: it.IsDir == 1,
			created: unixTime(it.ServerCTime), modified: unixTime(it.ServerMTime),
			encrypted: enc, remote: remote, acctID: n.ID, status: "ready",
			fsID: it.FsID,
		})
	}
}

// handleSearchFiles finds files and directories by name or logical path.
// It searches local metadata and, because the file manager lists live from
// Baidu, also runs a recursive netdisk search on each active node so items
// without a File row still show up. Password-protected descendants are
// omitted for non-owners so names inside a locked folder are not leaked;
// a match on the protected item itself is returned and flagged.
func (s *Server) handleSearchFiles(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusOK, gin.H{"files": []gin.H{}})
		return
	}
	if len([]rune(q)) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "搜索关键词过长"})
		return
	}

	role, _ := c.Get("role")
	uid := uidOf(c)
	var grantPaths []string
	if role == "temporary" {
		var grants []models.AccessGrant
		now := time.Now()
		s.db.Where("user_id = ? AND (expires_at IS NULL OR expires_at > ?)", uid, now).Find(&grants)
		for _, g := range grants {
			grantPaths = append(grantPaths, cleanLogical(g.Path))
		}
	}

	var rules []models.AccessRule
	s.db.Find(&rules)
	pwd := c.Query("pwd")
	if pwd == "" {
		pwd = c.GetHeader("X-Access-Password")
	}

	query := s.db.Model(&models.File{}).Where("status <> ? AND status <> ?", "deleted", "uploading")
	for _, tok := range queryTokens(q) {
		query = query.Where("logical_path LIKE ? ESCAPE ?", likeContains(tok), `\`)
	}
	if role == "temporary" {
		if len(grantPaths) == 0 {
			query = query.Where("1 = 0")
		} else {
			var conds []string
			var args []any
			for _, g := range grantPaths {
				conds = append(conds, "(logical_path = ? OR logical_path LIKE ?)")
				args = append(args, g, g+"/%")
			}
			query = query.Where(strings.Join(conds, " OR "), args...)
		}
	}

	var rows []models.File
	if err := query.Order("logical_path asc").Limit(300).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	acctIDs := map[uint]struct{}{}
	cands := make([]searchCand, 0, len(rows)+4)
	seen := map[string]bool{}
	for _, f := range rows {
		p := cleanLogical(f.LogicalPath)
		if p == "/" || hiddenLogical(p) || seen[p] {
			continue
		}
		seen[p] = true
		cands = append(cands, searchCand{
			path: p, id: f.ID, size: f.Size, isDir: f.IsDir,
			created: f.CreatedAt, modified: f.UpdatedAt, encrypted: f.Encrypted,
			remote: f.RemotePath, acctID: f.BaiduAccountID, status: f.Status,
		})
		if f.BaiduAccountID != 0 {
			acctIDs[f.BaiduAccountID] = struct{}{}
		}
	}

	nodes, err := s.activeNodes()
	if err == nil {
		for _, n := range nodes {
			m := mountName(n)
			p := "/" + m
			if !seen[p] && matchQuery(m, q) {
				if role != "temporary" || pathCoveredByGrant(p, grantPaths) {
					seen[p] = true
					cands = append(cands, searchCand{
						path: p, isDir: true, created: n.CreatedAt, modified: n.UpdatedAt,
						acctID: n.ID, status: "ready", mount: true,
					})
					acctIDs[n.ID] = struct{}{}
				}
			}
			s.mergeRemoteSearch(c, n, q, role == "temporary", grantPaths, seen, func(item searchCand) {
				cands = append(cands, item)
				if item.acctID != 0 {
					acctIDs[item.acctID] = struct{}{}
				}
			})
		}
	}

	ids := make([]uint, 0, len(acctIDs))
	for id := range acctIDs {
		ids = append(ids, id)
	}
	acctByID := map[uint]models.BaiduAccount{}
	if len(ids) > 0 {
		var accts []models.BaiduAccount
		s.db.Where("id IN ?", ids).Find(&accts)
		for _, a := range accts {
			acctByID[a.ID] = a
		}
	}

	type scored struct {
		item  gin.H
		score int
		path  string
	}
	out := make([]scored, 0, len(cands))
	for _, e := range cands {
		rule := coveringRule(e.path, rules)
		protected, own, by := false, false, ""
		if rule != nil {
			own = rule.OwnerUserID == uid
			protected = true
			by = rule.OwnerName
			if !own {
				ok := false
				if pwd != "" {
					plain, err := crypto.Unwrap(s.systemConfig().MasterKey, rule.PasswordEnc)
					ok = err == nil && plain == pwd
				}
				if !ok && e.path != rule.Path {
					// do not leak names inside a locked folder
					continue
				}
			}
		}
		owner := ""
		if acc, ok := acctByID[e.acctID]; ok {
			owner = acc.Nickname
			if e.mount {
				owner = "百度网盘"
			}
		}
		status := e.status
		if status == "" {
			status = "ready"
		}
		row := gin.H{
			"id": e.id, "path": e.path, "size": e.size,
			"status": status, "created_at": e.created, "modified": e.modified,
			"is_dir": e.isDir, "owner": owner, "node_id": e.acctID,
		}
		if e.remote != "" {
			row["remote_path"] = e.remote
		}
		if !e.isDir {
			row["download_url"] = fmt.Sprintf("/api/files/remote-download?path=%s", pathEscape(e.path))
		}
		if e.mount {
			row["mount"] = true
		}
		if e.encrypted {
			row["encrypted"] = true
		}
		if protected {
			row["protected"] = true
			row["protected_own"] = own
			row["protected_by"] = by
		}
		out = append(out, scored{item: row, score: searchScore(e.path, q), path: e.path})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].path < out[j].path
	})
	const maxResults = 80
	files := make([]gin.H, 0, len(out))
	for i, it := range out {
		if i >= maxResults {
			break
		}
		files = append(files, it.item)
	}
	c.JSON(http.StatusOK, gin.H{"files": files})
}

func (s *Server) remoteItemByPath(ctx *gin.Context, acc *models.BaiduAccount, token, remotePath string) (*baiduListRef, error) {
	dir, name := path.Dir(remotePath), path.Base(remotePath)
	if dir == "." {
		dir = "/"
	}
	if err := s.coord.Limiter.Wait(ctx.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
		return nil, err
	}
	items, err := s.client.ListDir(ctx.Request.Context(), token, dir)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.Path == remotePath || it.ServerFilename == name {
			return &baiduListRef{
				FsID: it.FsID, Name: it.ServerFilename, Size: it.Size,
				IsDir: it.IsDir == 1, MTime: it.ServerMTime,
			}, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

type baiduListRef struct {
	FsID  uint64
	Name  string
	Size  int64
	IsDir bool
	MTime int64
}

// fileByRemote finds the metadata row for a remote path on a node; nil when the
// file was never uploaded through the platform (e.g. pre-existing Baidu data).
func (s *Server) fileByRemote(accID uint, remotePath string) *models.File {
	var f models.File
	if err := s.db.Where("baidu_account_id = ? AND remote_path = ? AND status = ?", accID, remotePath, "ready").
		Order("id desc").First(&f).Error; err != nil {
		return nil
	}
	return &f
}

// fileEncKeys unwraps a file's per-file AES key and CTR IV.
func (s *Server) fileEncKeys(f *models.File) (key, iv []byte, err error) {
	mk := s.systemConfig().MasterKey
	keyHex, err := crypto.Unwrap(mk, f.EncKey)
	if err != nil {
		return nil, nil, err
	}
	ivHex, err := crypto.Unwrap(mk, f.EncIV)
	if err != nil {
		return nil, nil, err
	}
	if key, err = hex.DecodeString(keyHex); err != nil {
		return nil, nil, err
	}
	if iv, err = hex.DecodeString(ivHex); err != nil {
		return nil, nil, err
	}
	return key, iv, nil
}

// decryptedBody wraps the upstream response body with the file's CTR stream
// when the file is stored encrypted. The stream is positioned at the byte
// offset the upstream actually serves (Content-Range start for 206 answers).
func (s *Server) decryptedBody(resp *http.Response, f *models.File) (io.Reader, error) {
	if f == nil || !f.Encrypted {
		return resp.Body, nil
	}
	key, iv, err := s.fileEncKeys(f)
	if err != nil {
		return nil, err
	}
	var off int64
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if _, err := fmt.Sscanf(cr, "bytes %d-", &off); err != nil {
			off = 0
		}
	}
	return crypto.NewCTRReader(resp.Body, key, iv, off)
}

func (s *Server) proxyDownload(c *gin.Context, uid uint, acc *models.BaiduAccount, token string, fsID uint64, fallbackName, logLabel string, encFile *models.File) int64 {
	ctx := c.Request.Context()
	var resp *http.Response
	filename := fallbackName
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return 0
		}
		metas, merr := s.client.FileMetas(ctx, token, []uint64{fsID})
		if merr != nil || len(metas) == 0 || metas[0].Dlink == "" {
			log.Printf("[download] resolve dlink failed %s node=%d: %v", logLabel, acc.ID, merr)
			c.JSON(http.StatusBadGateway, gin.H{"error": "无法从云端获取下载地址: " + errString(merr)})
			return 0
		}
		// UUID-named storage: the cloud-side name is an opaque uuid, the real
		// name lives on the metadata row.
		if metas[0].Filename != "" && (encFile == nil || encFile.RemoteUUID == "") {
			filename = metas[0].Filename
		}
		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return 0
		}
		resp, err = s.client.OpenDownload(ctx, token, metas[0].Dlink, c.GetHeader("Range"))
		if err != nil {
			log.Printf("[download] open upstream failed %s node=%d: %v", logLabel, acc.ID, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return 0
		}
		if resp.StatusCode < 400 {
			break
		}
		resp.Body.Close()
		resp = nil
		log.Printf("[download] upstream HTTP error %s node=%d attempt=%d, re-resolving dlink", logLabel, acc.ID, attempt+1)
	}
	if resp == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "云端拒绝下载（直链失效），请重试"})
		return 0
	}
	defer resp.Body.Close()

	if filename == "" {
		filename = fmt.Sprintf("file-%d", fsID)
	}
	body, err := s.decryptedBody(resp, encFile)
	if err != nil {
		log.Printf("[download] decrypt init failed %s node=%d: %v", logLabel, acc.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法解密文件"})
		return 0
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Accept-Ranges", "bytes")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		c.Header("Content-Range", cr)
	}
	c.Status(resp.StatusCode)

	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 256*1024)
	var n int64
	var cerr error
	for {
		rn, rerr := body.Read(buf)
		if rn > 0 {
			if _, werr := c.Writer.Write(buf[:rn]); werr != nil {
				cerr = werr
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			n += int64(rn)
			s.meter.AddDown(int64(rn))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cerr = rerr
			break
		}
	}
	if cerr != nil {
		log.Printf("[download] stream interrupted %s node=%d after %d bytes: %v", logLabel, acc.ID, n, cerr)
	}
	if n > 0 {
		s.db.Model(&models.User{}).Where("id = ?", uid).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
		s.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
	}
	return n
}

func (s *Server) handleRemoteDownload(c *gin.Context) {
	logical := c.Query("path")
	acc, remotePath, clean, err := s.resolveMount(logical)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if path.Base(clean) == manifest.FileName {
		c.JSON(http.StatusForbidden, gin.H{"error": "系统文件不可访问"})
		return
	}
	if s.accessDenied(c, clean) {
		return
	}
	remotePath = s.resolveRemote(acc, clean, remotePath)
	token, err := s.pool.Token(c.Request.Context(), acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ref, err := s.remoteItemByPath(c, acc, token, remotePath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		return
	}
	if ref.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能下载文件夹"})
		return
	}
	s.logTransferEvent(&models.TransferEvent{
		UserID: uidOf(c), Kind: "download", Stage: "start", RefType: "file",
		Name: path.Base(clean), Path: clean, AccountID: acc.ID, Bytes: ref.Size,
	})
	n := s.proxyDownload(c, uidOf(c), acc, token, ref.FsID, path.Base(clean), "path="+clean, s.fileByRemote(acc.ID, remotePath))
	s.logTransferEvent(&models.TransferEvent{
		UserID: uidOf(c), Kind: "download", Stage: "finish", RefType: "file",
		Name: path.Base(clean), Path: clean, AccountID: acc.ID, Bytes: n,
	})
}

// handleCheckAccess verifies that the caller may access a protected file or path.
// The UI calls this before enqueueing a download so a wrong password is reported
// immediately instead of creating a transfer that fails with a network error.
// GET /api/files/access-check?id=<file_id>&pwd=... or ?path=<logical>&pwd=...
func (s *Server) handleCheckAccess(c *gin.Context) {
	if idStr := c.Query("id"); idStr != "" {
		id, _ := strconv.Atoi(idStr)
		var f models.File
		if err := s.db.First(&f, id).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if f.Status != "ready" {
			c.JSON(http.StatusConflict, gin.H{"error": "file not ready"})
			return
		}
		if s.accessDenied(c, f.LogicalPath) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	if p := c.Query("path"); p != "" {
		_, _, clean, err := s.resolveMount(p)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if s.accessDenied(c, clean) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "id or path required"})
}

// handleDownload streams a file's bytes through the platform (proxy mode), so
// the raw Baidu dlink/token never reaches the client. Range is forwarded to
// support resumable/multi-threaded downloads.
func (s *Server) handleDownload(c *gin.Context) {
	uid := uidOf(c)
	id, _ := strconv.Atoi(c.Param("id"))

	var f models.File
	// shared filesystem: any authenticated user may fetch any file.
	if err := s.db.First(&f, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if f.Status != "ready" {
		c.JSON(http.StatusConflict, gin.H{"error": "file not ready"})
		return
	}
	if s.accessDenied(c, f.LogicalPath) {
		return
	}

	var offsetStart, offsetEnd int64
	parseRangeHeader(c.GetHeader("Range"), f.Size, &offsetStart, &offsetEnd)
	s.logTransferEvent(
		&models.TransferEvent{
			UserID: uid, Kind: "download", Stage: "start", RefType: "file",
			RefID: strconv.Itoa(id), Name: path.Base(f.LogicalPath), Path: f.LogicalPath,
			AccountID: f.BaiduAccountID, OffsetStart: offsetStart, OffsetEnd: offsetEnd, Bytes: offsetEnd - offsetStart,
		})

	ctx := c.Request.Context()
	acc, err := s.pool.Get(ctx, f.BaiduAccountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Resolve the dlink and open the upstream stream. dlinks are temporary
	// (~8h) and can also be invalidated early; on a 4xx from the CDN we
	// re-resolve once and retry, so a stale cached dlink never fails the user.
	var resp *http.Response
	filename := ""
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		metas, merr := s.client.FileMetas(ctx, token, []uint64{f.FsID})
		if merr != nil || len(metas) == 0 || metas[0].Dlink == "" {
			log.Printf("[download] resolve dlink failed file=%d node=%d: %v", f.ID, acc.ID, merr)
			c.JSON(http.StatusBadGateway, gin.H{"error": "无法从云端获取下载地址: " + errString(merr)})
			return
		}
		if f.RemoteUUID != "" { // cloud-side name is an opaque uuid
			filename = path.Base(f.LogicalPath)
		} else {
			filename = metas[0].Filename
		}

		if err := s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		resp, err = s.client.OpenDownload(ctx, token, metas[0].Dlink, c.GetHeader("Range"))
		if err != nil {
			log.Printf("[download] open upstream failed file=%d node=%d: %v", f.ID, acc.ID, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		if resp.StatusCode < 400 {
			break
		}
		resp.Body.Close()
		resp = nil
		log.Printf("[download] upstream HTTP error file=%d node=%d attempt=%d, re-resolving dlink", f.ID, acc.ID, attempt+1)
	}
	if resp == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "云端拒绝下载（直链失效），请重试"})
		return
	}
	defer resp.Body.Close()

	if filename == "" {
		filename = fmt.Sprintf("file-%d", f.ID)
	}
	body, err := s.decryptedBody(resp, &f)
	if err != nil {
		log.Printf("[download] decrypt init failed file=%d node=%d: %v", f.ID, acc.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法解密文件"})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Accept-Ranges", "bytes")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		c.Header("Content-Range", cr)
	}
	c.Status(resp.StatusCode)

	// Relay cloud->client with a small buffer and an explicit Flush after every
	// write: nothing accumulates server-side, so the client receives bytes at
	// the same pace the cloud delivers them, and a slow client back-pressures
	// the cloud read (read and write alternate on the same buffer).
	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 256*1024)
	var n int64
	var cerr error
	for {
		rn, rerr := body.Read(buf)
		if rn > 0 {
			if _, werr := c.Writer.Write(buf[:rn]); werr != nil {
				cerr = werr
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			n += int64(rn)
			s.meter.AddDown(int64(rn)) // live meter: realtime chart sees the flow
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cerr = rerr
			break
		}
	}
	if cerr != nil {
		// Client disconnect or upstream drop mid-stream; the client will resume
		// via Range. Log for visibility.
		log.Printf("[download] stream interrupted file=%d node=%d after %d bytes: %v", f.ID, acc.ID, n, cerr)
	}

	// Traffic accounting for the download (live meter already fed in the loop).
	if n > 0 {
		s.db.Model(&models.User{}).Where("id = ?", uid).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
		s.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).
			UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", n))
	}
	s.logTransferEvent(
		&models.TransferEvent{
			UserID: uid, Kind: "download", Stage: "finish", RefType: "file",
			RefID: strconv.Itoa(id), Name: path.Base(f.LogicalPath), Path: f.LogicalPath,
			AccountID: f.BaiduAccountID, OffsetStart: offsetStart, OffsetEnd: offsetStart + n, Bytes: n,
		})
}

// handleDeleteFile removes a file from its storage node and marks it deleted.
func (s *Server) handleDeleteFile(c *gin.Context) {
	uid := uidOf(c)
	id, _ := strconv.Atoi(c.Param("id"))
	var f models.File
	if err := s.db.First(&f, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if s.accessDenied(c, f.LogicalPath) {
		return
	}
	ctx := c.Request.Context()
	acc, err := s.pool.Get(ctx, f.BaiduAccountID)
	if err == nil {
		if token, terr := s.pool.Token(ctx, acc); terr == nil {
			_ = s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID))
			// best-effort remote delete; metadata is authoritative either way
			_ = s.client.Delete(ctx, token, []string{f.RemotePath})
			s.pool.AddUsed(acc.ID, -f.Size)
		}
	}
	s.db.Model(&f).Update("status", "deleted")
	s.dropAccessRules(cleanLogical(f.LogicalPath))
	s.msync.Mark(f.BaiduAccountID)
	s.audit(c, uid, usernameOf(c), "file.delete", f.LogicalPath)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleRenameFile changes only the logical (user-facing) path; the bytes stay
// on the same node. Keeps the metadata layer authoritative.
func (s *Server) handleRenameFile(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var f models.File
	if err := s.db.First(&f, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if s.accessDenied(c, f.LogicalPath) {
		return
	}
	if path.Base(f.LogicalPath) == manifest.FileName || path.Base(cleanLogical(in.Path)) == manifest.FileName {
		c.JSON(http.StatusForbidden, gin.H{"error": "系统文件不可操作"})
		return
	}
	s.db.Model(&f).Update("logical_path", in.Path)
	s.moveAccessRules(cleanLogical(f.LogicalPath), cleanLogical(in.Path))
	s.movePathNotes(cleanLogical(f.LogicalPath), cleanLogical(in.Path))
	s.msync.Mark(f.BaiduAccountID)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleDeletePath(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能删除根目录"})
		return
	}
	if path.Base(p) == manifest.FileName {
		c.JSON(http.StatusForbidden, gin.H{"error": "系统文件不可删除"})
		return
	}
	if s.accessDenied(c, p) {
		return
	}
	acc, remotePath, _, err := s.resolveMount(p)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	remotePath = s.resolveRemote(acc, p, remotePath)
	token, err := s.pool.Token(c.Request.Context(), acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if err := s.client.Delete(c.Request.Context(), token, []string{remotePath}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	s.db.Model(&models.File{}).Where("(logical_path = ? OR logical_path LIKE ?) AND status != ?", p, p+"/%", "deleted").
		Update("status", "deleted")
	s.dropAccessRules(p)
	s.dropPathNotes(p, true)
	s.msync.Mark(acc.ID)
	s.audit(c, uid, usernameOf(c), "file.delete", p)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleCancelJob marks a still-pending job cancelled. Running transfers are
// left to finish (the worker owns them); this covers queued items.
func (s *Server) handleCancelJob(c *gin.Context) {
	uid := uidOf(c)
	id := c.Param("id")
	res := s.db.Model(&models.Job{}).
		Where("id = ? AND user_id = ? AND status = ?", id, uid, "pending").
		Updates(map[string]any{"status": "failed", "error": "cancelled by user"})
	if res.RowsAffected == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "job not cancellable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleListJobs(c *gin.Context) {
	uid := uidOf(c)
	var jobs []models.Job
	s.db.Where("user_id = ?", uid).Order("created_at desc").Limit(100).Find(&jobs)
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}

// handleStorage reports the aggregate capacity of the shared filesystem across
// all active storage nodes. Used for the capacity bar and pre-upload checks.
func (s *Server) handleStorage(c *gin.Context) {
	var res struct{ Total, Used int64 }
	s.db.Model(&models.BaiduAccount{}).Where("status = ?", "active").
		Select("COALESCE(SUM(quota_total),0) as total, COALESCE(SUM(quota_used),0) as used").Scan(&res)
	free := res.Total - res.Used
	if free < 0 {
		free = 0
	}
	c.JSON(http.StatusOK, gin.H{"total": res.Total, "used": res.Used, "free": free})
}

// handleCreateFolder creates a metadata-only (empty) directory record so folders
// can exist before any file is placed in them.
func (s *Server) handleCreateFolder(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先进入一个挂载节点"})
		return
	}
	// Creating inside a protected directory requires the access password.
	parent := path.Dir(p)
	if parent != "/" && s.accessDenied(c, parent) {
		return
	}
	acc, remotePath, _, err := s.resolveMount(p)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if s.systemConfig().UUIDStorage {
		// ensureUUIDDirs creates the whole chain (mapping rows); the deepest
		// level is this folder itself. MakeDir materializes it remotely so an
		// empty folder is browsable immediately.
		remotePath, err = s.ensureUUIDDirs(c, acc, p)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		remotePath = s.resolveRemote(acc, p, remotePath)
	}
	token, err := s.pool.Token(c.Request.Context(), acc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if err := s.client.MakeDir(c.Request.Context(), token, remotePath); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	s.msync.Mark(acc.ID)
	s.audit(c, uid, usernameOf(c), "folder.create", p)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleMove relocates a file or a whole folder subtree by rewriting logical
// paths (metadata-only; bytes stay on their nodes). Powers cut/paste + rename.
func (s *Server) handleMove(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Src string `json:"src" binding:"required"`
		Dst string `json:"dst" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, dst := cleanLogical(in.Src), cleanLogical(in.Dst)
	if src == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
		return
	}
	if path.Base(src) == manifest.FileName || path.Base(dst) == manifest.FileName {
		c.JSON(http.StatusForbidden, gin.H{"error": "系统文件不可操作"})
		return
	}
	if s.accessDenied(c, src) {
		return
	}
	// Moving into a protected directory (or creating a protected ancestor path)
	// also requires access to the destination.
	if dst != "/" && s.accessDenied(c, dst) {
		return
	}
	if dst == src {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	if strings.HasPrefix(dst+"/", src+"/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能移动到自身或子目录"})
		return
	}
	var n int64
	s.db.Model(&models.File{}).Where("logical_path = ? AND status != ?", dst, "deleted").Count(&n)
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "目标已存在同名项"})
		return
	}
	srcAcc, srcRemote, _, err := s.resolveMount(src)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	dstAcc, dstRemote, _, err := s.resolveMount(dst)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if srcAcc.ID != dstAcc.ID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "暂不支持跨挂载节点移动"})
		return
	}
	srcRemote = s.resolveRemote(srcAcc, src, srcRemote)
	var srcRow models.File
	hasRow := s.db.Where("baidu_account_id = ? AND logical_path = ? AND status <> ?", srcAcc.ID, src, "deleted").
		Order("id desc").First(&srcRow).Error == nil
	if hasRow && srcRow.RemoteUUID != "" {
		if path.Dir(src) == path.Dir(dst) {
			// pure rename: the uuid remote name doesn't change -> metadata-only
			dstRemote = srcRemote
		} else {
			var dstParent string
			if s.systemConfig().UUIDStorage {
				dstParent, err = s.ensureUUIDDirs(c, dstAcc, path.Dir(dst))
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
			} else {
				dstParent = s.resolveRemote(dstAcc, path.Dir(dst), path.Dir(dstRemote))
			}
			dstRemote = path.Join(dstParent, srcRow.RemoteUUID)
		}
	} else {
		dstRemote = s.resolveRemote(dstAcc, dst, dstRemote)
	}
	if srcRemote != dstRemote {
		token, err := s.pool.Token(c.Request.Context(), srcAcc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(srcAcc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		if err := s.client.Move(c.Request.Context(), token, srcRemote, dstRemote); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
	}
	// Move the item itself (file or folder record).
	s.db.Model(&models.File{}).Where("logical_path = ? AND status != ?", src, "deleted").
		Updates(map[string]any{"logical_path": dst, "remote_path": dstRemote})
	// Move descendants by prefix rewrite (logical always, remote when the
	// subtree physically moved).
	var kids []models.File
	s.db.Where("logical_path LIKE ? AND status != ?", src+"/%", "deleted").Find(&kids)
	for _, k := range kids {
		np := dst + strings.TrimPrefix(k.LogicalPath, src)
		updates := map[string]any{"logical_path": np}
		if srcRemote != dstRemote && strings.HasPrefix(k.RemotePath, srcRemote+"/") {
			updates["remote_path"] = dstRemote + strings.TrimPrefix(k.RemotePath, srcRemote)
		}
		s.db.Model(&models.File{}).Where("id = ?", k.ID).Updates(updates)
	}
	s.moveAccessRules(src, dst)
	s.movePathNotes(src, dst)
	s.msync.Mark(srcAcc.ID)
	s.audit(c, uid, usernameOf(c), "move", src+" -> "+dst)
	c.JSON(http.StatusOK, gin.H{"ok": true, "moved": len(kids) + 1})
}

// handleDeleteFolder removes a folder and everything under it (best-effort remote
// deletes for the real files), marking all affected records deleted.
func (s *Server) handleDeleteFolder(c *gin.Context) {
	uid := uidOf(c)
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := cleanLogical(in.Path)
	if p == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}
	if s.accessDenied(c, p) {
		return
	}
	if acc, remotePath, _, err := s.resolveMount(p); err == nil {
		remotePath = s.resolveRemote(acc, p, remotePath)
		token, err := s.pool.Token(c.Request.Context(), acc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := s.coord.Limiter.Wait(c.Request.Context(), "acct:"+itoa(acc.ID)); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		if err := s.client.Delete(c.Request.Context(), token, []string{remotePath}); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		s.db.Model(&models.File{}).Where("(logical_path = ? OR logical_path LIKE ?) AND status != ?", p, p+"/%", "deleted").
			Update("status", "deleted")
		s.dropAccessRules(p)
		s.dropPathNotes(p, true)
		s.msync.Mark(acc.ID)
		s.audit(c, uid, usernameOf(c), "folder.delete", p)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	var items []models.File
	s.db.Where("(logical_path = ? OR logical_path LIKE ?) AND status != ?", p, p+"/%", "deleted").Find(&items)
	ctx := c.Request.Context()
	touched := map[uint]bool{}
	for _, f := range items {
		if !f.IsDir && f.RemotePath != "" {
			if acc, err := s.pool.Get(ctx, f.BaiduAccountID); err == nil {
				if token, terr := s.pool.Token(ctx, acc); terr == nil {
					_ = s.coord.Limiter.Wait(ctx, "acct:"+itoa(acc.ID))
					_ = s.client.Delete(ctx, token, []string{f.RemotePath})
					s.pool.AddUsed(acc.ID, -f.Size)
				}
			}
		}
		touched[f.BaiduAccountID] = true
		s.db.Model(&models.File{}).Where("id = ?", f.ID).Update("status", "deleted")
	}
	s.dropAccessRules(p)
	s.dropPathNotes(p, true)
	for id := range touched {
		s.msync.Mark(id)
	}
	s.audit(c, uid, usernameOf(c), "folder.delete", p)
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(items)})
}

// parseRangeHeader parses a simple "bytes=start-end" Range header. If the
// header is missing or malformed, the full file range [0, size) is returned.
func parseRangeHeader(h string, size int64, start, end *int64) {
	*start, *end = 0, size
	if !strings.HasPrefix(h, "bytes=") {
		return
	}
	h = strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(h, "-", 2)
	if len(parts) != 2 {
		return
	}
	if s, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64); err == nil {
		*start = s
	}
	if e, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
		*end = e + 1 // Range end is inclusive
	}
	if *end > size {
		*end = size
	}
	if *start < 0 {
		*start = 0
	}
	if *start > *end {
		*start, *end = 0, size
	}
}
