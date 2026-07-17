package clientd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	cryptoutil "gosocode/internal/crypto"
)

const (
	dlSegSize     = 8 << 20  // per-connection segment
	dlReportAt    = 32 << 20 // report transferred bytes to the server this often
	dlRetryEach   = 5        // attempts per segment (with dlink re-resolve)
	dlRetryRounds = 6        // reconciliation rounds that only retry missing segments
)

type dlInfo struct {
	Dlink     string `json:"dlink"`
	Token     string `json:"access_token"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	NodeID    uint   `json:"node_id"`
	VipType   int    `json:"vip_type"`
	Encrypted bool   `json:"encrypted"`
	EncKey    string `json:"enc_key"`
	EncIV     string `json:"enc_iv"`
}

// downloadConnsForTier keeps ordinary accounts conservative while still
// allowing two ranged connections. Paid tiers may use more, but never beyond
// the operator's -dl-conns setting.
func downloadConnsForTier(requested, vipType int) int {
	limit := 2
	if vipType == 1 {
		limit = 4
	} else if vipType >= 2 {
		limit = 8
	}
	if requested < 1 {
		requested = 1
	}
	if requested > limit {
		requested = limit
	}
	return requested
}

// dlSource holds the resolved dlink and refreshes it when a segment hits a
// 4xx (dlinks are temporary and can be invalidated early).
type dlSource struct {
	d    *Daemon
	auth string
	path string
	task *Task
	mu   sync.Mutex
	info *dlInfo
}

func (s *dlSource) get() *dlInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *dlSource) refresh(ctx context.Context, stale *dlInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info != stale { // another worker already refreshed
		return nil
	}
	s.task.logf("refreshing expired cloud download link")
	var info *dlInfo
	err := retryTask(ctx, s.task, 5, "refresh cloud download link", func() error {
		var e error
		info, _, e = s.d.resolve(ctx, s.auth, s.path, s.task.pwd)
		return e
	})
	if err != nil {
		return err
	}
	s.info = info
	return nil
}

func (d *Daemon) resolve(ctx context.Context, auth, logicalPath, pwd string) (*dlInfo, []byte, error) {
	q := "/api/agent/download/resolve?path=" + url.QueryEscape(logicalPath)
	if pwd != "" {
		q += "&pwd=" + url.QueryEscape(pwd)
	}
	status, raw, err := d.serverJSON(ctx, auth, http.MethodGet, q, nil)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, raw, fmt.Errorf("%s", serverErrMsg(raw, "无法获取下载地址"))
	}
	var info dlInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.Dlink == "" {
		return nil, raw, fmt.Errorf("服务端响应异常")
	}
	return &info, raw, nil
}

// runDownload executes one download entirely inside this process: resolve the
// dlink through the server, fetch segments from Baidu with parallel ranged
// connections, write them at their offsets in a local .gscpart file, then
// rename into the configured download directory.
func (d *Daemon) runDownload(ctx context.Context, t *Task, logicalPath string) {
	t.logf("resolving cloud download source path=%q", logicalPath)
	var info *dlInfo
	err := retryTask(ctx, t, 5, "resolve cloud download source", func() error {
		var e error
		info, _, e = d.resolve(ctx, t.auth, logicalPath, t.pwd)
		return e
	})
	if err != nil {
		t.fail(err.Error())
		return
	}
	// Encrypted at rest: decrypt each fetched segment locally with the per-file
	// key the (trusted) server handed us.
	var encKey, encIV []byte
	if info.Encrypted {
		encKey, err = hex.DecodeString(info.EncKey)
		if err == nil {
			encIV, err = hex.DecodeString(info.EncIV)
		}
		if err != nil || len(encKey) != 32 || len(encIV) != 16 {
			t.fail("文件加密密钥无效")
			return
		}
		t.logf("encrypted file; segments will be decrypted locally")
	}
	t.set(func(t *Task) {
		if info.Name != "" {
			t.Name = info.Name
		}
		t.Size = info.Size
	})
	t.logf("download source resolved node=%d vip_type=%d name=%q size=%d", info.NodeID, info.VipType, info.Name, info.Size)
	t.setStatus("running")
	if ctx.Err() != nil {
		return
	}

	cfg := d.config()
	if err := os.MkdirAll(cfg.DownloadDir, 0o755); err != nil {
		t.fail("无法创建下载目录: " + err.Error())
		return
	}
	target := uniquePath(cfg.DownloadDir, t.Name)
	part := target + ".gscpart"
	t.logf("opening local target part=%q final=%q", part, target)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		t.fail("无法创建本地文件: " + err.Error())
		return
	}

	var reported int64 // bytes not yet reported to the server
	cleanup := func(ok bool) {
		f.Close()
		if !ok {
			_ = os.Remove(part)
		}
		if n := atomic.SwapInt64(&reported, 0); n > 0 {
			d.reportDown(t.auth, info.NodeID, n)
		}
	}

	if info.Size > 0 {
		src := &dlSource{d: d, auth: t.auth, path: logicalPath, task: t, info: info}
		nseg := int((info.Size + dlSegSize - 1) / dlSegSize)
		conns := downloadConnsForTier(d.opts.DLConns, info.VipType)
		if conns > nseg {
			conns = nseg
		}
		t.logf("segment download started segments=%d concurrency=%d requested_concurrency=%d segment_size=%d", nseg, conns, d.opts.DLConns, dlSegSize)
		progress := make([]int64, nseg)
		var progressMu sync.Mutex
		setSegmentProgress := func(i int, current int64) {
			progressMu.Lock()
			delta := current - progress[i]
			progress[i] = current
			progressMu.Unlock()
			if delta != 0 {
				t.addDone(delta)
			}
		}
		completed := make([]bool, nseg)
		pending := make([]int, nseg)
		for i := range pending {
			pending[i] = i
		}
		var lastErr error
		for round := 0; round < dlRetryRounds && len(pending) > 0; round++ {
			t.logf("download reconciliation round=%d/%d missing_segments=%d", round+1, dlRetryRounds, len(pending))
			var next int64
			var errMu sync.Mutex
			var wg sync.WaitGroup
			for k := 0; k < conns; k++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						at := int(atomic.AddInt64(&next, 1) - 1)
						if at >= len(pending) || ctx.Err() != nil {
							return
						}
						i := pending[at]
						s0 := int64(i) * dlSegSize
						e0 := s0 + dlSegSize - 1
						if e0 > info.Size-1 {
							e0 = info.Size - 1
						}
						data, err := src.fetchSegment(ctx, s0, e0, func(current int64) {
							setSegmentProgress(i, current)
						})
						if err == nil && encKey != nil {
							if err = cryptoutil.XORAt(data, encKey, encIV, s0); err != nil {
								err = fmt.Errorf("解密分段失败: %v", err)
							}
						}
						if err == nil {
							_, err = f.WriteAt(data, s0)
							if err != nil {
								setSegmentProgress(i, 0)
								err = fmt.Errorf("写入本地文件失败: %v", err)
							}
						}
						if err != nil {
							errMu.Lock()
							lastErr = err
							errMu.Unlock()
							continue
						}
						completed[i] = true
						if atomic.AddInt64(&reported, int64(len(data))) >= dlReportAt {
							d.reportDown(t.auth, info.NodeID, atomic.SwapInt64(&reported, 0))
						}
					}
				}()
			}
			wg.Wait()
			if ctx.Err() != nil { // user cancel
				t.logf("download workers stopped by cancellation")
				cleanup(false)
				return
			}
			pending = pending[:0]
			for i, done := range completed {
				if !done {
					pending = append(pending, i)
				}
			}
			if len(pending) > 0 && round < dlRetryRounds-1 {
				t.logf("download round incomplete; preserving %d completed segments and retrying %d missing segments", nseg-len(pending), len(pending))
				if err := waitTaskRetry(ctx, t, "download missing segments", round+1); err != nil {
					cleanup(false)
					return
				}
			}
		}
		if len(pending) > 0 {
			if lastErr == nil {
				lastErr = fmt.Errorf("unknown network error")
			}
			t.fail(fmt.Sprintf("仍有 %d 个下载分段未完成: %v", len(pending), lastErr))
			cleanup(false)
			return
		}
		t.logf("all download segments written bytes=%d", info.Size)
	}

	cleanup(true)
	if err := os.Rename(part, target); err != nil {
		t.fail("保存本地文件失败: " + err.Error())
		return
	}
	t.set(func(t *Task) { t.LocalPath = target })
	t.setStatus("done")
	t.logf("completed remote=%q local=%q bytes=%d", t.RemotePath, target, info.Size)

	// history entry, same as the browser flow does on completion
	body, _ := json.Marshal(map[string]any{"kind": "download", "name": t.Name, "path": t.RemotePath, "size": info.Size})
	bg, cancelBg := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelBg()
	_, _, _ = d.serverJSON(bg, t.auth, http.MethodPost, "/api/transfers/log", body)
}

// fetchSegment reads one ranged segment from Baidu, re-resolving the dlink on
// upstream errors (it may have expired mid-download).
func (s *dlSource) fetchSegment(ctx context.Context, s0, e0 int64, onProgress func(int64)) ([]byte, error) {
	rng := fmt.Sprintf("bytes=%d-%d", s0, e0)
	wantLen := e0 - s0 + 1
	var lastErr error
	for attempt := 0; attempt < dlRetryEach; attempt++ {
		onProgress(0)
		if attempt > 0 {
			select {
			case <-ctx.Done():
				onProgress(0)
				return nil, ctx.Err()
			case <-time.After(time.Duration(300*(1<<attempt)) * time.Millisecond):
			}
		}
		info := s.get()
		resp, err := s.d.baidu.OpenDownload(ctx, info.Token, info.Dlink, rng)
		if err != nil {
			lastErr = err
			s.task.logf("segment range=%s failed attempt=%d/%d: %v", rng, attempt+1, dlRetryEach, lastErr)
			continue
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			lastErr = fmt.Errorf("云端返回 HTTP %d", resp.StatusCode)
			if err := s.refresh(ctx, info); err != nil {
				lastErr = fmt.Errorf("云端直链失效且无法刷新: %w", err)
			}
			s.task.logf("segment range=%s failed attempt=%d/%d: %v", rng, attempt+1, dlRetryEach, lastErr)
			continue
		}
		// 200 instead of 206 means Range was ignored; only usable from offset 0.
		if resp.StatusCode == http.StatusOK && s0 != 0 {
			resp.Body.Close()
			lastErr = fmt.Errorf("云端不支持分段下载")
			s.task.logf("segment range=%s failed attempt=%d/%d: %v", rng, attempt+1, dlRetryEach, lastErr)
			continue
		}
		data := make([]byte, wantLen)
		var received int64
		var readErr error
		for received < wantLen {
			n, e := resp.Body.Read(data[received:])
			if n > 0 {
				received += int64(n)
				onProgress(received)
			}
			if e != nil {
				if e != io.EOF {
					readErr = e
				}
				break
			}
		}
		resp.Body.Close()
		if readErr != nil || received != wantLen {
			lastErr = fmt.Errorf("读取云端数据不完整: received=%d want=%d error=%v", received, wantLen, readErr)
			s.task.logf("segment range=%s failed attempt=%d/%d: %v", rng, attempt+1, dlRetryEach, lastErr)
			continue
		}
		return data, nil
	}
	onProgress(0)
	return nil, lastErr
}

// reportDown tells the server how many bytes moved directly (traffic
// accounting + live chart). Uses its own context: the task may already be
// canceled when the final report fires.
func (d *Daemon) reportDown(auth string, nodeID uint, bytes int64) {
	if bytes <= 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"node_id": nodeID, "bytes": bytes})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, raw, err := d.serverJSON(ctx, auth, http.MethodPost, "/api/agent/download/report", body)
	if err != nil {
		log.Printf("[client] 下载流量上报失败 node=%d bytes=%d: %v", nodeID, bytes, err)
	} else if status != http.StatusOK {
		log.Printf("[client] 下载流量上报失败 node=%d bytes=%d HTTP=%d error=%s", nodeID, bytes, status, serverErrMsg(raw, "unknown"))
	}
}
