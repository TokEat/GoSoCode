// Package clientd is the local CLI client daemon. It ships the same embedded
// web UI as the server and proxies all platform APIs to the server, but the
// transfer data plane runs inside this process: uploads read local files and
// push slices straight to Baidu, downloads fetch from Baidu straight to local
// disk — the browser only watches progress, no file byte passes through it or
// the platform server. The server stays the control plane (accounts, metadata,
// placement, sessions, accounting); the client is inoperable until it is
// pointed at a reachable server (configurable from the web UI).
package clientd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gosocode/internal/baidu"
	controlcrypto "gosocode/internal/crypto"
	"gosocode/internal/webui"
)

type Options struct {
	Server     string // initial server address (flag/env); overrides the saved config
	Window     int    // upload in-flight slice window (this machine -> Baidu)
	DLConns    int    // parallel download connections against Baidu
	WebUI      string // optional on-disk UI dir overriding the embedded one
	ControlKey string // 32-byte hex key encrypting server control-plane packets
}

// Config is what the web UI can change at runtime; persisted next to the user
// profile so it survives restarts.
type Config struct {
	Server      string `json:"server"`
	DownloadDir string `json:"download_dir"`
}

const localDrivesRoot = "::drives"

type localEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

type Daemon struct {
	opts    Options
	api     *http.Client
	baidu   *baidu.Client
	tasks   *taskList
	control *controlcrypto.Codec

	cfgMu   sync.RWMutex
	cfg     Config
	cfgPath string
	proxy   *httputil.ReverseProxy // nil until a server connection is established
}

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".gosocode", "client.json")
}

func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "downloads"
	}
	return filepath.Join(home, "Downloads")
}

// New loads the saved config (flag value wins), and tries to connect to the
// server if an address is known. An unreachable server is not fatal: the web
// UI can (re)configure the address at any time; transfers just refuse to run
// until connected.
func New(opts Options) (*Daemon, error) {
	if opts.Window < 1 {
		opts.Window = 8
	}
	if opts.DLConns < 1 {
		opts.DLConns = 4
	}
	if opts.DLConns > 8 {
		opts.DLConns = 8
	}
	control, err := controlcrypto.New(opts.ControlKey)
	if err != nil {
		return nil, fmt.Errorf("客户端控制面加密密钥无效: %w", err)
	}
	d := &Daemon{
		opts:    opts,
		api:     &http.Client{Timeout: 60 * time.Second},
		baidu:   baidu.New("", ""),
		tasks:   newTaskList(),
		control: control,
		cfgPath: configPath(),
	}
	if raw, err := os.ReadFile(d.cfgPath); err == nil {
		_ = json.Unmarshal(raw, &d.cfg)
	}
	if opts.Server != "" {
		d.cfg.Server = strings.TrimRight(opts.Server, "/")
	}
	if d.cfg.DownloadDir == "" {
		d.cfg.DownloadDir = defaultDownloadDir()
	}
	if d.cfg.Server != "" {
		if err := d.connect(d.cfg.Server); err != nil {
			log.Printf("[client] 服务端 %s 暂不可达，可稍后在网页中重新设置", d.cfg.Server)
		} else {
			log.Printf("[client] 已连接服务端 %s", d.cfg.Server)
			d.saveConfig()
		}
	} else {
		log.Printf("[client] 尚未设置服务端地址，请打开网页完成设置")
	}
	return d, nil
}

// connect validates the address, health-checks it and swaps in a new proxy.
func (d *Daemon) connect(server string) error {
	server = strings.TrimRight(server, "/")
	log.Printf("[client][config] checking server=%s", server)
	u, err := url.Parse(server)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Printf("[client][config] invalid server address %q", server)
		return fmt.Errorf("无效的服务端地址（示例: http://192.168.1.10:8080）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/health", nil)
	resp, err := d.api.Do(req)
	if err != nil {
		log.Printf("[client][config] server health check failed server=%s error=%v", server, err)
		return fmt.Errorf("无法连接服务端: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[client][config] server health check failed server=%s HTTP=%d", server, resp.StatusCode)
		return fmt.Errorf("服务端响应异常: HTTP %d", resp.StatusCode)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[client][proxy] %s %s failed: %v", r.Method, r.URL.RequestURI(), err)
		writeErr(w, http.StatusBadGateway, "无法连接服务端: "+err.Error())
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= http.StatusBadRequest {
			method, uri := "?", "?"
			if resp.Request != nil {
				method, uri = resp.Request.Method, resp.Request.URL.RequestURI()
			}
			log.Printf("[client][proxy] %s %s -> HTTP %d", method, uri, resp.StatusCode)
		}
		return nil
	}
	d.cfgMu.Lock()
	d.cfg.Server = server
	d.proxy = proxy
	d.cfgMu.Unlock()
	log.Printf("[client][config] connected server=%s", server)
	return nil
}

