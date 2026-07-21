// Package manifest maintains a per-node metadata manifest (.gsc-manifest.json)
// stored at the node's mount root on the netdisk. It records every
// platform-tracked file/directory (logical path, UUID remote name, encryption
// info) so another deployment can rebuild its metadata table from the netdisk
// alone. The manifest is written back on a debounce after mutations and
// imported manually from the admin UI.
package manifest

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/coord"
	"gosocode/internal/models"
	"gosocode/internal/realtime"
)

// FileName is the well-known manifest name at each node's mount root. The API
// layer hides it from listings and refuses direct operations on it.
const FileName = ".gsc-manifest.json"

type Entry struct {
	UUID       string    `json:"uuid,omitempty"` // remote name when obfuscated; empty for plain entries
	Path       string    `json:"path"`           // logical path relative to the mount root
	RemotePath string    `json:"remote_path"`
	FsID       uint64    `json:"fs_id,omitempty"`
	IsDir      bool      `json:"is_dir,omitempty"`
	Size       int64     `json:"size,omitempty"`
	MD5        string    `json:"md5,omitempty"`
	Encrypted  bool      `json:"encrypted,omitempty"`
	EncKey     string    `json:"enc_key,omitempty"` // wrapped with the master key; importing server needs the same key
	EncIV      string    `json:"enc_iv,omitempty"`
	MTime      time.Time `json:"mtime"`
}

type Doc struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Entries     []Entry   `json:"entries"`
}

