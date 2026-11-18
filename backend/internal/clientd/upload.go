package clientd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gosocode/internal/baidu"
	cryptoutil "gosocode/internal/crypto"
)

// uploadInit is the agent init response: everything needed to push slices to
// Baidu without the server in the data path.
type uploadInit struct {
	SessionID   string `json:"session_id"`
	UploadID    string `json:"upload_id"`
	NeedSeqs    []int  `json:"need_seqs"`
	Rapid       bool   `json:"rapid"`
	SliceSize   int64  `json:"slice_size"`
	Total       int    `json:"total"`
	AccessToken string `json:"access_token"`
	RemotePath  string `json:"remote_path"`

	// upload server base URLs from locateupload; retries rotate through them
	// because the default server intermittently answers empty HTTP 500s.
	servers []string

	// at-rest encryption: when set, every slice is CTR-encrypted at its file
	// offset before hashing/pushing, so Baidu only sees ciphertext.
	encKey []byte
	encIV  []byte
}

func (s *uploadInit) pickServer(attempt int) string {
	if len(s.servers) == 0 {
		return ""
	}
	return s.servers[attempt%len(s.servers)]
}

// runUpload executes one upload entirely inside this process: local md5
// pre-scan, agent init (server does precreate and hands back uploadid + node
// token), windowed slice pushes straight to Baidu, then the regular stream
// finish on the server (create + accounting).
func (d *Daemon) runUpload(ctx context.Context, t *Task) {
	// 1. plan: which slice size to hash with (node tier dependent)
	t.logf("planning upload with server")
	t.setDetail("正在向服务端申请上传计划")
	q := url.Values{}
	q.Set("size", strconv.FormatInt(t.Size, 10))
	q.Set("path", t.RemotePath)
	q.Set("agent", "1")
	var status int
	var raw []byte
	err := retryTask(ctx, t, 5, "request upload plan", func() error {
		var callErr error
		status, raw, callErr = d.serverJSON(ctx, t.auth, http.MethodGet, "/api/files/stream/plan?"+q.Encode(), nil)
		if callErr == nil && (status == http.StatusTooManyRequests || status >= 500) {
			return fmt.Errorf("server HTTP %d: %s", status, serverErrMsg(raw, "temporary error"))
		}
		return callErr
	})
	if err != nil {
		t.fail("无法连接服务端: " + err.Error())
		return
	}
	var plan struct {
		Enabled   bool   `json:"enabled"`
		SliceSize int64  `json:"slice_size"`
		Encrypt   bool   `json:"encrypt"`
		EncKey    string `json:"enc_key"`
		EncIV     string `json:"enc_iv"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &plan) != nil || !plan.Enabled || plan.SliceSize <= 0 {
		t.fail(serverErrMsg(raw, "服务端暂无可用存储空间或流式上传未启用"))
		return
	}
	sliceSize := plan.SliceSize
	var encKey, encIV []byte
	if plan.Encrypt {
		// Prefer the server-issued per-node key (+ fresh per-file IV); fall back
		// to a locally generated pair against older servers — either way the key
		// actually used is reported back at init and stored server-side.
		if k, e1 := hex.DecodeString(plan.EncKey); e1 == nil && len(k) == 32 {
			if v, e2 := hex.DecodeString(plan.EncIV); e2 == nil && len(v) == 16 {
				encKey, encIV = k, v
			}
		}
		if encKey == nil {
			var kerr error
			if encKey, encIV, kerr = cryptoutil.NewFileKey(); kerr != nil {
				t.fail("生成加密密钥失败: " + kerr.Error())
				return
			}
		}
		t.logf("server requires encrypted storage; slices will be AES-CTR encrypted locally")
	}
	t.logf("plan ready slice_size=%d window=%d encrypt=%v", sliceSize, d.opts.Window, plan.Encrypt)

	// 2. hash + init (the node is only locked at init; if its tier implies a
	// different slice size, re-hash once with the new value)
	var ses uploadInit
	var blocks []string
	for attempt := 0; ; attempt++ {
		t.setStatus("hashing")
		t.setDetail("正在校验，准备秒传… 0%")
		t.logf("hashing local file attempt=%d slice_size=%d", attempt+1, sliceSize)
		var size int64
		blocks, size, err = sliceMD5sWithProgress(t, sliceSize, encKey, encIV)
		if err != nil {
			t.fail("读取本地文件失败: " + err.Error())
			return
		}
		if size != t.Size {
			t.logf("local file size changed stat=%d read=%d", t.Size, size)
			t.set(func(t *Task) { t.Size = size }) // file changed on disk since stat
		}
		t.logf("hashing complete slices=%d size=%d", len(blocks), size)
		if ctx.Err() != nil {
			return
		}
		init := map[string]any{
			"name": t.Name, "path": t.RemotePath, "size": size,
			"slice_size": sliceSize, "block_list": blocks,
		}
		if encKey != nil {
			init["enc_key"] = hex.EncodeToString(encKey)
			init["enc_iv"] = hex.EncodeToString(encIV)
		}
		t.setDetail("正在创建云端上传会话")
		body, _ := json.Marshal(init)
		err = retryTask(ctx, t, 5, "initialize upload session", func() error {
			var callErr error
			status, raw, callErr = d.serverJSONAccess(ctx, t.auth, http.MethodPost, "/api/agent/upload/init", body, t.pwd)
			if callErr == nil && (status == http.StatusTooManyRequests || status >= 500) {
				return fmt.Errorf("server HTTP %d: %s", status, serverErrMsg(raw, "temporary error"))
			}
			return callErr
		})
		if err != nil {
			t.fail("无法连接服务端: " + err.Error())
			return
		}
		if status == http.StatusConflict && attempt == 0 {
			var conflict struct {
				SliceSize int64 `json:"slice_size"`
			}
			if json.Unmarshal(raw, &conflict) == nil && conflict.SliceSize > 0 && conflict.SliceSize != sliceSize {
				t.logf("server selected different slice_size=%d; rehashing", conflict.SliceSize)
				sliceSize = conflict.SliceSize
				continue
			}
		}
		if status != http.StatusOK {
			t.fail(serverErrMsg(raw, "上传初始化失败"))
			return
		}
		if json.Unmarshal(raw, &ses) != nil || ses.SessionID == "" {
			t.fail("服务端响应异常")
			return
		}
		ses.encKey, ses.encIV = encKey, encIV
		break
	}
	t.logf("upload session opened session=%s total_slices=%d need_slices=%d rapid=%v", ses.SessionID, ses.Total, len(ses.NeedSeqs), ses.Rapid)

	// on any abort the server session must be canceled to free the node's load slot
	abort := func() {
		body, _ := json.Marshal(map[string]string{"session_id": ses.SessionID})
		bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _, _ = d.serverJSONAccess(bg, t.auth, http.MethodPost, "/api/files/stream/cancel", body, t.pwd)
	}

	t.setStatus("running")
	if ses.Rapid {
		t.logf("rapid upload hit; no slice transfer needed")
		t.setDetail("秒传命中，无需传输数据")
		t.set(func(t *Task) { t.Done = t.Size })
	} else {
		if servers, err := d.baidu.LocateUpload(ctx, ses.AccessToken, ses.RemotePath, ses.UploadID); err != nil || len(servers) == 0 {
			t.logf("locateupload unavailable, using default upload server: %v", err)
		} else {
			ses.servers = servers
			t.logf("upload servers resolved count=%d primary=%s", len(servers), servers[0])
		}
		if err := d.syncUploadMissing(ctx, t, &ses, sliceSize, ses.NeedSeqs); err != nil {
			if ctx.Err() == nil { // real failure (not user cancel)
				t.fail(err.Error())
			}
			abort()
			return
		}
	}

	// 3. finish (create + accounting run server-side). Ambiguous/transient
	// failures are retried; a bitmap conflict only re-sends missing slices.
	finishErr := "云端保存未完成"
	for attempt := 0; attempt < 5; attempt++ {
		t.logf("finishing cloud file attempt=%d/5 session=%s", attempt+1, ses.SessionID)
		t.setDetail("云端合并处理中…")
		body, _ := json.Marshal(map[string]string{"session_id": ses.SessionID})
		status, raw, err = d.serverJSONAccess(ctx, t.auth, http.MethodPost, "/api/files/stream/finish", body, t.pwd)
		if err != nil {
			finishErr = "无法连接服务端: " + err.Error()
		} else if status == http.StatusOK {
			t.set(func(t *Task) { t.Done = t.Size })
			t.setDetail("")
			t.setStatus("done")
			t.logf("completed remote=%q bytes=%d", t.RemotePath, t.Size)
			return
		} else if status == http.StatusConflict {
			var need struct {
				NeedSeqs []int `json:"need_seqs"`
			}
			if json.Unmarshal(raw, &need) == nil && len(need.NeedSeqs) > 0 {
				t.logf("finish reported %d missing slices; retrying them", len(need.NeedSeqs))
				if err := d.syncUploadMissing(ctx, t, &ses, sliceSize, need.NeedSeqs); err == nil {
					continue
				} else {
					finishErr = err.Error()
				}
			} else {
				finishErr = serverErrMsg(raw, "云端仍有分片未同步")
			}
		} else {
			finishErr = serverErrMsg(raw, fmt.Sprintf("server HTTP %d", status))
		}
		if ctx.Err() != nil {
			abort()
			return
		}
		if attempt < 4 {
			if err := waitTaskRetry(ctx, t, "finish cloud file", attempt+1); err != nil {
				abort()
				return
			}
		}
	}
	t.fail(finishErr)
	t.logf("upload session retained for server-side reconciliation session=%s", ses.SessionID)
}

// countReader reports cumulative bytes read; used for hashing progress.
type countReader struct {
	r  io.Reader
	n  int64
	fn func(int64)
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		c.n += int64(n)
		c.fn(c.n)
	}
	return n, err
}

// sliceMD5sWithProgress hashes the file per slice (the AES-CTR ciphertext when
// encryption is on — that is what precreate/create will verify), publishing a
// percentage to the task detail as it scans.
func sliceMD5sWithProgress(t *Task, sliceSize int64, key, iv []byte) ([]string, int64, error) {
	f, err := os.Open(t.LocalPath)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var src io.Reader = f
	if key != nil {
		if src, err = cryptoutil.NewCTRReader(f, key, iv, 0); err != nil {
			return nil, 0, err
		}
	}
	lastPct := -1
	cr := &countReader{r: src, fn: func(read int64) {
		if t.Size <= 0 {
			return
		}
		// every 5% to keep the phase log readable
		if pct := int(read * 100 / t.Size); pct <= 100 && (lastPct < 0 || pct-lastPct >= 5) {
			lastPct = pct
			t.setDetail(fmt.Sprintf("正在校验，准备秒传… %d%%", pct))
		}
	}}
	return baidu.SliceReaderMD5s(cr, sliceSize)
}

func uploadMissingBytes(size, sliceSize int64, seqs []int) int64 {
	var missing int64
	for _, seq := range seqs {
		off := int64(seq) * sliceSize
		length := sliceSize
		if off+length > size {
			length = size - off
		}
		if length > 0 {
			missing += length
		}
	}
	return missing
}

func (d *Daemon) uploadNeedSeqs(ctx context.Context, t *Task, sessionID string) ([]int, error) {
	var status int
	var raw []byte
	err := retryTask(ctx, t, 4, "query missing upload slices", func() error {
		var callErr error
		status, raw, callErr = d.serverJSONAccess(ctx, t.auth, http.MethodGet,
			"/api/files/stream/status?session="+url.QueryEscape(sessionID), nil, t.pwd)
		if callErr != nil {
			return callErr
		}
		if status != http.StatusOK {
			return fmt.Errorf("server HTTP %d: %s", status, serverErrMsg(raw, "status query failed"))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		NeedSeqs []int `json:"need_seqs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析上传进度失败: %w", err)
	}
	return out.NeedSeqs, nil
}

