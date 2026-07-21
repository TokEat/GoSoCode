package baidu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Official share endpoints (xpan share): verify extraction code -> sekey,
// list shared files, transfer (转存) into the caller's own netdisk.
const apiShare = "https://pan.baidu.com/rest/2.0/xpan/share"

// shareHeaders are required by the share write endpoints (verify/transfer):
// without a pan.baidu.com Referer Baidu answers errno=2 (参数错误).
var shareHeaders = map[string]string{"Referer": "https://pan.baidu.com/"}

// transferBatch caps fsidlist entries per transfer call.
const transferBatch = 100

var (
	reShareSURL = regexp.MustCompile(`pan\.baidu\.com/s/([A-Za-z0-9_-]+)`)
	reShareInit = regexp.MustCompile(`[?&]surl=([A-Za-z0-9_-]+)`)
	reSharePwd  = regexp.MustCompile(`[?&]pwd=([A-Za-z0-9]{4})`)
	reShareTxt  = regexp.MustCompile(`提取码[:：]?\s*([A-Za-z0-9]{4})`)
)

// ParseShareURL extracts the short-link code (surl, without the leading "1")
// and the extraction code from pasted share text. Accepts a bare link, a
// link?pwd=xxxx form, a share/init?surl= form, or the full "链接: ... 提取码: xxxx"
// text Baidu puts on the clipboard.
func ParseShareURL(text string) (surl, pwd string) {
	if m := reShareSURL.FindStringSubmatch(text); m != nil {
		surl = strings.TrimPrefix(m[1], "1")
	} else if m := reShareInit.FindStringSubmatch(text); m != nil {
		surl = m[1]
	}
	if m := reSharePwd.FindStringSubmatch(text); m != nil {
		pwd = m[1]
	} else if m := reShareTxt.FindStringSubmatch(text); m != nil {
		pwd = m[1]
	}
	return surl, pwd
}

// ShareErrMsg maps share-API errnos to user-facing Chinese messages.
func ShareErrMsg(err error) string {
	ae, ok := err.(*APIError)
	if !ok {
		return err.Error()
	}
	switch ae.Errno {
	case -9:
		return "提取码错误"
	case -62:
		return "尝试次数过多，请稍后再试"
	case 105:
		return "分享链接无效"
	case -7, -21:
		return "分享已过期或文件被禁止分享"
	case -1, -105:
		return "分享链接不存在或已被取消"
	case 12:
		return "部分文件转存失败（可能已存在同名文件）"
	case 4:
		return "网盘节点容量不足"
	case 111:
		return "应用无分享接口权限（需在百度开放平台申请）"
	case 2131:
		return "该分享不存在"
	}
	return fmt.Sprintf("百度接口错误 errno=%d %s", ae.Errno, ae.Errmsg)
}

// ShareVerify checks the extraction code and returns the sekey (randsk) needed
// by ShareList / ShareTransfer. Shares without an extraction code skip this.
func (c *Client) ShareVerify(ctx context.Context, token, surl, pwd string) (string, error) {
	form := url.Values{}
	form.Set("pwd", pwd)

	u := fmt.Sprintf("%s?method=verify&access_token=%s&surl=%s",
		apiShare, url.QueryEscape(token), url.QueryEscape(surl))
	var out struct {
		baseErr
		Randsk string `json:"randsk"`
	}
	if err := c.postFormH(ctx, u, form, shareHeaders, &out); err != nil {
		return "", err
	}
	if err := out.err(); err != nil {
		return "", err
	}
	// randsk arrives percent-encoded inside the JSON; decode it so callers can
	// pass it through normal form/query encoding without double-escaping.
	if v, err := url.QueryUnescape(out.Randsk); err == nil {
		return v, nil
	}
	return out.Randsk, nil
}

type ShareItem struct {
	FsID           uint64
	Path           string
	ServerFilename string
	Size           int64
	IsDir          int
	MD5            string
}