func (d *Daemon) config() Config {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.cfg
}

func (d *Daemon) connected() bool {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.proxy != nil
}

func (d *Daemon) saveConfig() {
	cfg := d.config()
	_ = os.MkdirAll(filepath.Dir(d.cfgPath), 0o755)
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(d.cfgPath, raw, 0o600); err != nil {
		log.Printf("[client] 保存配置失败: %v", err)
	} else {
		log.Printf("[client][config] saved path=%q server=%s download_dir=%q", d.cfgPath, cfg.Server, cfg.DownloadDir)
	}
}

// ---- HTTP surface ---------------------------------------------------------

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/client/", d.handleClientAPI)
	mux.HandleFunc("/api/", d.handleAPI)
	mux.HandleFunc("/", d.handleRoot)
	return mux
}

// handleClientAPI serves the client-local endpoints (never proxied).
func (d *Daemon) handleClientAPI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/client")
	switch {
	// Binary downloads are served by the server (GSC_CLIENT_DIR); proxy them
	// so the download card also works when browsing through the local client.
	case strings.HasPrefix(p, "/download"):
		d.handleAPI(w, r)
	case p == "/info" && r.Method == http.MethodGet:
		d.handleInfo(w, r)
	case p == "/config" && r.Method == http.MethodPut:
		d.handleSetConfig(w, r)
	case p == "/fs" && r.Method == http.MethodGet:
		d.handleFS(w, r)
	case p == "/transfers" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"tasks": d.tasks.snapshot()})
	case p == "/transfers/upload" && r.Method == http.MethodPost:
		d.handleCreateUpload(w, r)
	case p == "/transfers/download" && r.Method == http.MethodPost:
		d.handleCreateDownload(w, r)
	case strings.HasSuffix(p, "/cancel") && strings.HasPrefix(p, "/transfers/") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/transfers/"), "/cancel")
		d.handleCancelTask(w, id)
	case strings.HasPrefix(p, "/transfers/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(p, "/transfers/")
		d.handleRemoveTask(w, id)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// handleInfo answers the mode probe (the server answers mode=server on the
// same path); the UI uses it to adapt and to drive the first-run setup.
func (d *Daemon) handleInfo(w http.ResponseWriter, r *http.Request) {
	cfg := d.config()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": "client", "configured": d.connected(),
		"server": cfg.Server, "download_dir": cfg.DownloadDir,
		"window": d.opts.Window, "dl_conns": d.opts.DLConns,
	})
}

