package job

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"gosocode/internal/baidu"
	"gosocode/internal/models"
)

// ShareImportSpec describes one 转存 job: copy files from an external Baidu
// share into the node that owns the target logical directory. The caller has
// already taken a load slot on AccountID (released by the job goroutine).
type ShareImportSpec struct {
	UserID    uint
	AccountID uint
	SURL      string
	SeKey     string
	ShareID   uint64
	UK        uint64
	FsIDs     []uint64
	Target       string // cleaned logical destination directory (inside the mount)
	TargetRemote string // real remote dir of Target (UUID mappings resolved by the API layer)
	MountRoot    string // node's remote mount root
	Encrypt      bool
	RandName     bool // rename the *remote* files to bare UUIDs (like UUID storage); logical names stay
}

// impFile pairs a transferred remote item with its logical destination. The
// logical path is computed from the pre-rename remote path, so RandName can
// rewrite item.Path without losing the user-visible name.
type impFile struct {
	item    baidu.ListItem
	logical string
	uuid    string // set when RandName renamed the remote file
}

func (m *Manager) SubmitShareImport(spec ShareImportSpec) (string, error) {
	jobID := uuid.NewString()
	job := models.Job{
		ID: jobID, UserID: spec.UserID, Type: "share_import",
		BaiduAccountID: spec.AccountID, Status: "pending",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := m.db.Create(&job).Error; err != nil {
		m.pool.Release(spec.AccountID)
		return "", err
	}
	m.logTransferEvent(&models.TransferEvent{
		UserID: spec.UserID, Kind: "import", Stage: "init", RefType: "share",
		RefID: jobID, Path: spec.Target, AccountID: spec.AccountID,
	})
	go m.runShareImport(jobID, spec)
	return jobID, nil
}

func (m *Manager) runShareImport(jobID string, spec ShareImportSpec) {
	ctx := context.Background()
	defer m.pool.Release(spec.AccountID)

	fail := func(err error) {
		log.Printf("[job] share import FAILED job=%s user=%d target=%s: %v", jobID, spec.UserID, spec.Target, err)
		m.logTransferEvent(
			&models.TransferEvent{
				UserID: spec.UserID, Kind: "import", Stage: "failure", RefType: "share",
				RefID: jobID, Path: spec.Target, AccountID: spec.AccountID,
				Error: baidu.ShareErrMsg(err),
			})
		m.jobStatus(jobID, spec.UserID, spec.Target, "failed", 0, baidu.ShareErrMsg(err))
	}
	progress := func(p int) {
		m.jobStatus(jobID, spec.UserID, spec.Target, "running", p, "")
	}
	progress(5)

	acc, err := m.pool.Get(ctx, spec.AccountID)
	if err != nil {
		fail(err)
		return
	}
	token, err := m.pool.Token(ctx, acc)
	if err != nil {
		fail(err)
		return
	}
	acctKey := "acct:" + itoa(acc.ID)

	stagingRoot := path.Join(spec.MountRoot, models.ImportsDirName)
	staging := path.Join(stagingRoot, jobID)

	// Hidden metadata rows: the daily reconcile imports any remote path it does
	// not know as a visible entry, so the staging tree must be pre-registered
	// under an internal logical namespace users never list.
	m.ensureSystemDirRow(acc.ID, stagingRoot, "/__system/imports/"+itoa(acc.ID))
	m.ensureSystemDirRow(acc.ID, staging, "/__system/imports/"+itoa(acc.ID)+"/"+jobID)

	if err := m.coord.Limiter.Wait(ctx, acctKey); err != nil {
		fail(err)
		return
	}
	if err := m.client.MakeDir(ctx, token, staging); err != nil {
		fail(err)
		return
	}

	unlock, err := m.coord.Locker.Lock(ctx, "path:"+staging, 10*time.Minute)
	if err != nil {
		fail(err)
		return
	}
	defer unlock()

	if err := m.client.ShareTransfer(ctx, m.coord.Limiter, acctKey, token, spec.ShareID, spec.UK, spec.SeKey, spec.FsIDs, staging); err != nil {
		m.cleanupStaging(ctx, token, acctKey, staging)
		fail(err)
		return
	}
	progress(30)

	items, err := m.client.ListDirRecursive(ctx, m.coord.Limiter, acctKey, token, staging, 20)
	if err != nil {
		fail(err)
		return
	}
	if len(items) == 0 {
		m.cleanupStaging(ctx, token, acctKey, staging)
		fail(fmt.Errorf("转存完成但未找到任何文件"))
		return
	}
	progress(40)

	now := time.Now()
	var total int64
	var files, dirs []impFile
	for _, it := range items {
		logical := importLogical(spec.Target, staging, it.Path)
		if it.IsDir == 1 {
			dirs = append(dirs, impFile{item: it, logical: logical})
			continue
		}
		total += it.Size
		files = append(files, impFile{item: it, logical: logical})
	}

	// RandName: rename each remote file in place to a bare UUID (same shape as
	// UUID storage) before rows are registered. The logical name is untouched
	// and RemoteUUID lands in the cloud manifest via msync below. A failed
	// rename degrades to the original remote name instead of failing the job.
	if spec.RandName {
		for i := range files {
			id := uuid.NewString()
			newPath := path.Join(path.Dir(files[i].item.Path), id)
			if err := withRetry(3, "rename "+files[i].item.Path, func() error {
				if e := m.coord.Limiter.Wait(ctx, acctKey); e != nil {
					return e
				}
				return m.client.Move(ctx, token, files[i].item.Path, newPath)
			}); err != nil {
				log.Printf("[job] share import rename to uuid failed job=%s path=%s: %v", jobID, files[i].item.Path, err)
				continue
			}
			files[i].item.Path = newPath
			files[i].uuid = id
		}
		progress(45)
	}

	// Relocate everything out of the temporary staging dir into the target
	// directory's real remote dir, then drop the staging dir — no data may
	// linger under .gsc-imports (it is plumbing, not storage).
	dest := spec.TargetRemote
	if dest == "" {
		dest = spec.MountRoot
	}
	allMoved := m.moveOutOfStaging(ctx, jobID, token, acctKey, staging, dest, spec.Target, files, dirs)
	if allMoved {
		m.cleanupStaging(ctx, token, acctKey, staging)
		m.db.Model(&models.File{}).
			Where("baidu_account_id = ? AND remote_path = ? AND logical_path LIKE ?", acc.ID, staging, "/__system/%").
			Update("status", "deleted")
		// Opportunistic sweep: remove stale staging dirs from older/failed jobs
		// that no live file row references anymore.
		m.sweepImportStaging(ctx, token, acctKey, acc.ID, stagingRoot, staging)
	}
	progress(50)

	for _, d := range dirs {
		m.db.Create(&models.File{
			OwnerUserID: spec.UserID, BaiduAccountID: acc.ID,
			LogicalPath: d.logical, RemotePath: d.item.Path,
			IsDir: true, Status: "ready", CreatedAt: now, UpdatedAt: now,
		})
	}

	if spec.Encrypt {
		if err := m.encryptImportedFiles(ctx, spec, acc.ID, acc.VipType, token, acctKey, files, total, progress); err != nil {
			fail(err)
			return
		}
	} else {
		for _, f := range files {
			m.db.Create(&models.File{
				OwnerUserID: spec.UserID, BaiduAccountID: acc.ID,
				LogicalPath: f.logical, RemotePath: f.item.Path, RemoteUUID: f.uuid,
				FsID: f.item.FsID, Size: f.item.Size, Status: "ready",
				CreatedAt: now, UpdatedAt: now,
			})
		}
	}

	m.pool.AddUsed(acc.ID, total)
	name := fmt.Sprintf("分享转存（%d 个文件）", len(files))
	if len(files) == 1 {
		name = path.Base(files[0].logical)
	}
	m.db.Create(&models.TransferLog{
		UserID: spec.UserID, Kind: "import", Name: name,
		Path: spec.Target, Size: total, At: time.Now(),
	})
	m.msync.Mark(acc.ID)
	m.logTransferEvent(
		&models.TransferEvent{
			UserID: spec.UserID, Kind: "import", Stage: "finish", RefType: "share",
			RefID: jobID, Path: spec.Target, AccountID: spec.AccountID,
			Bytes: total,
		})
	m.jobStatus(jobID, spec.UserID, spec.Target, "done", 100, "")
	log.Printf("[job] share import done job=%s user=%d target=%s files=%d bytes=%d encrypt=%v",
		jobID, spec.UserID, spec.Target, len(files), total, spec.Encrypt)
}

// encryptImportedFiles runs the download -> AES-256-CTR -> re-upload pipeline
// for every transferred file, overwriting each file in place (rtype=3) so the
// staging tree keeps its structure while the stored bytes become ciphertext.
func (m *Manager) encryptImportedFiles(ctx context.Context, spec ShareImportSpec, accID uint, vipType int, token, acctKey string, files []impFile, total int64, progress func(int)) error {
	var sc models.SystemConfig
	m.db.First(&sc, 1)
	sliceSize := baidu.SliceSizeForVip(vipType)

	var done int64
	now := time.Now()
	for _, fi := range files {
		it := fi.item
		f := models.File{
			OwnerUserID: spec.UserID, BaiduAccountID: accID,
			LogicalPath: fi.logical, RemotePath: it.Path, RemoteUUID: fi.uuid,
			FsID: it.FsID, Size: it.Size, Status: "uploading",
			CreatedAt: now, UpdatedAt: now,
		}
		if err := m.db.Create(&f).Error; err != nil {
			return err
		}
		// Empty files carry no data worth encrypting.
		if it.Size == 0 {
			m.db.Model(&f).Update("status", "ready")
			continue
		}
		if err := m.encryptOneImport(ctx, token, acctKey, &f, sc.MasterKey, accID, sliceSize); err != nil {
			m.db.Model(&f).Update("status", "failed")
			return fmt.Errorf("加密 %s 失败: %w", path.Base(f.LogicalPath), err)
		}
		done += it.Size
		p := 50
		if total > 0 {
			p += int(done * 48 / total)
		}
		progress(p)
	}

	// Traffic accounting: every byte crossed the server twice (down + up).
	m.db.Model(&models.User{}).Where("id = ?", spec.UserID).
		UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", total))
	m.db.Model(&models.User{}).Where("id = ?", spec.UserID).
		UpdateColumn("bytes_up", gorm.Expr("bytes_up + ?", total))
	m.db.Model(&models.BaiduAccount{}).Where("id = ?", accID).
		UpdateColumn("bytes_down", gorm.Expr("bytes_down + ?", total))
	m.db.Model(&models.BaiduAccount{}).Where("id = ?", accID).
		UpdateColumn("bytes_up", gorm.Expr("bytes_up + ?", total))
	return nil
}

func (m *Manager) encryptOneImport(ctx context.Context, token, acctKey string, f *models.File, masterKey string, accID uint, sliceSize int64) error {
	// 1. Resolve the freshly transferred file's dlink and pull it down.
	if err := m.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return err
	}
	metas, err := m.client.FileMetas(ctx, token, []uint64{f.FsID})
	if err != nil {
		return err
	}
	if len(metas) == 0 || metas[0].Dlink == "" {
		return fmt.Errorf("无法获取下载直链")
	}
	if err := m.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return err
	}
	resp, err := m.client.OpenDownload(ctx, token, metas[0].Dlink, "")
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return fmt.Errorf("下载暂存文件失败 HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "gsc-imp-*")
	if err != nil {
		resp.Body.Close()
		return err
	}
	err = m.copyMetered(tmp, resp.Body)
	resp.Body.Close()
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	defer os.Remove(tmp.Name())

	// 2. Encrypt (records wrapped key + IV on the File row).
	encPath, err := m.encryptFileToTemp(tmp.Name(), masterKey, accID, f.ID)
	if err != nil {
		return err
	}
	defer os.Remove(encPath)

	// 3. Re-upload the ciphertext over the same remote path (rtype=3 overwrite).
	blocks, size, err := baidu.SliceFileMD5s(encPath, sliceSize)
	if err != nil {
		return err
	}
	unlock, err := m.coord.Locker.Lock(ctx, "path:"+f.RemotePath, 5*time.Minute)
	if err != nil {
		return err
	}
	defer unlock()

	var uploadID string
	if err := withRetry(3, "precreate "+f.RemotePath, func() error {
		if e := m.coord.Limiter.Wait(ctx, acctKey); e != nil {
			return e
		}
		pre, e := m.client.Precreate(ctx, token, f.RemotePath, size, blocks)
		if e != nil {
			return e
		}
		uploadID = pre.UploadID
		return nil
	}); err != nil {
		return err
	}

	t := uploadTask{jobID: "", userID: 0, tmpPath: encPath, remotePath: f.RemotePath, logical: f.LogicalPath, size: size}
	if err := m.streamImportChunks(ctx, t, token, uploadID, acctKey, sliceSize); err != nil {
		return err
	}

	var fsID uint64
	if err := withRetry(3, "create "+f.RemotePath, func() error {
		if e := m.coord.Limiter.Wait(ctx, acctKey); e != nil {
			return e
		}
		res, e := m.client.Create(ctx, token, f.RemotePath, size, uploadID, blocks)
		if e != nil {
			return e
		}
		fsID = res.FsID
		return nil
	}); err != nil {
		return err
	}
	return m.db.Model(f).Updates(map[string]any{
		"fs_id": fsID, "size": size, "status": "ready", "updated_at": time.Now(),
	}).Error
}

