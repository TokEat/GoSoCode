package clientd

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
)

// uploadSession is the client-side state of a direct-to-Baidu upload: enough
// to push superfile2 slices without asking the server per slice. The server's
// bitmap stays authoritative via the progress reports.
type uploadSession struct {
	ID         string
	UploadID   string
	RemotePath string
	Token      string
	Size       int64
	SliceSize  int64
	Total      int
	BlockList  []string
}

// handlePlan forwards the plan to the server (as an agent, exempt from the
// server-memory concurrency cap) and raises the advertised window: the
// browser->client leg is localhost, so more slices in flight only cost this
// machine's uplink.
func (d *Daemon) handlePlan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("agent", "1")
	status, raw, err := d.serverJSON(r.Context(), r.Header.Get("Authorization"),
		http.MethodGet, "/api/files/stream/plan?"+q.Encode(), nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "无法连接服务端: "+err.Error())
		return
	}
	var plan map[string]any
	if status == http.StatusOK && json.Unmarshal(raw, &plan) == nil {
		if enabled, _ := plan["enabled"].(bool); enabled {
			if cur, _ := plan["window"].(float64); int(cur) < d.opts.Window {
				plan["window"] = d.opts.Window
			}
			plan["mode"] = "client"
			writeJSON(w, http.StatusOK, plan)
			return
		}
	}
	passthrough(w, status, raw)
}

// handleInit opens the session through the server's agent control plane, which
// returns the node token + remote path for direct pushes. The browser gets the
// same response shape as the server's stream init (credentials stripped).
func (d *Daemon) handleInit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求失败: "+err.Error())
		return
	}
	status, raw, err := d.serverJSON(r.Context(), r.Header.Get("Authorization"),
		http.MethodPost, "/api/agent/upload/init", body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "无法连接服务端: "+err.Error())
		return
	}
	if status != http.StatusOK {
		passthrough(w, status, raw)
		return
	}

	var out struct {
		SessionID   string `json:"session_id"`
		UploadID    string `json:"upload_id"`
		NeedSeqs    []int  `json:"need_seqs"`
		Rapid       bool   `json:"rapid"`
		SliceSize   int64  `json:"slice_size"`
		Total       int    `json:"total"`
		AccessToken string `json:"access_token"`
		RemotePath  string `json:"remote_path"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.SessionID == "" {
		writeErr(w, http.StatusBadGateway, "服务端响应异常")
		return
	}
	var in struct {
		Size      int64    `json:"size"`
		BlockList []string `json:"block_list"`
	}
	_ = json.Unmarshal(body, &in)

	d.mu.Lock()
	d.sessions[out.SessionID] = &uploadSession{
		ID: out.SessionID, UploadID: out.UploadID, RemotePath: out.RemotePath,
		Token: out.AccessToken, Size: in.Size, SliceSize: out.SliceSize,
		Total: out.Total, BlockList: in.BlockList,
	}
	d.mu.Unlock()
	log.Printf("[client] 上传会话 %s 已开启（直连百度，窗口 %d）", out.SessionID, d.opts.Window)

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": out.SessionID, "upload_id": out.UploadID,
		"need_seqs": out.NeedSeqs, "rapid": out.Rapid,
		"slice_size": out.SliceSize, "total": out.Total, "window": d.opts.Window,
	})
}

// session returns local state for id, rebuilding it from the server after a
// client restart so uploads survive the client process.
func (d *Daemon) session(r *http.Request, id string) (*uploadSession, int) {
	d.mu.Lock()
	sess := d.sessions[id]
	d.mu.Unlock()
	if sess != nil {
		return sess, 0
	}
	status, raw, err := d.serverJSON(r.Context(), r.Header.Get("Authorization"),
		http.MethodGet, "/api/agent/upload/session?session_id="+id, nil)
	if err != nil || status != http.StatusOK {
		if status == 0 {
			status = http.StatusBadGateway
		}
		return nil, status
	}
	var out struct {
		SessionID   string `json:"session_id"`
		UploadID    string `json:"upload_id"`
		SliceSize   int64  `json:"slice_size"`
		Total       int    `json:"total"`
		Size        int64  `json:"size"`
		BlockList   string `json:"block_list"` // JSON array stored as string
		AccessToken string `json:"access_token"`
		RemotePath  string `json:"remote_path"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.SessionID == "" {
		return nil, http.StatusBadGateway
	}
	var blocks []string
	_ = json.Unmarshal([]byte(out.BlockList), &blocks)
	sess = &uploadSession{
		ID: out.SessionID, UploadID: out.UploadID, RemotePath: out.RemotePath,
		Token: out.AccessToken, Size: out.Size, SliceSize: out.SliceSize,
		Total: out.Total, BlockList: blocks,
	}
	d.mu.Lock()
	d.sessions[id] = sess
	d.mu.Unlock()
	return sess, 0
}