// UnmarshalJSON tolerates the share API returning numeric fields either as
// numbers or as quoted strings ("fs_id":"129660991388659").
func (it *ShareItem) UnmarshalJSON(b []byte) error {
	var raw struct {
		FsID           json.Number `json:"fs_id"`
		Path           string      `json:"path"`
		ServerFilename string      `json:"server_filename"`
		Size           json.Number `json:"size"`
		IsDir          json.Number `json:"isdir"`
		MD5            string      `json:"md5"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	it.Path, it.ServerFilename, it.MD5 = raw.Path, raw.ServerFilename, raw.MD5
	if v, err := raw.FsID.Int64(); err == nil {
		it.FsID = uint64(v)
	}
	if v, err := raw.Size.Int64(); err == nil {
		it.Size = v
	}
	if v, err := raw.IsDir.Int64(); err == nil {
		it.IsDir = int(v)
	}
	return nil
}

type ShareListResult struct {
	ShareID uint64
	UK      uint64
	List    []ShareItem
}

type shareListResp struct {
	baseErr
	// field name differs across doc versions; accept both spellings, and both
	// numeric and quoted values.
	ShareID  json.Number `json:"share_id"`
	ShareID2 json.Number `json:"shareid"`
	UK       json.Number `json:"uk"`
	List     []ShareItem `json:"list"`
}

// ShareList lists the files of a share. dir=="" lists the share root;
// otherwise dir is a path from a previous listing (isdir entries). Pages
// internally until exhausted.
func (c *Client) ShareList(ctx context.Context, token, surl, sekey, dir string) (*ShareListResult, error) {
	const pageSize = 100
	res := &ShareListResult{}
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("method", "list")
		q.Set("access_token", token)
		q.Set("shorturl", surl)
		q.Set("page", strconv.Itoa(page))
		q.Set("num", strconv.Itoa(pageSize))
		if sekey != "" {
			q.Set("sekey", sekey)
		}
		if dir == "" {
			q.Set("root", "1")
		} else {
			q.Set("dir", dir)
		}

		var out shareListResp
		if err := c.getJSON(ctx, apiShare+"?"+q.Encode(), &out); err != nil {
			return nil, err
		}
		if err := out.err(); err != nil {
			return nil, err
		}
		if v, err := out.ShareID.Int64(); err == nil && v != 0 {
			res.ShareID = uint64(v)
		} else if v, err := out.ShareID2.Int64(); err == nil && v != 0 {
			res.ShareID = uint64(v)
		}
		if uk, err := out.UK.Int64(); err == nil && uk != 0 {
			res.UK = uint64(uk)
		}
		res.List = append(res.List, out.List...)
		if len(out.List) < pageSize {
			return res, nil
		}
	}
}

// ShareTransfer copies the given shared fs_ids into destPath in the token
// owner's netdisk (Baidu-side copy, no data transits this machine). limiter
// gates each batch call.
func (c *Client) ShareTransfer(ctx context.Context, limiter Limiter, acctKey, token string, shareID, uk uint64, sekey string, fsids []uint64, destPath string) error {
	for start := 0; start < len(fsids); start += transferBatch {
		end := start + transferBatch
		if end > len(fsids) {
			end = len(fsids)
		}
		if limiter != nil {
			if err := limiter.Wait(ctx, acctKey); err != nil {
				return err
			}
		}
		if err := c.shareTransferBatch(ctx, token, shareID, uk, sekey, fsids[start:end], destPath); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) shareTransferBatch(ctx context.Context, token string, shareID, uk uint64, sekey string, fsids []uint64, destPath string) error {
	ids, _ := json.Marshal(fsids)
	form := url.Values{}
	form.Set("fsidlist", string(ids))
	form.Set("path", destPath)
	form.Set("ondup", "newcopy")
	if sekey != "" {
		form.Set("sekey", sekey)
	}

	u := fmt.Sprintf("%s?method=transfer&access_token=%s&shareid=%d&from=%d",
		apiShare, url.QueryEscape(token), shareID, uk)
	var out struct {
		baseErr
		Extra struct {
			List []struct {
				From  string `json:"from"`
				To    string `json:"to"`
				Errno int    `json:"errno"`
			} `json:"list"`
		} `json:"extra"`
	}
	if err := c.postFormH(ctx, u, form, shareHeaders, &out); err != nil {
		return err
	}
	if err := out.err(); err != nil {
		return err
	}
	for _, it := range out.Extra.List {
		if it.Errno != 0 {
			return &APIError{Errno: it.Errno, Errmsg: fmt.Sprintf("转存 %s 失败", it.From)}
		}
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	for k, v := range shareHeaders {
		req.Header.Set(k, v)
	}
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
