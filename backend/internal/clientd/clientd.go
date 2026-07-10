// Package clientd is the local CLI client daemon. It binds the same web UI as
// the server (proxying everything to the server by default) but intercepts the
// transfer data plane: upload slices are pushed to Baidu directly from this
// machine and downloads are fetched from Baidu directly (a native client can
// set User-Agent pan.baidu.com and open many connections), so file bytes never
// pass through the platform server. The server stays the control plane:
// accounts, metadata, placement, sessions and traffic accounting.
package clientd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gosocode/internal/baidu"
)

type Options struct {
	Server  string // platform server base URL (required)
	Window  int    // upload in-flight slice window advertised to the browser
	DLConns int    // parallel download connections against Baidu
	WebUI   string // optional local static UI dir; empty = proxy the server's UI
}

type Daemon struct {
	opts  Options
	proxy *httputil.ReverseProxy
	api   *http.Client // control-plane calls to the server
	baidu *baidu.Client

	mu       sync.Mutex
	sessions map[string]*uploadSession
}

// New validates the options and verifies the server is reachable: the client
// cannot operate without the server (it holds accounts and metadata).
func New(opts Options) (*Daemon, error) {
	opts.Server = strings.TrimRight(opts.Server, "/")
	u, err := url.Parse(opts.Server)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("无效的服务端地址 %q（示例: http://192.168.1.10:8080）", opts.Server)
	}
	if opts.Window < 1 {
		opts.Window = 8
	}
	if opts.DLConns < 1 {
		opts.DLConns = 4
	}
	if opts.DLConns > 8 {
		opts.DLConns = 8
	}
	d := &Daemon{
		opts:     opts,
		proxy:    httputil.NewSingleHostReverseProxy(u),
		api:      &http.Client{Timeout: 60 * time.Second},
		baidu:    baidu.New("", ""),
		sessions: map[string]*uploadSession{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, opts.Server+"/api/health", nil)
	resp, err := d.api.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接服务端 %s: %w（客户端必须先连接服务端才能使用）", opts.Server, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("服务端 %s 响应异常: HTTP %d", opts.Server, resp.StatusCode)
	}
	return d, nil
}

var downloadRe = regexp.MustCompile(`^/api/files/(\d+)/download$`)

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/client/info", d.handleInfo)
	mux.HandleFunc("/api/files/stream/plan", d.handlePlan)
	mux.HandleFunc("/api/files/stream/init", d.handleInit)
	mux.HandleFunc("/api/files/stream/slice", d.handleSlice)
	mux.HandleFunc("/api/files/", d.handleFiles)
	mux.HandleFunc("/", d.handleRoot)
	return mux
}

// handleInfo answers the mode probe: the web UI calls the same path on the
// server (which answers mode=server) to auto-detect what it is bound to.
func (d *Daemon) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": "client", "window": d.opts.Window,
		"dl_conns": d.opts.DLConns, "server": d.opts.Server,
	})
}

// handleFiles intercepts only the per-file download route; every other
// /api/files/* call (list, folder ops, stream finish/status/cancel, ...) is
// pure control plane and goes to the server untouched.
func (d *Daemon) handleFiles(w http.ResponseWriter, r *http.Request) {
	if m := downloadRe.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet {
		d.handleDownload(w, r, m[1])
		return
	}
	d.proxy.ServeHTTP(w, r)
}

// handleRoot serves the local web UI if one was provided, otherwise proxies
// (the server may serve the same UI).
func (d *Daemon) handleRoot(w http.ResponseWriter, r *http.Request) {
	if d.opts.WebUI != "" && r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
		fp := filepath.Join(d.opts.WebUI, filepath.FromSlash(path.Clean("/"+r.URL.Path)))
		if st, err := os.Stat(fp); err == nil && !st.IsDir() {
			http.ServeFile(w, r, fp)
			return
		}
		http.ServeFile(w, r, filepath.Join(d.opts.WebUI, "index.html"))
		return
	}
	d.proxy.ServeHTTP(w, r)
}

// serverJSON performs a control-plane call against the server, forwarding the
// browser's Authorization so everything runs as the requesting user.
func (d *Daemon) serverJSON(ctx context.Context, auth, method, p string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.opts.Server+p, rd)
	if err != nil {
		return 0, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.api.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// passthrough relays a server JSON response (usually an error or a fallback
// hint) to the browser unchanged, so error semantics stay identical.
func passthrough(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func retry(attempts int, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(time.Duration(400*(1<<i)) * time.Millisecond)
		}
	}
	return err
}