// handleSetConfig lets the web UI point the client at a server (validated with
// a health check before being persisted) and change the download directory.
func (d *Daemon) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server      string `json:"server"`
		DownloadDir string `json:"download_dir"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		log.Printf("[client][config] invalid config request: %v", err)
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if in.Server != "" {
		if err := d.connect(in.Server); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if in.DownloadDir != "" {
		if err := os.MkdirAll(in.DownloadDir, 0o755); err != nil {
			log.Printf("[client][config] cannot create download_dir=%q: %v", in.DownloadDir, err)
			writeErr(w, http.StatusBadRequest, "无法创建下载目录: "+err.Error())
			return
		}
		d.cfgMu.Lock()
		d.cfg.DownloadDir = in.DownloadDir
		d.cfgMu.Unlock()
	}
	d.saveConfig()
	d.handleInfo(w, r)
}

// handleFS lists a local directory so the web UI can offer a file picker for
// uploads (a browser cannot read local paths itself).
func (d *Daemon) handleFS(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	drives := localRoots()
	if p == localDrivesRoot {
		writeJSON(w, http.StatusOK, map[string]any{
			"path": localDrivesRoot, "parent": "", "entries": drives, "drives": drives,
		})
		return
	}
	if p == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p = home
		} else {
			p = "."
		}
	}
	p = filepath.Clean(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		log.Printf("[client][fs] cannot read path=%q: %v", p, err)
		writeErr(w, http.StatusBadRequest, "无法读取目录: "+err.Error())
		return
	}
	out := make([]localEntry, 0, len(entries))
	for _, e := range entries {
		fe := localEntry{Name: e.Name(), Path: filepath.Join(p, e.Name()), Dir: e.IsDir()}
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				fe.Size = fi.Size()
			}
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	parent := filepath.Dir(p)
	if volume := filepath.VolumeName(p); volume != "" {
		root := filepath.Clean(volume + string(os.PathSeparator))
		if strings.EqualFold(filepath.Clean(p), root) {
			parent = localDrivesRoot
		}
	} else if parent == p {
		parent = localDrivesRoot
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": p, "parent": parent, "entries": out, "drives": drives,
	})
}

// handleAPI proxies every other platform API to the server; without a server
// connection the client is inoperable by design.
func (d *Daemon) handleAPI(w http.ResponseWriter, r *http.Request) {
	d.cfgMu.RLock()
	proxy := d.proxy
	d.cfgMu.RUnlock()
	if proxy == nil {
		writeErr(w, http.StatusServiceUnavailable, "客户端尚未连接服务端，请先设置服务端地址")
		return
	}
	proxy.ServeHTTP(w, r)
}

// handleRoot serves the web UI: on-disk override > embedded assets.
func (d *Daemon) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if d.opts.WebUI != "" {
		fp := filepath.Join(d.opts.WebUI, filepath.FromSlash(path.Clean("/"+r.URL.Path)))
		if st, err := os.Stat(fp); err == nil && !st.IsDir() {
			http.ServeFile(w, r, fp)
			return
		}
		http.ServeFile(w, r, filepath.Join(d.opts.WebUI, "index.html"))
		return
	}
	if h := webui.Handler(); h != nil {
		h.ServeHTTP(w, r)
		return
	}
	http.Error(w, "未内嵌 Web UI，请用 -webui 指定前端目录", http.StatusNotFound)
}

// ---- server control-plane helpers -----------------------------------------

// serverJSON performs a control-plane call against the server, forwarding the
// user's Authorization so everything runs as the requesting user.
func (d *Daemon) serverJSON(ctx context.Context, auth, method, p string, body []byte) (int, []byte, error) {
	started := time.Now()
	cfg := d.config()
	if !d.connected() {
		log.Printf("[client][control] %s %s failed: not connected", method, p)
		return 0, nil, fmt.Errorf("尚未连接服务端")
	}
	if d.control == nil {
		return 0, nil, fmt.Errorf("客户端未配置 GSC_AGENT_CONTROL_KEY，已拒绝发送控制面数据")
	}
	target, err := url.Parse(p)
	if err != nil || target.Path == "" {
		return 0, nil, fmt.Errorf("无效的控制面路径")
	}
	encrypted, err := d.control.SealRequest(method, target.Path, controlcrypto.Request{
		Authorization: auth,
		Query:         target.RawQuery,
		Body:          body,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("加密控制请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.Server+target.Path, bytes.NewReader(encrypted))
	if err != nil {
		log.Printf("[client][control] %s %s build request failed: %v", method, p, err)
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(controlcrypto.Header, controlcrypto.Version)
	resp, err := d.api.Do(req)
	if err != nil {
		log.Printf("[client][control] %s %s transport failed after=%s: %v", method, p, time.Since(started).Round(time.Millisecond), err)
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		log.Printf("[client][control] %s %s read response failed HTTP=%d after=%s: %v", method, p, resp.StatusCode, time.Since(started).Round(time.Millisecond), err)
		return resp.StatusCode, nil, err
	}
	if resp.Header.Get(controlcrypto.Header) == controlcrypto.Version {
		raw, err = d.control.OpenResponse(method, target.Path, resp.StatusCode, raw)
		if err != nil {
			return resp.StatusCode, nil, fmt.Errorf("解密控制响应失败: %w", err)
		}
	}
	// Progress reports are one per uploaded slice; keep successful ones quiet.
	// All other control-plane operations, and every failure, remain traceable.
	if resp.StatusCode >= http.StatusBadRequest || !strings.HasSuffix(p, "/api/agent/upload/progress") {
		if resp.StatusCode >= http.StatusBadRequest {
			log.Printf("[client][control] %s %s -> HTTP %d after=%s error=%s", method, p, resp.StatusCode, time.Since(started).Round(time.Millisecond), serverErrMsg(raw, "unknown"))
		} else {
			log.Printf("[client][control] %s %s -> HTTP %d after=%s", method, p, resp.StatusCode, time.Since(started).Round(time.Millisecond))
		}
	}
	return resp.StatusCode, raw, nil
}

// serverErrMsg extracts {"error": ...} from a server response for task errors.
func serverErrMsg(raw []byte, fallback string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