// handleSlice receives one slice from the browser (localhost) and pushes it to
// Baidu directly. A 200 means cloud-synced AND server-recorded, matching the
// server's streaming semantics, so the web UI needs no changes.
func (d *Daemon) handleSlice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		d.proxy.ServeHTTP(w, r)
		return
	}
	id := r.URL.Query().Get("session")
	seq, err := strconv.Atoi(r.URL.Query().Get("seq"))
	if err != nil || seq < 0 {
		writeErr(w, http.StatusBadRequest, "invalid seq")
		return
	}
	sess, errStatus := d.session(r, id)
	if sess == nil {
		if errStatus == http.StatusNotFound {
			writeErr(w, http.StatusNotFound, "上传会话已失效，请重新上传")
		} else {
			writeErr(w, errStatus, "无法连接服务端，请稍后重试")
		}
		return
	}
	if seq >= sess.Total {
		writeErr(w, http.StatusBadRequest, "seq out of range")
		return
	}

	want := sess.SliceSize
	if seq == sess.Total-1 {
		want = sess.Size - int64(sess.Total-1)*sess.SliceSize
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, sess.SliceSize+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取分片失败: "+err.Error())
		return
	}
	if int64(len(data)) != want {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("分片大小不符: got %d want %d", len(data), want))
		return
	}
	if seq < len(sess.BlockList) {
		sum := md5.Sum(data)
		if hex.EncodeToString(sum[:]) != sess.BlockList[seq] {
			writeErr(w, http.StatusBadRequest, "数据校验未通过，请重试")
			return
		}
	}

	// data plane: this machine -> Baidu, no server in between
	if err := retry(3, func() error {
		return d.baidu.UploadChunk(r.Context(), sess.Token, sess.RemotePath, sess.UploadID, seq, data)
	}); err != nil {
		log.Printf("[client] 分片直传失败 session=%s seq=%d: %v", id, seq, err)
		writeErr(w, http.StatusBadGateway, "云端同步失败: "+err.Error())
		return
	}

	// keep the server's bitmap authoritative; if reporting fails the browser
	// retries this slice, and superfile2 is idempotent by partseq.
	rep, _ := json.Marshal(map[string]any{"session_id": id, "seqs": []int{seq}, "bytes": len(data)})
	var status int
	var raw []byte
	if err := retry(2, func() error {
		var e error
		status, raw, e = d.serverJSON(r.Context(), r.Header.Get("Authorization"),
			http.MethodPost, "/api/agent/upload/progress", rep)
		if e == nil && status >= 500 {
			return fmt.Errorf("server HTTP %d", status)
		}
		return e
	}); err != nil {
		writeErr(w, http.StatusBadGateway, "云端已同步但进度上报失败，请重试")
		return
	}
	if status == http.StatusNotFound {
		writeErr(w, http.StatusNotFound, "上传会话已失效，请重新上传")
		return
	}
	if status != http.StatusOK {
		passthrough(w, status, raw)
		return
	}
	var pr struct {
		DoneCount int `json:"done_count"`
		Total     int `json:"total"`
	}
	_ = json.Unmarshal(raw, &pr)
	writeJSON(w, http.StatusOK, map[string]any{"seq": seq, "done_count": pr.DoneCount, "total": pr.Total})
}
