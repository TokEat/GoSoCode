package manifest

import (
	"context"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"gosocode/internal/baidu"
	"gosocode/internal/models"
)

// DailyReconcileStats reports the result of one daily cloud-to-local
// reconciliation run for a single storage node.
type DailyReconcileStats struct {
	Imported int           `json:"imported"`
	Updated  int           `json:"updated"`
	Deleted  int           `json:"deleted"`
	Warnings int           `json:"warnings"`
	Duration time.Duration `json:"duration_ms"`
}

func (s DailyReconcileStats) String() string {
	return fmt.Sprintf("imported=%d updated=%d deleted=%d warnings=%d duration=%s",
		s.Imported, s.Updated, s.Deleted, s.Warnings, s.Duration)
}

// dailyReconcileLoop triggers a full netdisk-to-local reconciliation at 00:00
// every day (server-local time or configured timezone). It also catches up on
// startup if the server missed the scheduled run.
func (s *Syncer) dailyReconcileLoop(enabled bool, timezone string, lastRun *lastRunRecorder) {
	if !enabled {
		return
	}
	loc := time.Local
	if timezone != "" {
		if l, err := time.LoadLocation(timezone); err == nil {
			loc = l
		}
	}
	// Catch-up on startup: if we never ran or the last run was >24h ago, run now.
	if lastRun == nil || time.Since(lastRun.at()) > 24*time.Hour {
		s.runDailyReconcileAll(context.Background())
		if lastRun != nil {
			lastRun.set(time.Now())
		}
	}

	for {
		now := time.Now().In(loc)
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Add(24 * time.Hour)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
		s.runDailyReconcileAll(context.Background())
		if lastRun != nil {
			lastRun.set(time.Now())
		}
		// Avoid a tight loop if the sleep somehow returned immediately.
		time.Sleep(1 * time.Second)
	}
}

func (s *Syncer) runDailyReconcileAll(ctx context.Context) {
	log.Printf("[manifest] daily reconcile started")
	var ids []uint
	if err := s.db.Model(&models.BaiduAccount{}).Where("status = ?", "active").Pluck("id", &ids).Error; err != nil {
		log.Printf("[manifest] daily reconcile failed to list accounts: %v", err)
		return
	}
	for _, id := range ids {
		st, err := s.DailyReconcile(ctx, id)
		if err != nil {
			log.Printf("[manifest] daily reconcile failed node=%d: %v", id, err)
			continue
		}
		log.Printf("[manifest] daily reconcile finished node=%d %s", id, st.String())
	}
}

// DailyReconcile fetches the on-netdisk manifest, recursively lists the actual
// cloud filesystem for this node, and updates the local File table so that the
// netdisk is authoritative. Items present remotely but unknown locally are
// imported (preferring the manifest for logical path, uuid and encryption
// metadata). Items present locally but missing remotely are soft-deleted.
// Remote items unknown to both the DB and the manifest are imported as plain
// entries under their remote filename so they stay visible and downloadable.
func (s *Syncer) DailyReconcile(ctx context.Context, accID uint) (DailyReconcileStats, error) {
	start := time.Now()
	var st DailyReconcileStats
	acc, err := s.pool.Get(ctx, accID)
	if err != nil {
		return st, err
	}

	s.broadcast(map[string]any{
		"type": "manifest.daily_sync", "phase": "started",
		"account_id": accID, "started_at": start,
	})

	token, err := s.pool.Token(ctx, acc)
	if err != nil {
		s.broadcast(map[string]any{
			"type": "manifest.daily_sync", "phase": "finished",
			"account_id": accID, "error": err.Error(),
		})
		return st, err
	}

	// 1. Recursively list the actual cloud tree.
	acctKey := "acct:" + strconv.Itoa(int(accID))
	items, err := s.client.ListDirRecursive(ctx, s.coord.Limiter, acctKey, token, mountRoot(acc), 20)
	if err != nil {
		s.broadcast(map[string]any{
			"type": "manifest.daily_sync", "phase": "finished",
			"account_id": accID, "error": err.Error(),
		})
		return st, fmt.Errorf("递归列目录失败: %w", err)
	}

	// 2. Try to load the manifest for logical-path/uuid/encryption metadata.
	doc, _ := s.Fetch(ctx, acc)
	entryByRemote := map[string]Entry{}
	if doc != nil {
		for _, e := range doc.Entries {
			entryByRemote[e.RemotePath] = e
		}
	}

	// 3. Import / update every remote item. Netdisk wins.
	seenRemote := map[string]bool{}
	for _, it := range items {
		seenRemote[it.Path] = true
		imp := s.reconcileItem(acc, it, entryByRemote)
		st.Imported += imp.Imported
		st.Updated += imp.Updated
		st.Warnings += imp.Warnings
	}

	// 4. Soft-delete local rows whose remote object is gone. Rows touched
	// after the reconcile started are skipped: an upload that finished while
	// we were listing the cloud tree is not in the snapshot but is not an
	// orphan either.
	var orphans []models.File
	s.db.Where("baidu_account_id = ? AND status = ? AND updated_at < ?", accID, "ready", start).Find(&orphans)
	now := time.Now()
	for _, f := range orphans {
		if !seenRemote[f.RemotePath] {
			if err := s.db.Model(&f).Updates(map[string]any{
				"status":      "deleted",
				"deleted_by":  "daily_reconcile",
				"deleted_at":  now,
				"updated_at":  now,
			}).Error; err != nil {
				return st, err
			}
			st.Deleted++
		}
	}

	// 5. Write the now-authoritative manifest back to the netdisk.
	if err := s.SyncNow(ctx, accID); err != nil {
		log.Printf("[manifest] daily reconcile manifest write-back failed node=%d: %v", accID, err)
		// not fatal; local DB is already correct, and the periodic reconcile
		// loop will retry the write-back.
	}

	st.Duration = time.Since(start)
	s.broadcast(map[string]any{
		"type": "manifest.daily_sync", "phase": "finished",
		"account_id": accID, "imported": st.Imported, "updated": st.Updated,
		"deleted": st.Deleted, "warnings": st.Warnings,
		"duration_ms": st.Duration.Milliseconds(),
	})
	s.audit(accID, "daily_reconcile", st.String())
	return st, nil
}

