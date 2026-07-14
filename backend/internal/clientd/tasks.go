package clientd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Task is one client-side transfer executed by this process. The web UI only
// polls these for progress; the bytes flow local disk <-> Baidu directly.
type Task struct {
	ID         string
	Kind       string // upload | download
	Name       string
	LocalPath  string
	RemotePath string
	Size       int64
	Done       int64
	Status     string // waiting | hashing | running | done | error | canceled
	Error      string
	Speed      int64 // bytes/s, sampled at poll time
	CreatedAt  time.Time

	mu       sync.Mutex
	auth     string
	cancel   context.CancelFunc
	lastDone int64
	lastAt   time.Time
}

// TaskView is the lock-free JSON snapshot handed to the web UI.
type TaskView struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	LocalPath  string    `json:"local_path"`
	RemotePath string    `json:"remote_path"`
	Size       int64     `json:"size"`
	Done       int64     `json:"done"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Speed      int64     `json:"speed"`
	CreatedAt  time.Time `json:"created_at"`
}

func (t *Task) set(fn func(*Task)) {
	t.mu.Lock()
	fn(t)
	t.mu.Unlock()
}

func (t *Task) logf(format string, args ...any) {
	prefix := fmt.Sprintf("[client][task=%s][%s] ", t.ID, t.Kind)
	log.Printf(prefix+format, args...)
}

func (t *Task) setStatus(status string) {
	t.mu.Lock()
	old := t.Status
	t.Status = status
	t.mu.Unlock()
	if old != status {
		t.logf("status %s -> %s", old, status)
	}
}

func (t *Task) addDone(n int64) {
	t.mu.Lock()
	t.Done += n
	if t.Done < 0 {
		t.Done = 0
	}
	if t.Size > 0 && t.Done > t.Size {
		t.Done = t.Size
	}
	t.mu.Unlock()
}

func (t *Task) fail(msg string) {
	t.mu.Lock()
	old := t.Status
	changed := old != "canceled"
	if changed {
		t.Status = "error"
		t.Error = msg
	}
	t.mu.Unlock()
	if changed {
		t.logf("status %s -> error: %s (local=%q remote=%q)", old, msg, t.LocalPath, t.RemotePath)
	}
}

// retryTask logs every failed attempt and respects cancellation while waiting
// for the next attempt. Successful first attempts stay quiet to avoid per-slice
// log noise during healthy transfers.
func retryTask(ctx context.Context, t *Task, attempts int, operation string, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			if i > 0 {
				t.logf("%s recovered on attempt %d/%d", operation, i+1, attempts)
			}
			return nil
		}
		t.logf("%s failed attempt %d/%d: %v", operation, i+1, attempts, err)
		if i >= attempts-1 {
			break
		}
		if err := waitTaskRetry(ctx, t, operation, i+1); err != nil {
			return err
		}
	}
	return err
}

func waitTaskRetry(ctx context.Context, t *Task, operation string, round int) error {
	delay := time.Duration(500*(1<<min(round-1, 5))) * time.Millisecond
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	t.logf("%s retrying after %s", operation, delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type taskList struct {
	mu    sync.Mutex
	tasks map[string]*Task
}

func newTaskList() *taskList {
	return &taskList{tasks: map[string]*Task{}}
}

func (l *taskList) add(t *Task) {
	l.mu.Lock()
	l.tasks[t.ID] = t
	l.mu.Unlock()
}

func (l *taskList) get(id string) *Task {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tasks[id]
}

// remove deletes only terminal tasks. Active tasks must be canceled first so
// their context and any server-side upload session are cleaned up correctly.
func (l *taskList) remove(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.tasks[id]
	if t == nil {
		return false
	}
	t.mu.Lock()
	terminal := t.Status == "done" || t.Status == "error" || t.Status == "canceled"
	t.mu.Unlock()
	if !terminal {
		return false
	}
	delete(l.tasks, id)
	return true
}

// snapshot returns poll-safe copies, newest first, sampling per-task speed
// from the byte delta since the previous poll.
func (l *taskList) snapshot() []TaskView {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]TaskView, 0, len(l.tasks))
	now := time.Now()
	for _, t := range l.tasks {
		t.mu.Lock()
		if !t.lastAt.IsZero() {
			if dt := now.Sub(t.lastAt).Seconds(); dt >= 0.5 {
				inst := int64(float64(t.Done-t.lastDone) / dt)
				t.Speed = (t.Speed + inst) / 2
				t.lastDone, t.lastAt = t.Done, now
			}
		} else {
			t.lastDone, t.lastAt = t.Done, now
		}
		if t.Status != "running" {
			t.Speed = 0
		}
		cp := TaskView{
			ID: t.ID, Kind: t.Kind, Name: t.Name, LocalPath: t.LocalPath,
			RemotePath: t.RemotePath, Size: t.Size, Done: t.Done,
			Status: t.Status, Error: t.Error, Speed: t.Speed, CreatedAt: t.CreatedAt,
		}
		t.mu.Unlock()
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// ---- handlers --------------------------------------------------------------

// handleCreateUpload starts a daemon-side upload of a local file into a
// logical directory of the shared filesystem.
// POST /api/client/transfers/upload {local_path, remote_dir}
func (d *Daemon) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		writeErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	if !d.connected() {
		writeErr(w, http.StatusServiceUnavailable, "客户端尚未连接服务端")
		return
	}
	var in struct {
		LocalPath string `json:"local_path"`
		RemoteDir string `json:"remote_dir"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.LocalPath == "" {
		writeErr(w, http.StatusBadRequest, "请指定本地文件路径")
		return
	}
	fi, err := os.Stat(in.LocalPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "本地文件不存在: "+in.LocalPath)
		return
	}
	if fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "暂不支持上传文件夹，请选择文件")
		return
	}
	if fi.Size() == 0 {
		writeErr(w, http.StatusBadRequest, "暂不支持上传空文件")
		return
	}
	dir := in.RemoteDir
	if dir == "" {
		dir = "/"
	}
	name := fi.Name()
	logical := path.Join("/", dir, name)

	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID: uuid.NewString(), Kind: "upload", Name: name,
		LocalPath: in.LocalPath, RemotePath: logical, Size: fi.Size(),
		Status: "waiting", CreatedAt: time.Now(), auth: auth, cancel: cancel,
	}
	d.tasks.add(t)
	t.logf("created local=%q remote=%q size=%d", t.LocalPath, t.RemotePath, t.Size)
	go d.runUpload(ctx, t)
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID})
}

