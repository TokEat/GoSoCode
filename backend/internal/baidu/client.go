// Package baidu wraps the Baidu Netdisk OpenAPI (pan.baidu.com/union).
//
// Upload is a 3-step flow: precreate -> superfile2 (chunked) -> create.
// Download is: filemetas (get dlink) -> GET dlink with UA pan.baidu.com.
package baidu

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
)

const (
	// ChunkSize is the slice size for chunked upload. 4MB is the limit for
	// normal (non-VIP) Baidu users; raise to 16/32MB for VIP accounts.
	ChunkSize = 4 * 1024 * 1024

	apiFile  = "https://pan.baidu.com/rest/2.0/xpan/file"
	apiMulti = "https://pan.baidu.com/rest/2.0/xpan/multimedia"
	apiNas   = "https://pan.baidu.com/rest/2.0/xpan/nas"
	// Default PCS upload server; LocateUpload can return closer ones.
	pcsDefaultServer = "https://d.pcs.baidu.com"
	apiToken         = "https://openapi.baidu.com/oauth/2.0/token"
	apiAuth          = "https://openapi.baidu.com/oauth/2.0/authorize"
	apiQuota         = "https://pan.baidu.com/api/quota"
	// UserAgent required when fetching a dlink, otherwise Baidu returns 403.
	UserAgent = "pan.baidu.com"
)

type Client struct {
	http *http.Client

	// Credentials are mutable at runtime: the admin sets them from the UI and
	// they are persisted in the DB, so no env vars are required. Guarded by mu
	// because token refresh can run concurrently with a config update.
	mu     sync.RWMutex
	appKey string
	secret string
}

func New(appKey, secret string) *Client {
	return &Client{
		// dlink issues a 302 we follow ourselves for streaming; default client
		// follows redirects for the JSON endpoints which is fine.
		http:   &http.Client{Timeout: 0},
		appKey: appKey,
		secret: secret,
	}
}

// SetCreds updates the OAuth app credentials at runtime (called on startup and
// whenever the admin saves the Baidu config).
func (c *Client) SetCreds(appKey, secret string) {
	c.mu.Lock()
	c.appKey, c.secret = appKey, secret
	c.mu.Unlock()
}

// Configured reports whether an AppKey has been set.
func (c *Client) Configured() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.appKey != ""
}

func (c *Client) creds() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.appKey, c.secret
}

// baseErr is the common errno envelope Baidu returns.
type baseErr struct {
	Errno  int    `json:"errno"`
	Errmsg string `json:"errmsg"`
}

// APIError preserves the numeric Baidu errno so callers can log and classify
// ambiguous/transient API failures without parsing an error string.
type APIError struct {
	Errno  int
	Errmsg string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("baidu api errno=%d msg=%s", e.Errno, e.Errmsg)
}

func (b baseErr) err() error {
	if b.Errno != 0 {
		return &APIError{Errno: b.Errno, Errmsg: b.Errmsg}
	}
	return nil
}

// ---- OAuth token refresh -------------------------------------------------

type TokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// AuthCodeURL builds the OAuth authorization URL the user visits to grant the
// platform access to their netdisk. After consent Baidu redirects to
// redirectURI with a ?code= that ExchangeCode turns into tokens.
func (c *Client) AuthCodeURL(redirectURI, state string) string {
	appKey, _ := c.creds()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", appKey)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "basic,netdisk")
	q.Set("state", state)
	return apiAuth + "?" + q.Encode()
}

// ExchangeCode swaps an authorization code for access/refresh tokens.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (*TokenResp, error) {
	appKey, secret := c.creds()
	q := url.Values{}
	q.Set("grant_type", "authorization_code")
	q.Set("code", code)
	q.Set("client_id", appKey)
	q.Set("client_secret", secret)
	q.Set("redirect_uri", redirectURI)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiToken+"?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr TokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("code exchange failed: %s %s", tr.Error, tr.ErrorDesc)
	}
	return &tr, nil
}

// Quota reports a node's storage capacity, used for capacity-aware placement.
type Quota struct {
	baseErr
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Free  int64 `json:"free"`
}

func (c *Client) GetQuota(ctx context.Context, token string) (*Quota, error) {
	q := url.Values{}
	q.Set("access_token", token)
	q.Set("checkfree", "1")
	q.Set("checkexpire", "1")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiQuota+"?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out Quota
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Free == 0 && out.Total > 0 {
		out.Free = out.Total - out.Used
	}
	return &out, out.err()
}

// SliceSizeForVip maps a Baidu account tier to its maximum superfile2 slice
// size: normal 4MB, VIP 16MB, SVIP 32MB. Bigger slices mean fewer requests.
func SliceSizeForVip(vipType int) int64 {
	switch {
	case vipType >= 2:
		return 32 << 20
	case vipType == 1:
		return 16 << 20
	default:
		return ChunkSize
	}
}

// UserInfo describes the netdisk account, notably its VIP tier.
type UserInfo struct {
	baseErr
	BaiduName   string `json:"baidu_name"`
	NetdiskName string `json:"netdisk_name"`
	VipType     int    `json:"vip_type"` // 0 normal / 1 VIP / 2 SVIP
}