// reconcileItem imports or updates one remote item. It returns the import
// stats and a warning count (always 0: unknown items are imported as plain
// unencrypted files using their remote filename).
func (s *Syncer) reconcileItem(acc *models.BaiduAccount, it baidu.ListItem, entryByRemote map[string]Entry) DailyReconcileStats {
	st := DailyReconcileStats{}
	accID := acc.ID
	mountLabel := mountNameFromAccount(acc)

	var local models.File
	err := s.db.Where("baidu_account_id = ? AND remote_path = ? AND status <> ?",
		accID, it.Path, "deleted").Order("id desc").First(&local).Error

	// Known locally: just refresh fs_id/size if changed.
	if err == nil {
		updates := map[string]any{}
		if local.FsID != it.FsID && it.FsID != 0 {
			updates["fs_id"] = it.FsID
		}
		if local.Size != it.Size {
			updates["size"] = it.Size
		}
		if len(updates) > 0 {
			updates["updated_at"] = time.Now()
			if err := s.db.Model(&local).Updates(updates).Error; err == nil {
				st.Updated++
			}
		}
		return st
	}

	// Not locally known: try to import from the manifest.
	if e, ok := entryByRemote[it.Path]; ok {
		logical := path.Clean("/" + mountLabel + "/" + e.Path)
		now := time.Now()
		f := models.File{
			BaiduAccountID: accID, LogicalPath: logical, RemotePath: e.RemotePath,
			RemoteUUID: e.UUID, FsID: e.FsID, Size: e.Size, MD5: e.MD5,
			Encrypted: e.Encrypted, EncKey: e.EncKey, EncIV: e.EncIV,
			IsDir: e.IsDir, Status: "ready", CreatedAt: now, UpdatedAt: now,
		}
		if it.FsID != 0 && f.FsID == 0 {
			f.FsID = it.FsID
		}
		if it.Size != 0 && f.Size == 0 {
			f.Size = it.Size
		}
		if err := s.db.Create(&f).Error; err == nil {
			st.Imported++
		}
		return st
	}

	// Unknown remote item and not in manifest: treat it as a plain (unencrypted)
	// file/directory uploaded outside this deployment. Use the remote filename as
	// the logical name so the entry is visible and downloadable.
	logical := path.Clean("/" + mountLabel + "/" + strings.TrimPrefix(it.Path, mountRoot(acc)))
	if logical == "/"+mountLabel {
		logical = path.Clean("/" + mountLabel + "/" + it.ServerFilename)
	}
	now := time.Now()
	f := models.File{
		BaiduAccountID: accID, LogicalPath: logical, RemotePath: it.Path,
		FsID: it.FsID, Size: it.Size, IsDir: it.IsDir == 1,
		Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.Create(&f).Error; err == nil {
		st.Imported++
	}
	return st
}

// mountNameFromAccount mirrors api.mountLabel without importing api.
func mountNameFromAccount(acc *models.BaiduAccount) string {
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

func (s *Syncer) broadcast(payload any) {
	if s.hub != nil {
		s.hub.BroadcastAdmins(payload)
	}
}

func (s *Syncer) audit(accountID uint, action, detail string) {
	if s.db == nil {
		return
	}
	_ = s.db.Create(&models.AuditLog{
		At:       time.Now(),
		UserID:   0,
		Username: "system",
		Action:   "manifest.daily_sync." + action,
		Detail:   fmt.Sprintf("node=%d %s", accountID, detail),
		IP:       "",
	})
}

// lastRunRecorder persists the last daily-reconcile timestamp so a restarted
// process can decide whether to catch up immediately.
type lastRunRecorder struct {
	db *gorm.DB
}

func newLastRunRecorder(db *gorm.DB) *lastRunRecorder {
	return &lastRunRecorder{db: db}
}

func (r *lastRunRecorder) at() time.Time {
	if r == nil || r.db == nil {
		return time.Time{}
	}
	var f models.File
	err := r.db.Where("status = ? AND deleted_by = ?", "deleted", "daily_reconcile_marker").
		Order("deleted_at desc").First(&f).Error
	if err != nil {
		return time.Time{}
	}
	if f.DeletedAt != nil {
		return *f.DeletedAt
	}
	return f.UpdatedAt
}

func (r *lastRunRecorder) set(t time.Time) {
	if r == nil || r.db == nil {
		return
	}
	now := time.Now()
	var f models.File
	if err := r.db.Where("status = ? AND deleted_by = ?", "deleted", "daily_reconcile_marker").
		First(&f).Error; err == nil {
		r.db.Model(&f).Updates(map[string]any{
			"deleted_at": t, "updated_at": now,
		})
		return
	}
	r.db.Create(&models.File{
		BaiduAccountID: 0,
		LogicalPath:    "/__system/daily_reconcile_marker",
		RemotePath:     "/__system/daily_reconcile_marker",
		Status:         "deleted",
		DeletedBy:      "daily_reconcile_marker",
		DeletedAt:      &t,
		CreatedAt:      now, UpdatedAt: now,
	})
}
