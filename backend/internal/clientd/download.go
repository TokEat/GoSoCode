package clientd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dlSegSize   = 8 << 20  // per-connection segment
	dlReportAt  = 32 << 20 // report transferred bytes to the server this often
	dlRetryEach = 4        // attempts per segment (with dlink re-resolve)
)

type dlInfo struct {
	Dlink string `json:"dlink"`
	Token string `json:"access_token"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
}

// dlSource holds the resolved dlink and refreshes it once when a segment hits
// a 4xx (dlinks are temporary and can be invalidated early).
type dlSource struct {
	d      *Daemon
	auth   string
	fileID string
	mu     sync.Mutex
	info   *dlInfo
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
	info, _, _, err := s.d.resolve(ctx, s.auth, s.fileID)
	if err != nil {
		return err
	}
	s.info = info
	return nil
}

func (d *Daemon) resolve(ctx context.Context, auth, fileID string) (*dlInfo, int, []byte, error) {
	status, raw, err := d.serverJSON(ctx, auth, http.MethodGet, "/api/agent/download/resolve?id="+fileID, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	if status != http.StatusOK {
		return nil, status, raw, fmt.Errorf("resolve HTTP %d", status)
	}
	var info dlInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.Dlink == "" {
		return nil, status, raw, fmt.Errorf("服务端响应异常")
	}
	return &info, status, raw, nil
}

// parseRange handles the single-range forms the web UI sends: "bytes=a-" and
// "bytes=a-b".
func parseRange(h string, size int64) (start, end int64, ok bool) {
	h = strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(h, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if p := strings.TrimSpace(parts[1]); p != "" {
		if e, err := strconv.ParseInt(p, 10, 64); err == nil && e >= start && e < size {
			end = e
		}
	}
	return start, end, true
}

// handleDownload streams a file to the browser while fetching it from Baidu
// directly with several parallel ranged connections (ordered re-assembly), so
// the effective speed is the sum of the connections and no byte touches the
// platform server.
func (d *Daemon) handleDownload(w http.ResponseWriter, r *http.Request, fileID string) {
	auth := r.Header.Get("Authorization")
	info, status, raw, err := d.resolve(r.Context(), auth, fileID)
	if err != nil {
		if raw != nil {
			passthrough(w, status, raw)
		} else {
			writeErr(w, http.StatusBadGateway, "无法连接服务端: "+err.Error())
		}
		return
	}

	start, end := int64(0), info.Size-1
	ranged := false
	if rh := r.Header.Get("Range"); rh != "" && info.Size > 0 {
		if s0, e0, ok := parseRange(rh, info.Size); ok {
			start, end, ranged = s0, e0, true
		}
	}

	h := w.Header()
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, info.Name))
	h.Set("Accept-Ranges", "bytes")
	if info.Size <= 0 { // empty file: headers only
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	length := end - start + 1
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if ranged {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	nseg := int((length + dlSegSize - 1) / dlSegSize)
	conns := d.opts.DLConns
	if conns > nseg {
		conns = nseg
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	src := &dlSource{d: d, auth: auth, fileID: fileID, info: info}

	results := make([]chan []byte, nseg)
	for i := range results {
		results[i] = make(chan []byte, 1)
	}
	var next int64
	var fetchErr atomic.Value
	worker := func() {
		for {
			i := int(atomic.AddInt64(&next, 1) - 1)
			if i >= nseg || ctx.Err() != nil {
				return
			}
			s0 := start + int64(i)*dlSegSize
			e0 := s0 + dlSegSize - 1
			if e0 > end {
				e0 = end
			}
			data, err := src.fetchSegment(ctx, s0, e0)
			if err != nil {
				fetchErr.Store(err)
				cancel()
				return
			}
			select {
			case results[i] <- data:
			case <-ctx.Done():
				return
			}
		}
	}
	for k := 0; k < conns; k++ {
		go worker()
	}

	flusher, _ := w.(http.Flusher)
	var written, unreported int64
	for i := 0; i < nseg; i++ {
		var data []byte
		select {
		case data = <-results[i]:
		case <-ctx.Done():
			if e, _ := fetchErr.Load().(error); e != nil {
				log.Printf("[client] 直连下载中断 file=%s after %d bytes: %v", fileID, written, e)
			}
			d.reportDown(auth, fileID, unreported)
			return
		}
		if _, err := w.Write(data); err != nil {
			cancel() // browser went away; it resumes via Range
			d.reportDown(auth, fileID, unreported+int64(len(data)))
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		written += int64(len(data))
		unreported += int64(len(data))
		if unreported >= dlReportAt {
			d.reportDown(auth, fileID, unreported)
			unreported = 0
		}
	}
	d.reportDown(auth, fileID, unreported)
}

// fetchSegment reads one ranged segment from Baidu, re-resolving the dlink on
// upstream errors (it may have expired mid-download).
func (s *dlSource) fetchSegment(ctx context.Context, s0, e0 int64) ([]byte, error) {
	rng := fmt.Sprintf("bytes=%d-%d", s0, e0)
	wantLen := e0 - s0 + 1
	var lastErr error
	for attempt := 0; attempt < dlRetryEach; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(300*(1<<attempt)) * time.Millisecond):
			}
		}
		info := s.get()
		resp, err := s.d.baidu.OpenDownload(ctx, info.Token, info.Dlink, rng)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			lastErr = fmt.Errorf("云端返回 HTTP %d", resp.StatusCode)
			if err := s.refresh(ctx, info); err != nil {
				lastErr = fmt.Errorf("云端直链失效且无法刷新: %w", err)
			}
			continue
		}
		// 200 instead of 206 means Range was ignored; only usable from offset 0.
		if resp.StatusCode == http.StatusOK && s0 != 0 {
			resp.Body.Close()
			lastErr = fmt.Errorf("云端不支持断点续传")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, wantLen))
		resp.Body.Close()
		if err != nil || int64(len(data)) != wantLen {
			lastErr = fmt.Errorf("读取云端数据不完整: %v", err)
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

// reportDown tells the server how many bytes moved directly (traffic
// accounting + live chart). Uses its own context: the browser request may
// already be gone when the final report fires.
func (d *Daemon) reportDown(auth, fileID string, bytes int64) {
	if bytes <= 0 {
		return
	}
	id, _ := strconv.Atoi(fileID)
	body, _ := json.Marshal(map[string]any{"file_id": id, "bytes": bytes})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, _, err := d.serverJSON(ctx, auth, http.MethodPost, "/api/agent/download/report", body); err != nil {
		log.Printf("[client] 下载流量上报失败 file=%s bytes=%d: %v", fileID, bytes, err)
	}
}