func (c *Client) GetUserInfo(ctx context.Context, token string) (*UserInfo, error) {
	q := url.Values{}
	q.Set("method", "uinfo")
	q.Set("access_token", token)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiNas+"?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, out.err()
}

// RefreshToken exchanges a refresh_token for a fresh access_token. Access
// tokens live 30 days; refresh proactively before expiry.
func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (*TokenResp, error) {
	appKey, secret := c.creds()
	q := url.Values{}
	q.Set("grant_type", "refresh_token")
	q.Set("refresh_token", refreshToken)
	q.Set("client_id", appKey)
	q.Set("client_secret", secret)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiToken+"?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr TokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("token refresh failed: %s %s", tr.Error, tr.ErrorDesc)
	}
	return &tr, nil
}

// ---- Upload: precreate -> superfile2 -> create ---------------------------

type precreateResp struct {
	baseErr
	UploadID   string `json:"uploadid"`
	ReturnType int    `json:"return_type"` // 2 => rapid-upload hit (file already created)
	BlockList  []int  `json:"block_list"`  // indices of chunks that still need upload
}

// Precreate registers the upload and detects rapid-upload (秒传). blockMD5s are
// the hex md5 of each chunk in order.
func (c *Client) Precreate(ctx context.Context, token, remotePath string, size int64, blockMD5s []string) (*precreateResp, error) {
	blocks, _ := json.Marshal(blockMD5s)
	form := url.Values{}
	form.Set("path", remotePath)
	form.Set("size", strconv.FormatInt(size, 10))
	form.Set("isdir", "0")
	form.Set("autoinit", "1")
	form.Set("rtype", "3") // overwrite if name+path collide
	form.Set("block_list", string(blocks))

	u := fmt.Sprintf("%s?method=precreate&access_token=%s", apiFile, url.QueryEscape(token))
	var out precreateResp
	if err := c.postForm(ctx, u, form, &out); err != nil {
		return nil, err
	}
	return &out, out.err()
}

// LocateUpload returns upload server base URLs (e.g. "https://xx.baidupcs.com")
// close to this machine. The default d.pcs.baidu.com intermittently answers
// empty-body HTTP 500s; rotating across located servers works around that.
func (c *Client) LocateUpload(ctx context.Context, token, remotePath, uploadID string) ([]string, error) {
	q := url.Values{}
	q.Set("method", "locateupload")
	q.Set("appid", "250528")
	q.Set("access_token", token)
	q.Set("path", remotePath)
	q.Set("uploadid", uploadID)
	q.Set("upload_version", "2.0")

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pcsDefaultServer+"/rest/2.0/pcs/file?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		ErrorCode int    `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Servers   []struct {
			Server string `json:"server"`
		} `json:"servers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out.ErrorCode != 0 {
		return nil, fmt.Errorf("locateupload error %d: %s", out.ErrorCode, out.ErrorMsg)
	}
	var servers []string
	for _, s := range out.Servers {
		if strings.HasPrefix(s.Server, "https://") {
			servers = append(servers, strings.TrimRight(s.Server, "/"))
		}
	}
	return servers, nil
}

// UploadChunk sends one slice to superfile2. partseq is the 0-based index.
func (c *Client) UploadChunk(ctx context.Context, token, remotePath, uploadID string, partseq int, chunk []byte) error {
	return c.UploadChunkTo(ctx, "", token, remotePath, uploadID, partseq, chunk)
}

// UploadChunkTo is UploadChunk against a specific upload server base URL from
// LocateUpload; empty means the default server.
func (c *Client) UploadChunkTo(ctx context.Context, server, token, remotePath, uploadID string, partseq int, chunk []byte) error {
	if server == "" {
		server = pcsDefaultServer
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "chunk")
	if err != nil {
		return err
	}
	if _, err := fw.Write(chunk); err != nil {
		return err
	}
	mw.Close()

	q := url.Values{}
	q.Set("method", "upload")
	q.Set("access_token", token)
	q.Set("type", "tmpfile")
	q.Set("path", remotePath)
	q.Set("uploadid", uploadID)
	q.Set("partseq", strconv.Itoa(partseq))

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server+"/rest/2.0/pcs/superfile2?"+q.Encode(), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var be baseErr
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("baidu upload HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &be); err != nil {
		return fmt.Errorf("invalid baidu upload response: %w", err)
	}
	// superfile2 returns {md5:...} on success, errno on failure.
	return be.err()
}

type createResp struct {
	baseErr
	FsID uint64 `json:"fs_id"`
	Size int64  `json:"size"`
}

// Create finalizes the upload by merging uploaded chunks into a file.
func (c *Client) Create(ctx context.Context, token, remotePath string, size int64, uploadID string, blockMD5s []string) (*createResp, error) {
	blocks, _ := json.Marshal(blockMD5s)
	form := url.Values{}
	form.Set("path", remotePath)
	form.Set("size", strconv.FormatInt(size, 10))
	form.Set("isdir", "0")
	form.Set("uploadid", uploadID)
	form.Set("rtype", "3")
	form.Set("block_list", string(blocks))

	u := fmt.Sprintf("%s?method=create&access_token=%s", apiFile, url.QueryEscape(token))
	var out createResp
	if err := c.postForm(ctx, u, form, &out); err != nil {
		return nil, err
	}
	return &out, out.err()
}

func (c *Client) MakeDir(ctx context.Context, token, remotePath string) error {
	form := url.Values{}
	form.Set("path", remotePath)
	form.Set("isdir", "1")
	form.Set("rtype", "3")

	u := fmt.Sprintf("%s?method=create&access_token=%s", apiFile, url.QueryEscape(token))
	var out baseErr
	if err := c.postForm(ctx, u, form, &out); err != nil {
		return err
	}
	return out.err()
}

// Delete removes files by their remote paths via the filemanager API.
func (c *Client) Delete(ctx context.Context, token string, remotePaths []string) error {
	list, _ := json.Marshal(remotePaths)
	form := url.Values{}
	form.Set("async", "0")
	form.Set("filelist", string(list))

	u := fmt.Sprintf("%s?method=filemanager&opera=delete&access_token=%s", apiFile, url.QueryEscape(token))
	var out baseErr
	if err := c.postForm(ctx, u, form, &out); err != nil {
		return err
	}
	return out.err()
}

func (c *Client) Move(ctx context.Context, token, srcPath, dstPath string) error {
	item := []map[string]string{{
		"path":    srcPath,
		"dest":    path.Dir(dstPath),
		"newname": path.Base(dstPath),
	}}
	list, _ := json.Marshal(item)
	form := url.Values{}
	form.Set("async", "0")
	form.Set("filelist", string(list))

	u := fmt.Sprintf("%s?method=filemanager&opera=move&access_token=%s", apiFile, url.QueryEscape(token))
	var out baseErr
	if err := c.postForm(ctx, u, form, &out); err != nil {
		return err
	}
	return out.err()
}

// ---- Directory listing -----------------------------------------------------

type ListItem struct {
	FsID           uint64 `json:"fs_id"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Size           int64  `json:"size"`
	IsDir          int    `json:"isdir"`
	ServerMTime    int64  `json:"server_mtime"`
	ServerCTime    int64  `json:"server_ctime"`
}