// mountRoot mirrors api.nodeMountPath without importing the api package.
func mountRoot(acc *models.BaiduAccount) string {
	p := strings.TrimSpace(strings.ReplaceAll(acc.MountPath, "\\", "/"))
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// RemotePath is where the node's manifest lives on the netdisk.
func RemotePath(acc *models.BaiduAccount) string {
	return path.Join(mountRoot(acc), FileName)
}

// relLogical strips the mount-name segment: "/<mount>/a/b" -> "a/b". Stored
// relative so a differently-named mount on the importing server still works.
func relLogical(logical string) string {
	parts := strings.SplitN(strings.TrimPrefix(logical, "/"), "/", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// Build renders the manifest JSON for one node from the local File table.
// Only "ready" entries are included: an in-flight upload must never be
// importable as a complete file on another deployment. Entries that carry no
// mapping information are left out entirely: system rows (/__system), and
// plain passthrough files (no UUID, not encrypted, remote path identical to
// the logical path) — reconcile re-derives those from the netdisk itself, so
// recording them would only bloat the manifest.
func Build(db *gorm.DB, acc *models.BaiduAccount) ([]byte, error) {
	var rows []models.File
	if err := db.Where("baidu_account_id = ? AND status = ?", acc.ID, "ready").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	mroot := mountRoot(acc)
	doc := Doc{Version: 1, GeneratedAt: time.Now()}
	for _, f := range rows {
		if strings.HasPrefix(f.LogicalPath, "/__system/") {
			continue
		}
		rel := relLogical(f.LogicalPath)
		if rel == "" {
			continue
		}
		remoteRel := strings.TrimPrefix(strings.TrimPrefix(f.RemotePath, mroot), "/")
		if f.RemoteUUID == "" && !f.Encrypted && remoteRel == rel {
			continue
		}
		doc.Entries = append(doc.Entries, Entry{
			UUID: f.RemoteUUID, Path: rel, RemotePath: f.RemotePath, FsID: f.FsID,
			IsDir: f.IsDir, Size: f.Size, MD5: f.MD5,
			Encrypted: f.Encrypted, EncKey: f.EncKey, EncIV: f.EncIV,
			MTime: f.UpdatedAt,
		})
	}
	sort.Slice(doc.Entries, func(i, j int) bool { return doc.Entries[i].Path < doc.Entries[j].Path })
	return json.MarshalIndent(doc, "", " ")
}

// Syncer debounces manifest write-backs so bulk operations rewrite once.
type Syncer struct {
	db     *gorm.DB
	pool   *account.Pool
	client *baidu.Client
	coord  *coord.Coordinator
	hub    *realtime.Hub

	mu    sync.Mutex
	dirty map[uint]bool
	kick  chan struct{}
	// lastSync is the Build snapshot time of the last successful write-back
	// per node; the reconcile loop compares it against files.updated_at to
	// catch marks lost to a crash inside the debounce window.
	lastSync map[uint]time.Time
}

func NewSyncer(db *gorm.DB, pool *account.Pool, client *baidu.Client, co *coord.Coordinator, hub *realtime.Hub) *Syncer {
	s := &Syncer{db: db, pool: pool, client: client, coord: co, hub: hub,
		dirty: map[uint]bool{}, kick: make(chan struct{}, 1),
		lastSync: map[uint]time.Time{}}
	go s.loop()
	go s.reconcileLoop()
	return s
}

// StartDailyReconcile starts the 00:00 daily netdisk-to-local reconciliation.
// It is a separate opt-in method so tests and deployments that do not want
// automatic scheduling can omit it.
func (s *Syncer) StartDailyReconcile(timezone string) {
	go s.dailyReconcileLoop(true, timezone, newLastRunRecorder(s.db))
}

// reconcileLoop is the safety net: every 10 minutes it marks any node whose
// metadata changed after its last successful manifest write-back, so a crash
// (or a missed Mark call) only delays the cloud manifest, never loses it.
func (s *Syncer) reconcileLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		var ids []uint
		if err := s.db.Model(&models.BaiduAccount{}).Where("status = ?", "active").Pluck("id", &ids).Error; err != nil {
			continue
		}
		for _, id := range ids {
			s.mu.Lock()
			since := s.lastSync[id]
			s.mu.Unlock()
			var n int64
			s.db.Model(&models.File{}).
				Where("baidu_account_id = ? AND updated_at > ?", id, since).
				Limit(1).Count(&n)
			if n > 0 {
				s.Mark(id)
			}
		}
	}
}

// Mark schedules a manifest rewrite for the node (best-effort, debounced).
func (s *Syncer) Mark(accID uint) {
	if accID == 0 {
		return
	}
	s.mu.Lock()
	s.dirty[accID] = true
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Syncer) loop() {
	for range s.kick {
		time.Sleep(5 * time.Second) // debounce window: coalesce bursts of mutations
		s.mu.Lock()
		ids := make([]uint, 0, len(s.dirty))
		for id := range s.dirty {
			ids = append(ids, id)
		}
		s.dirty = map[uint]bool{}
		s.mu.Unlock()
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := s.SyncNow(ctx, id); err != nil {
				log.Printf("[manifest] write-back failed node=%d: %v", id, err)
			}
			cancel()
		}
	}
}

// SyncNow rebuilds and uploads the node's manifest immediately.
func (s *Syncer) SyncNow(ctx context.Context, accID uint) error {
	acc, err := s.pool.Get(ctx, accID)
	if err != nil {
		return err
	}
	snapshotAt := time.Now()
	data, err := Build(s.db, acc)
	if err != nil {
		return err
	}
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		return err
	}
	unlock, err := s.coord.Locker.Lock(ctx, "manifest:"+strconv.Itoa(int(accID)), 2*time.Minute)
	if err != nil {
		return err
	}
	defer unlock()

	remote := RemotePath(acc)
	sum := md5.Sum(data)
	blocks := []string{hex.EncodeToString(sum[:])}
	acctKey := "acct:" + strconv.Itoa(int(accID))

	if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return err
	}
	pre, err := s.client.Precreate(ctx, token, remote, int64(len(data)), blocks)
	if err != nil {
		return fmt.Errorf("precreate: %w", err)
	}
	if pre.ReturnType == 2 { // rapid-upload: identical content already merged
		s.markSynced(accID, snapshotAt)
		return nil
	}
	if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return err
	}
	if err := s.client.UploadChunk(ctx, token, remote, pre.UploadID, 0, data); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return err
	}
	if _, err := s.client.Create(ctx, token, remote, int64(len(data)), pre.UploadID, blocks); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	s.markSynced(accID, snapshotAt)
	log.Printf("[manifest] wrote %s node=%d bytes=%d", remote, accID, len(data))
	return nil
}

