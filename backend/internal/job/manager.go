// Package job runs long uploads asynchronously through a bounded worker pool.
// One in-process queue keeps the MVP dependency-free; each account is a storage
// node whose concurrent access is guarded by the coordinator (rate limit + a
// per-path distributed lock so two uploads never race on the same node path).
package job

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/coord"
	"gosocode/internal/crypto"
	"gosocode/internal/keystore"
	"gosocode/internal/manifest"
	"gosocode/internal/metrics"
	"gosocode/internal/models"
	"gosocode/internal/realtime"
)

type uploadTask struct {
	jobID      string
	userID     uint
	tmpPath    string // local temp file holding the uploaded bytes
	remotePath string
	logical    string
	size       int64
}

type Manager struct {
	db      *gorm.DB
	pool    *account.Pool
	client  *baidu.Client
	coord   *coord.Coordinator
	hub     *realtime.Hub
	meter   *metrics.Meter
	msync   *manifest.Syncer
	queue   chan uploadTask
	keysDir string // local backup store for per-node encryption keys
}

func NewManager(db *gorm.DB, pool *account.Pool, client *baidu.Client, co *coord.Coordinator, hub *realtime.Hub, meter *metrics.Meter, msync *manifest.Syncer, workers int, keysDir string) *Manager {
	m := &Manager{
		db: db, pool: pool, client: client, coord: co, hub: hub, meter: meter, msync: msync,
		queue: make(chan uploadTask, 256), keysDir: keysDir,
	}
	if workers <= 0 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		go m.worker()
	}
	go m.uploadJanitor()
	go m.auditJanitor()
	return m
}

// logTransferEvent writes a fine-grained transfer stage event. Best-effort.
func (m *Manager) logTransferEvent(ev *models.TransferEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	m.db.Create(ev)
}

// auditJanitor trims the audit log to the configured retention cap
// (SystemConfig.LogMaxEntries, default 500), oldest entries first.
func (m *Manager) auditJanitor() {
	m.trimAuditLogs()
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		m.trimAuditLogs()
	}
}

func (m *Manager) trimAuditLogs() {
	var sc models.SystemConfig
	m.db.First(&sc, 1)
	max := sc.LogMaxEntries
	if max <= 0 {
		max = 500
	}
	var count int64
	m.db.Model(&models.AuditLog{}).Count(&count)
	if count <= int64(max) {
		return
	}
	var cutoff models.AuditLog
	if err := m.db.Model(&models.AuditLog{}).Order("id desc").Offset(max - 1).First(&cutoff).Error; err != nil {
		return
	}
	res := m.db.Where("id < ?", cutoff.ID).Delete(&models.AuditLog{})
	log.Printf("[janitor] audit log trimmed deleted=%d kept<=%d", res.RowsAffected, max)
}

// uploadJanitor reaps abandoned resumable-upload sessions (browser closed mid
// upload, etc.) so their temp files don't accumulate. Completed/cancelled/failed
// uploads are cleaned up inline; this is the safety net for orphans.
func (m *Manager) uploadJanitor() {
	m.cleanupStaleUploads()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		m.cleanupStaleUploads()
	}
}

func (m *Manager) cleanupStaleUploads() {
	var stale []models.UploadSession
	m.db.Where("updated_at < ?", time.Now().Add(-24*time.Hour)).Find(&stale)
	for _, s := range stale {
		os.Remove(s.TmpPath)
		m.db.Delete(&models.UploadSession{}, "id = ?", s.ID)
	}
	// Streaming sessions hold a load slot on their node and a placeholder File
	// record; both must be released when a session is abandoned. Baidu's
	// upload_id expires after ~24h, so by now the session is unrecoverable.
	var staleStreams []models.StreamSession
	m.db.Where("updated_at < ?", time.Now().Add(-24*time.Hour)).Find(&staleStreams)
	for _, s := range staleStreams {
		if !s.Completed {
			m.pool.Release(s.BaiduAccountID)
			if s.FileID != 0 {
				m.db.Model(&models.File{}).Where("id = ? AND status = ?", s.FileID, "uploading").
					Update("status", "failed")
			}
		}
		m.db.Delete(&models.StreamSession{}, "id = ?", s.ID)
		log.Printf("[stream] janitor reaped stale session=%s path=%s", s.ID, s.LogicalPath)
	}
}