type listResp struct {
	baseErr
	List []ListItem `json:"list"`
}

func (c *Client) ListDir(ctx context.Context, token, remoteDir string) ([]ListItem, error) {
	q := url.Values{}
	q.Set("method", "list")
	q.Set("access_token", token)
	q.Set("dir", remoteDir)
	q.Set("order", "name")
	q.Set("desc", "0")
	q.Set("folder", "0")
	q.Set("web", "1")

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiFile+"?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out listResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if err := out.err(); err != nil {
		return nil, err
	}
	return out.List, nil
}

// ---- Download: filemetas -> dlink ---------------------------------------

type FileMeta struct {
	FsID     uint64 `json:"fs_id"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Dlink    string `json:"dlink"`
}

type filemetasResp struct {
	baseErr
	List []FileMeta `json:"list"`
}

// FileMetas resolves fs_ids to metadata including the temporary dlink
// (valid ~8h). token is appended by the caller when fetching the dlink.
func (c *Client) FileMetas(ctx context.Context, token string, fsIDs []uint64) ([]FileMeta, error) {
	ids, _ := json.Marshal(fsIDs)
	q := url.Values{}
	q.Set("method", "filemetas")
	q.Set("access_token", token)
	q.Set("fsids", string(ids))
	q.Set("dlink", "1")

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiMulti+"?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out filemetasResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if err := out.err(); err != nil {
		return nil, err
	}
	return out.List, nil
}

// OpenDownload streams a file's content from its dlink. It forwards rangeHeader
// (may be empty) to support resumable/multi-threaded downloads. The caller owns
// closing the returned body.
func (c *Client) OpenDownload(ctx context.Context, token, dlink, rangeHeader string) (*http.Response, error) {
	sep := "?"
	if strings.Contains(dlink, "?") {
		sep = "&"
	}
	u := dlink + sep + "access_token=" + url.QueryEscape(token)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", UserAgent) // mandatory for dlink
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return c.http.Do(req)
}

// ---- helpers -------------------------------------------------------------

func (c *Client) postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("baidu API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// SliceFileMD5s streams a file in sliceSize blocks and returns the hex md5 of
// each block plus the total size. Only one block is held in memory at a time,
// so it is safe for large files (unlike loading the whole file).
func SliceFileMD5s(path string, sliceSize int64) (blocks []string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if sliceSize <= 0 {
		sliceSize = ChunkSize
	}
	buf := make([]byte, sliceSize)
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			sum := md5.Sum(buf[:n])
			blocks = append(blocks, hex.EncodeToString(sum[:]))
			size += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, 0, rerr
		}
	}
	if len(blocks) == 0 { // empty file still needs one (empty) block
		sum := md5.Sum(nil)
		blocks = append(blocks, hex.EncodeToString(sum[:]))
	}
	return blocks, size, nil
}
