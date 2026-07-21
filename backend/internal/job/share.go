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
	Target    string // cleaned logical destination directory (inside the mount)
	MountRoot string // node's remote mount root
	Encrypt   bool
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
	go m.runShareImport(jobID, spec)
	return jobID, nil
}

func (m *Manager) runShareImport(jobID string, spec ShareImportSpec) {
	ctx := context.Background()
	defer m.pool.Release(spec.AccountID)

	fail := func(err error) {
		log.Printf("[job] share import FAILED job=%s user=%d target=%s: %v", jobID, spec.UserID, spec.Target, err)
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
	var files []baidu.ListItem
	for _, it := range items {
		logical := importLogical(spec.Target, staging, it.Path)
		if it.IsDir == 1 {
			// Directories stay in staging (their remote path is real); files
			// registered below hang off them via LogicalPath.
			m.db.Create(&models.File{
				OwnerUserID: spec.UserID, BaiduAccountID: acc.ID,
				LogicalPath: logical, RemotePath: it.Path,
				IsDir: true, Status: "ready", CreatedAt: now, UpdatedAt: now,
			})
			continue
		}
		total += it.Size
		files = append(files, it)
	}

	if spec.Encrypt {
		if err := m.encryptImportedFiles(ctx, jobID, spec, acc.ID, acc.VipType, token, acctKey, staging, files, total, progress); err != nil {
			fail(err)
			return
		}
	} else {
		for _, it := range files {
			m.db.Create(&models.File{
				OwnerUserID: spec.UserID, BaiduAccountID: acc.ID,
				LogicalPath: importLogical(spec.Target, staging, it.Path), RemotePath: it.Path,
				FsID: it.FsID, Size: it.Size, Status: "ready",
				CreatedAt: now, UpdatedAt: now,
			})
		}
	}

	m.pool.AddUsed(acc.ID, total)
	name := fmt.Sprintf("分享转存（%d 个文件）", len(files))
	if len(files) == 1 {
		name = path.Base(files[0].Path)
	}
	m.db.Create(&models.TransferLog{
		UserID: spec.UserID, Kind: "import", Name: name,
		Path: spec.Target, Size: total, At: time.Now(),
	})
	m.msync.Mark(acc.ID)
	m.jobStatus(jobID, spec.UserID, spec.Target, "done", 100, "")
	log.Printf("[job] share import done job=%s user=%d target=%s files=%d bytes=%d encrypt=%v",
		jobID, spec.UserID, spec.Target, len(files), total, spec.Encrypt)
}

// encryptImportedFiles runs the download -> AES-256-CTR -> re-upload pipeline
// for every transferred file, overwriting each file in place (rtype=3) so the
// staging tree keeps its structure while the stored bytes become ciphertext.
func (m *Manager) encryptImportedFiles(ctx context.Context, jobID string, spec ShareImportSpec, accID uint, vipType int, token, acctKey, staging string, files []baidu.ListItem, total int64, progress func(int)) error {
	var sc models.SystemConfig
	m.db.First(&sc, 1)
	sliceSize := baidu.SliceSizeForVip(vipType)

	var done int64
	now := time.Now()
	for _, it := range files {
		f := models.File{
			OwnerUserID: spec.UserID, BaiduAccountID: accID,
			LogicalPath: importLogical(spec.Target, staging, it.Path), RemotePath: it.Path,
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
		p := 40
		if total > 0 {
			p += int(done * 58 / total)
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