// streamImportChunks is streamChunks without per-chunk job status updates (the
// import job reports aggregate progress per file instead).
func (m *Manager) streamImportChunks(ctx context.Context, t uploadTask, token, uploadID, acctKey string, sliceSize int64) error {
	f, err := os.Open(t.tmpPath)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, sliceSize)
	seq := 0
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			chunk := buf[:n]
			partseq := seq
			if err := withRetry(4, fmt.Sprintf("chunk seq=%d path=%s", partseq, t.remotePath), func() error {
				if e := m.coord.Limiter.Wait(ctx, acctKey); e != nil {
					return e
				}
				return m.client.UploadChunk(ctx, token, t.remotePath, uploadID, partseq, chunk)
			}); err != nil {
				return err
			}
			m.meter.AddUp(int64(n))
			seq++
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

func (m *Manager) copyMetered(dst io.Writer, src io.Reader) error {
	buf := make([]byte, 256*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			m.meter.AddDown(int64(n))
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// ensureSystemDirRow registers a staging directory under the internal
// /__system namespace so the daily reconcile treats it as known instead of
// surfacing it to users.
func (m *Manager) ensureSystemDirRow(accID uint, remotePath, logical string) {
	var f models.File
	if err := m.db.Where("baidu_account_id = ? AND remote_path = ? AND status <> ?",
		accID, remotePath, "deleted").First(&f).Error; err == nil {
		return
	}
	now := time.Now()
	m.db.Create(&models.File{
		BaiduAccountID: accID, LogicalPath: logical, RemotePath: remotePath,
		IsDir: true, Status: "ready", CreatedAt: now, UpdatedAt: now,
	})
}

// moveOutOfStaging relocates every top-level staged entry into destDir and
// rewrites the recorded remote (and, for renamed plain entries, logical)
// paths in files/dirs. A destination name collision retries once with a
// job-suffixed name. A persistently failing entry keeps its staging location
// — data is never lost — and blocks staging deletion (returns false).
func (m *Manager) moveOutOfStaging(ctx context.Context, jobID, token, acctKey, staging, destDir, target string, files, dirs []impFile) bool {
	// The destination dir may only exist as platform metadata (folders are
	// metadata-only): make sure it exists remotely. Failure is tolerated —
	// the moves below will fail and we fall back to staging paths.
	if destDir != "/" {
		if err := m.coord.Limiter.Wait(ctx, acctKey); err == nil {
			if err := m.client.MakeDir(ctx, token, destDir); err != nil {
				log.Printf("[job] share import ensure dest dir %s: %v", destDir, err)
			}
		}
	}

	// Collect the unique top-level entry names under staging, in stable order.
	tops := map[string]bool{}
	var order []string
	for _, list := range [][]impFile{dirs, files} {
		for _, f := range list {
			rel := strings.TrimPrefix(f.item.Path, staging+"/")
			top := rel
			if i := strings.Index(rel, "/"); i >= 0 {
				top = rel[:i]
			}
			if !tops[top] {
				tops[top] = true
				order = append(order, top)
			}
		}
	}

	type mapping struct{ oldR, newR, oldName, newName string }
	var maps []mapping
	allMoved := true
	suffix := "-" + jobID[:8]
	for _, top := range order {
		src := staging + "/" + top
		try := func(name string) error {
			if e := m.coord.Limiter.Wait(ctx, acctKey); e != nil {
				return e
			}
			return m.client.Move(ctx, token, src, path.Join(destDir, name))
		}
		name := top
		if err := try(name); err != nil {
			// most likely a same-name entry already in the destination
			alt := suffixName(top, suffix)
			if err2 := try(alt); err2 != nil {
				log.Printf("[job] share import move out of staging failed job=%s src=%s: %v / %v", jobID, src, err, err2)
				allMoved = false
				continue
			}
			name = alt
		}
		maps = append(maps, mapping{oldR: src, newR: path.Join(destDir, name), oldName: top, newName: name})
	}

	rewrite := func(list []impFile) {
		for i := range list {
			for _, mp := range maps {
				if list[i].item.Path != mp.oldR && !strings.HasPrefix(list[i].item.Path, mp.oldR+"/") {
					continue
				}
				list[i].item.Path = mp.newR + strings.TrimPrefix(list[i].item.Path, mp.oldR)
				// Plain entries share their logical top segment with the remote
				// name; keep them in sync when a collision forced a rename.
				// (RandName files have UUID remote names that never match, so
				// their logical names stay untouched.)
				if mp.newName != mp.oldName {
					lp := path.Join(target, mp.oldName)
					if list[i].logical == lp || strings.HasPrefix(list[i].logical, lp+"/") {
						list[i].logical = path.Join(target, mp.newName) + strings.TrimPrefix(list[i].logical, lp)
					}
				}
				break
			}
		}
	}
	rewrite(dirs)
	rewrite(files)
	return allMoved
}

func suffixName(name, suffix string) string {
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + suffix + ext
}

// sweepImportStaging deletes leftover staging dirs (from failed or pre-move-out
// era jobs) that no live File row references anymore.
func (m *Manager) sweepImportStaging(ctx context.Context, token, acctKey string, accID uint, stagingRoot, current string) {
	if err := m.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return
	}
	items, err := m.client.ListDir(ctx, token, stagingRoot)
	if err != nil {
		return
	}
	for _, it := range items {
		if it.IsDir != 1 || it.Path == current {
			continue
		}
		var live int64
		m.db.Model(&models.File{}).
			Where("baidu_account_id = ? AND status <> ? AND (remote_path = ? OR remote_path LIKE ?)",
				accID, "deleted", it.Path, it.Path+"/%").
			Where("logical_path NOT LIKE ?", "/__system/%").
			Count(&live)
		if live > 0 {
			continue // an older import still stores real data here
		}
		if err := m.coord.Limiter.Wait(ctx, acctKey); err != nil {
			return
		}
		if err := m.client.Delete(ctx, token, []string{it.Path}); err != nil {
			log.Printf("[job] share import sweep failed %s: %v", it.Path, err)
			continue
		}
		m.db.Model(&models.File{}).
			Where("baidu_account_id = ? AND remote_path = ? AND logical_path LIKE ?", accID, it.Path, "/__system/%").
			Update("status", "deleted")
		log.Printf("[job] share import swept stale staging dir %s", it.Path)
	}
}

func (m *Manager) cleanupStaging(ctx context.Context, token, acctKey, staging string) {
	if err := m.coord.Limiter.Wait(ctx, acctKey); err != nil {
		return
	}
	if err := m.client.Delete(ctx, token, []string{staging}); err != nil {
		log.Printf("[job] share import staging cleanup failed %s: %v", staging, err)
	}
}

func importLogical(target, staging, remote string) string {
	return path.Join(target, strings.TrimPrefix(remote, staging))
}