// handleCreateDownload starts a daemon-side download of a shared file into the
// configured local download directory.
// POST /api/client/transfers/download {file_id, name, path}
func (d *Daemon) handleCreateDownload(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		writeErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	if !d.connected() {
		writeErr(w, http.StatusServiceUnavailable, "客户端尚未连接服务端")
		return
	}
	var in struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.Path == "" {
		writeErr(w, http.StatusBadRequest, "请指定文件")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID: uuid.NewString(), Kind: "download", Name: in.Name,
		RemotePath: in.Path, Status: "waiting", CreatedAt: time.Now(),
		auth: auth, cancel: cancel,
	}
	d.tasks.add(t)
	t.logf("created remote=%q", t.RemotePath)
	go d.runDownload(ctx, t, in.Path)
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID})
}

func (d *Daemon) handleCancelTask(w http.ResponseWriter, id string) {
	t := d.tasks.get(id)
	if t == nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	changed := false
	var done, size int64
	t.set(func(t *Task) {
		if t.Status == "waiting" || t.Status == "hashing" || t.Status == "running" {
			t.Status = "canceled"
			changed = true
		}
		done, size = t.Done, t.Size
	})
	if changed {
		t.logf("canceled by user at %d/%d bytes", done, size)
	}
	if t.cancel != nil {
		t.cancel()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (d *Daemon) handleRemoveTask(w http.ResponseWriter, id string) {
	if !d.tasks.remove(id) {
		writeErr(w, http.StatusConflict, "只能清除已完成、失败或取消的任务")
		return
	}
	log.Printf("[client][task=%s] removed from task list", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// uniquePath returns target, or "name (1).ext" style variants if taken.
func uniquePath(dir, name string) string {
	target := filepath.Join(dir, name)
	_, targetErr := os.Stat(target)
	_, partErr := os.Stat(target + ".gscpart")
	if os.IsNotExist(targetErr) && os.IsNotExist(partErr) {
		return target
	}
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		target = filepath.Join(dir, base+" ("+itoa(i)+")"+ext)
		_, targetErr = os.Stat(target)
		_, partErr = os.Stat(target + ".gscpart")
		if os.IsNotExist(targetErr) && os.IsNotExist(partErr) {
			return target
		}
	}
	return target
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