func (s *Syncer) markSynced(accID uint, at time.Time) {
	s.mu.Lock()
	s.lastSync[accID] = at
	s.mu.Unlock()
}

// Fetch downloads and parses the node's manifest from the netdisk.
func (s *Syncer) Fetch(ctx context.Context, acc *models.BaiduAccount) (*Doc, error) {
	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		return nil, err
	}
	acctKey := "acct:" + strconv.Itoa(int(acc.ID))
	if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return nil, err
	}
	items, err := s.client.ListDir(ctx, token, mountRoot(acc))
	if err != nil {
		return nil, fmt.Errorf("读取网盘目录失败: %w", err)
	}
	var fsID uint64
	for _, it := range items {
		if it.ServerFilename == FileName {
			fsID = it.FsID
			break
		}
	}
	if fsID == 0 {
		return nil, fmt.Errorf("网盘上未找到元数据清单 %s", FileName)
	}
	if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return nil, err
	}
	metas, err := s.client.FileMetas(ctx, token, []uint64{fsID})
	if err != nil || len(metas) == 0 || metas[0].Dlink == "" {
		return nil, fmt.Errorf("无法获取清单下载地址: %v", err)
	}
	if err := s.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return nil, err
	}
	resp, err := s.client.OpenDownload(ctx, token, metas[0].Dlink, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("清单下载失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var doc Doc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("清单格式无效: %w", err)
	}
	return &doc, nil
}

type ImportStats struct {
	Imported int `json:"imported"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
}

// Import upserts manifest entries into the File table, keyed by
// (baidu_account_id, remote_path). mountLabel is the node's current
// user-facing mount name; entry paths are relative to it. Locally deleted
// rows are respected (skipped), so an import never resurrects deletions.
func Import(db *gorm.DB, acc *models.BaiduAccount, mountLabel string, doc *Doc) (ImportStats, error) {
	var st ImportStats
	for _, e := range doc.Entries {
		if e.RemotePath == "" || e.Path == "" || strings.Contains(e.Path, "..") {
			st.Skipped++
			continue
		}
		logical := path.Clean("/" + mountLabel + "/" + e.Path)
		var f models.File
		err := db.Where("baidu_account_id = ? AND remote_path = ?", acc.ID, e.RemotePath).First(&f).Error
		switch {
		case err == gorm.ErrRecordNotFound:
			now := time.Now()
			f = models.File{
				BaiduAccountID: acc.ID, LogicalPath: logical, RemotePath: e.RemotePath,
				RemoteUUID: e.UUID, FsID: e.FsID, Size: e.Size, MD5: e.MD5,
				Encrypted: e.Encrypted, EncKey: e.EncKey, EncIV: e.EncIV,
				IsDir: e.IsDir, Status: "ready", CreatedAt: now, UpdatedAt: now,
			}
			if err := db.Create(&f).Error; err != nil {
				return st, err
			}
			st.Imported++
		case err != nil:
			return st, err
		case f.Status == "deleted":
			st.Skipped++
		default:
			updates := map[string]any{}
			if f.LogicalPath != logical {
				updates["logical_path"] = logical
			}
			if f.RemoteUUID != e.UUID && e.UUID != "" {
				updates["remote_uuid"] = e.UUID
			}
			if f.FsID == 0 && e.FsID != 0 {
				updates["fs_id"] = e.FsID
			}
			if f.MD5 == "" && e.MD5 != "" {
				updates["md5"] = e.MD5
			}
			if !f.Encrypted && e.Encrypted {
				updates["encrypted"] = true
				updates["enc_key"] = e.EncKey
				updates["enc_iv"] = e.EncIV
			}
			if len(updates) == 0 {
				st.Skipped++
				continue
			}
			updates["updated_at"] = time.Now()
			if err := db.Model(&f).Updates(updates).Error; err != nil {
				return st, err
			}
			st.Updated++
		}
	}
	return st, nil
}