// SubmitUploadTo registers a job + file record on the chosen node and
// enqueues the transfer. remoteUUID is non-empty when the remote name is an
// obfuscation uuid (UUID storage mode).
func (m *Manager) SubmitUploadTo(userID uint, tmpPath, logical, remotePath, remoteUUID string, size int64, acc *models.BaiduAccount) (string, error) {
	jobID := uuid.NewString()
	job := models.Job{
		ID: jobID, UserID: userID, Type: "upload",
		BaiduAccountID: acc.ID, Status: "pending",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := m.db.Create(&job).Error; err != nil {
		m.pool.Release(acc.ID)
		return "", err
	}
	f := models.File{
		OwnerUserID: userID, BaiduAccountID: acc.ID,
		LogicalPath: logical, RemotePath: remotePath, RemoteUUID: remoteUUID,
		Size: size, Status: "uploading",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	m.db.Create(&f)
	m.db.Model(&models.Job{}).Where("id = ?", jobID).Update("file_id", f.ID)

	m.logTransferEvent(&models.TransferEvent{
		UserID: userID, Kind: "upload", Stage: "init", RefType: "job",
		RefID: jobID, Name: path.Base(logical), Path: logical,
		AccountID: acc.ID, Bytes: size,
	})

	m.queue <- uploadTask{
		jobID: jobID, userID: userID, tmpPath: tmpPath,
		remotePath: remotePath, logical: logical, size: size,
	}
	return jobID, nil
}

func (m *Manager) worker() {
	for t := range m.queue {
		m.runUpload(t)
	}
}

func (m *Manager) runUpload(t uploadTask) {
	ctx := context.Background()
	var job models.Job
	m.db.First(&job, "id = ?", t.jobID)
	defer m.pool.Release(job.BaiduAccountID)
	defer os.Remove(t.tmpPath)

	m.logTransferEvent(
		&models.TransferEvent{
			UserID: t.userID, Kind: "upload", Stage: "start", RefType: "job",
			RefID: t.jobID, Name: path.Base(t.logical), Path: t.logical,
			AccountID: job.BaiduAccountID, Bytes: t.size,
		})

	m.setStatus(t, "running", 0, "")

	acc, err := m.pool.Get(ctx, job.BaiduAccountID)
	if err != nil {
		m.fail(t, err)
		return
	}
	token, err := m.pool.Token(ctx, acc)
	if err != nil {
		m.fail(t, err)
		return
	}

	// At-rest encryption: swap the temp file for its AES-256-CTR ciphertext
	// before hashing, so Baidu only ever sees encrypted bytes. CTR preserves
	// the size, so all placement/quota math done so far stays valid.
	var sc models.SystemConfig
	m.db.First(&sc, 1)
	if sc.EncryptStorage && t.size > 0 {
		if err := m.encryptTemp(&t, sc.MasterKey, acc.ID, job.FileID); err != nil {
			m.fail(t, err)
			return
		}
		defer os.Remove(t.tmpPath) // ciphertext temp (the original is already gone)
	}

	// Slice size follows the node's VIP tier (4/16/32MB).
	sliceSize := baidu.SliceSizeForVip(acc.VipType)

	// Streaming pass 1: compute per-block md5 without loading the whole file.
	blocks, size, err := baidu.SliceFileMD5s(t.tmpPath, sliceSize)
	if err != nil {
		m.fail(t, err)
		return
	}

	acctKey := "acct:" + itoa(acc.ID)

	// Serialize operations targeting the same node path across instances.
	unlock, err := m.coord.Locker.Lock(ctx, "path:"+t.remotePath, 5*time.Minute)
	if err != nil {
		m.fail(t, err)
		return
	}
	defer unlock()

	// Step 1: precreate (also detects rapid-upload / 秒传). Retried on transient
	// network errors so a single dropped packet doesn't fail the whole upload.
	var returnType int
	var uploadID string
	err = withRetry(3, "precreate "+t.remotePath, func() error {
		if e := m.coord.Limiter.Wait(ctx, acctKey); e != nil {
			return e
		}
		pre, e := m.client.Precreate(ctx, token, t.remotePath, size, blocks)
		if e != nil {
			return e
		}
		returnType, uploadID = pre.ReturnType, pre.UploadID
		return nil
	})
	if err != nil {
		m.fail(t, err)
		return
	}

	// Step 2: stream missing chunks (skipped entirely on rapid-upload hit).
	if returnType != 2 {
		if err := m.streamChunks(ctx, t, token, uploadID, acctKey, len(blocks), sliceSize, acc.ID); err != nil {
			m.fail(t, err)
			return
		}
	}

	// Step 3: create (merge chunks into the final file).
	var fsID uint64
	err = withRetry(3, "create "+t.remotePath, func() error {
		if e := m.coord.Limiter.Wait(ctx, acctKey); e != nil {
			return e
		}
		res, e := m.client.Create(ctx, token, t.remotePath, size, uploadID, blocks)
		if e != nil {
			return e
		}
		fsID = res.FsID
		return nil
	})
	if err != nil {
		m.fail(t, err)
		return
	}

	m.db.Model(&models.File{}).Where("id = ?", job.FileID).
		Updates(map[string]any{"fs_id": fsID, "size": size, "status": "ready", "updated_at": time.Now()})
	m.pool.AddUsed(acc.ID, size) // node capacity accounting

	// Traffic accounting for the upload (live meter is fed per-chunk in streamChunks).
	m.db.Model(&models.User{}).Where("id = ?", t.userID).UpdateColumn("bytes_up", gorm.Expr("bytes_up + ?", size))
	m.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).UpdateColumn("bytes_up", gorm.Expr("bytes_up + ?", size))

	m.db.Create(&models.TransferLog{
		UserID: t.userID, Kind: "upload", Name: path.Base(t.logical),
		Path: t.logical, Size: size, At: time.Now(),
	})
	m.logTransferEvent(
		&models.TransferEvent{
			UserID: t.userID, Kind: "upload", Stage: "finish", RefType: "job",
			RefID: t.jobID, Name: path.Base(t.logical), Path: t.logical,
			AccountID: acc.ID, Bytes: size,
		})

	m.msync.Mark(acc.ID)
	m.setStatus(t, "done", 100, "")
}

// encryptTemp streams t.tmpPath through AES-256-CTR into a fresh temp file,
// replaces t.tmpPath with it, and records the wrapped key material on the File
// row so downloads can decrypt. The key is the target node's account key
// (one per node, double-stored in DB + KeysDir); the IV is fresh per file.
func (m *Manager) encryptTemp(t *uploadTask, masterKey string, accID, fileID uint) error {
	encPath, err := m.encryptFileToTemp(t.tmpPath, masterKey, accID, fileID)
	if err != nil {
		return err
	}
	os.Remove(t.tmpPath)
	t.tmpPath = encPath
	log.Printf("[job] upload encrypted at rest job=%s path=%s", t.jobID, t.logical)
	return nil
}

// encryptFileToTemp streams srcPath through AES-256-CTR into a fresh temp file
// and records the wrapped key material on the File row so downloads can
// decrypt. The key is the node's account key; the IV is fresh per file. The
// caller owns both the source file and the returned ciphertext file.
func (m *Manager) encryptFileToTemp(srcPath, masterKey string, accID, fileID uint) (string, error) {
	key, err := keystore.NodeKey(m.db, masterKey, m.keysDir, accID)
	if err != nil {
		return "", err
	}
	iv, err := crypto.NewIV()
	if err != nil {
		return "", err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	enc, err := crypto.NewCTRReader(src, key, iv, 0)
	if err != nil {
		return "", err
	}
	dst, err := os.CreateTemp("", "gsc-enc-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, enc); err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(dst.Name())
		return "", err
	}

	wk, err := crypto.Wrap(masterKey, hex.EncodeToString(key))
	if err != nil {
		os.Remove(dst.Name())
		return "", err
	}
	wiv, err := crypto.Wrap(masterKey, hex.EncodeToString(iv))
	if err != nil {
		os.Remove(dst.Name())
		return "", err
	}
	if err := m.db.Model(&models.File{}).Where("id = ?", fileID).Updates(map[string]any{
		"encrypted": true, "enc_key": wk, "enc_iv": wiv, "updated_at": time.Now(),
	}).Error; err != nil {
		os.Remove(dst.Name())
		return "", err
	}
	return dst.Name(), nil
}

// streamChunks reads the temp file one sliceSize block at a time and uploads
// each via superfile2, so peak memory is a single block regardless of file size.
func (m *Manager) streamChunks(ctx context.Context, t uploadTask, token, uploadID, acctKey string, total int, sliceSize int64, accountID uint) error {
	f, err := os.Open(t.tmpPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if total == 0 {
		total = 1
	}
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
			m.meter.AddUp(int64(n)) // live meter: count bytes as they reach Baidu
			m.logTransferEvent(
				&models.TransferEvent{
					UserID: t.userID, Kind: "upload", Stage: "chunk", RefType: "job",
					RefID: t.jobID, Name: path.Base(t.logical), Path: t.logical,
					AccountID: accountID,
					OffsetStart: int64(partseq) * sliceSize,
					OffsetEnd:   int64(partseq)*sliceSize + int64(n),
					Bytes:       int64(n),
				})
			seq++
			m.setStatus(t, "running", seq*100/total, "")
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

func (m *Manager) setStatus(t uploadTask, status string, progress int, errMsg string) {
	m.jobStatus(t.jobID, t.userID, t.logical, status, progress, errMsg)
}

func (m *Manager) jobStatus(jobID string, userID uint, logical, status string, progress int, errMsg string) {
	m.db.Model(&models.Job{}).Where("id = ?", jobID).Updates(map[string]any{
		"status": status, "progress": progress, "error": errMsg, "updated_at": time.Now(),
	})
	m.hub.Publish(userID, map[string]any{
		"type": "job", "job_id": jobID, "status": status,
		"progress": progress, "logical_path": logical, "error": errMsg,
	})
}

func (m *Manager) fail(t uploadTask, err error) {
	log.Printf("[job] upload FAILED job=%s user=%d path=%s: %v", t.jobID, t.userID, t.logical, err)
	m.db.Model(&models.File{}).
		Where("logical_path = ? AND owner_user_id = ?", t.logical, t.userID).
		Update("status", "failed")
	var job models.Job
	var accountID uint
	if m.db.First(&job, "id = ?", t.jobID).Error == nil {
		accountID = job.BaiduAccountID
	}
	m.logTransferEvent(
		&models.TransferEvent{
			UserID: t.userID, Kind: "upload", Stage: "failure", RefType: "job",
			RefID: t.jobID, Name: path.Base(t.logical), Path: t.logical,
			AccountID: accountID, Bytes: t.size, Error: err.Error(),
		})
	m.setStatus(t, "failed", 0, err.Error())
}

// withRetry runs fn up to `attempts` times, logging each failure and backing off
// between tries, so a transient network hiccup on the server->Baidu leg doesn't
// abort the whole upload.
func withRetry(attempts int, label string, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		log.Printf("[job] %s attempt %d/%d failed: %v", label, i+1, attempts, err)
		if i < attempts-1 {
			time.Sleep(time.Duration(400*(1<<i)) * time.Millisecond)
		}
	}
	return err
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