// syncUploadMissing performs several reconciliation rounds. Each round asks
// the server for its authoritative bitmap and only re-sends slices still marked
// missing. superfile2 is idempotent by partseq.
func (d *Daemon) syncUploadMissing(ctx context.Context, t *Task, ses *uploadInit, sliceSize int64, need []int) error {
	var lastErr error
	for round := 0; round < 6; round++ {
		missing := uploadMissingBytes(t.Size, sliceSize, need)
		t.set(func(t *Task) { t.Done = t.Size - missing })
		if len(need) == 0 {
			return nil
		}
		t.logf("upload reconciliation round=%d/6 missing_slices=%d missing_bytes=%d", round+1, len(need), missing)
		pushErr := d.pushSlices(ctx, t, ses, sliceSize, need)
		serverNeed, statusErr := d.uploadNeedSeqs(ctx, t, ses.SessionID)
		if statusErr == nil {
			need = serverNeed
			missing = uploadMissingBytes(t.Size, sliceSize, need)
			t.set(func(t *Task) { t.Done = t.Size - missing })
			if len(need) == 0 {
				return nil
			}
		}
		if pushErr != nil {
			lastErr = pushErr
		} else if statusErr != nil {
			lastErr = statusErr
		} else {
			lastErr = fmt.Errorf("仍有 %d 个分片未同步", len(need))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if round < 5 {
			if err := waitTaskRetry(ctx, t, "upload missing slices", round+1); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("补传未完成分片失败: %w", lastErr)
}

// ulStallTimeout aborts a slice attempt when no request bytes move for this
// long (the Baidu HTTP client has no timeout; packet loss can freeze a
// connection forever otherwise).
const ulStallTimeout = 30 * time.Second

// pushSlices uploads the given slices to Baidu with a window of concurrent
// connections, reporting each success to the server (bitmap + live meter).
func (d *Daemon) pushSlices(ctx context.Context, t *Task, ses *uploadInit, sliceSize int64, seqs []int) error {
	f, err := os.Open(t.LocalPath)
	if err != nil {
		return fmt.Errorf("读取本地文件失败: %v", err)
	}
	defer f.Close()

	// Per-tier window (inferred from the planned slice size): bounds bytes in
	// flight so big-slice tiers don't trickle too many parallel requests on a
	// limited uplink, while small-slice tiers get enough parallelism to beat
	// Baidu's per-connection throttle.
	lim := baidu.LimitsForSliceSize(sliceSize)
	conc := d.opts.Window
	if conc > lim.UploadWindow {
		conc = lim.UploadWindow
	}
	if conc < 1 {
		conc = 1
	}

	inner, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var pushErr error
	var doneSlices int32
	t.logf("slice batch started count=%d concurrency=%d slice_size=%d", len(seqs), conc, sliceSize)
	// Replace the "创建云端上传会话" phase immediately; the per-slice update
	// below only fires when a whole slice completes, which can take a while.
	t.setDetail(fmt.Sprintf("正在上传：云端已同步 %d/%d 分片", ses.Total-len(seqs), ses.Total))

	// Streaming per-slice progress so speed/ETA move during a slice, not only
	// when one completes (big VIP slices otherwise leave the meter at 0 for
	// most of the transfer). A retried slice rolls its partial bytes back.
	var progressMu sync.Mutex
	sliceSent := map[int]int64{}
	setSliceProgress := func(seq int, current int64) {
		progressMu.Lock()
		delta := current - sliceSent[seq]
		sliceSent[seq] = current
		progressMu.Unlock()
		if delta != 0 {
			t.addDone(delta)
		}
	}

	for _, seq := range seqs {
		if inner.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			defer func() { <-sem }()
			off := int64(seq) * sliceSize
			length := sliceSize
			if off+length > t.Size {
				length = t.Size - off
			}
			buf := make([]byte, length)
			if _, err := f.ReadAt(buf, off); err != nil {
				mu.Lock()
				pushErr = fmt.Errorf("读取本地文件失败: %v", err)
				mu.Unlock()
				cancel()
				return
			}
			if ses.encKey != nil {
				if err := cryptoutil.XORAt(buf, ses.encKey, ses.encIV, off); err != nil {
					mu.Lock()
					pushErr = fmt.Errorf("加密分片失败: %v", err)
					mu.Unlock()
					cancel()
					return
				}
			}
			attempt := seq // stagger start so concurrent slices spread across servers
			if err := retryTask(inner, t, 5, fmt.Sprintf("upload slice seq=%d", seq), func() error {
				server := ses.pickServer(attempt)
				attempt++
				setSliceProgress(seq, 0) // reset partial bytes from a failed attempt
				// Stall watchdog: no request bytes for ulStallTimeout aborts
				// this attempt so a frozen connection is retried.
				attemptCtx, cancelAttempt := context.WithCancel(inner)
				defer cancelAttempt()
				watchdog := time.AfterFunc(ulStallTimeout, cancelAttempt)
				defer watchdog.Stop()
				err := d.baidu.UploadChunkToProgress(attemptCtx, server, ses.AccessToken, ses.RemotePath, ses.UploadID, seq, buf, func(sent int64) {
					watchdog.Reset(ulStallTimeout)
					if sent > length {
						sent = length // exclude multipart framing overhead
					}
					setSliceProgress(seq, sent)
				})
				if err != nil && inner.Err() == nil && attemptCtx.Err() != nil {
					err = fmt.Errorf("上传连接停滞超过 %s，断开重试", ulStallTimeout)
				}
				return err
			}); err != nil {
				mu.Lock()
				pushErr = fmt.Errorf("云端同步失败: %v", err)
				mu.Unlock()
				cancel()
				return
			}
			rep, _ := json.Marshal(map[string]any{"session_id": ses.SessionID, "seqs": []int{seq}, "bytes": length})
			if err := retryTask(inner, t, 4, fmt.Sprintf("report slice seq=%d", seq), func() error {
				st, _, e := d.serverJSON(inner, t.auth, http.MethodPost, "/api/agent/upload/progress", rep)
				if e == nil && st != http.StatusOK {
					return fmt.Errorf("server HTTP %d", st)
				}
				return e
			}); err != nil {
				mu.Lock()
				pushErr = fmt.Errorf("进度上报失败: %v", err)
				mu.Unlock()
				cancel()
				return
			}
			setSliceProgress(seq, length)
			synced := ses.Total - len(seqs) + int(atomic.AddInt32(&doneSlices, 1))
			t.setDetail(fmt.Sprintf("正在上传：云端已同步 %d/%d 分片", synced, ses.Total))
		}(seq)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if pushErr == nil {
		t.logf("slice batch completed count=%d", len(seqs))
	}
	return pushErr
}
