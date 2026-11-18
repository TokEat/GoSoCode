package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/config"
	"gosocode/internal/coord"
	"gosocode/internal/models"
	"gosocode/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func searchJSONResp(r *http.Request, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Request:    r,
	}
}

func TestMatchQuery(t *testing.T) {
	if !matchQuery("/docs/report.pdf", "rep") {
		t.Fatal("substring should match")
	}
	if !matchQuery("/docs/年度报告.pdf", "报告") {
		t.Fatal("chinese substring should match")
	}
	if !matchQuery("/docs/My Report.pdf", "my report") {
		t.Fatal("multi-token AND should match")
	}
	if matchQuery("/docs/年度报告.pdf", "本地 报告") {
		t.Fatal("missing token should not match")
	}
	if !matchQuery("/photos/vacation", "vac") {
		t.Fatal("directory name should match")
	}
	if matchQuery("/docs/report.pdf", "foo") {
		t.Fatal("unrelated query should not match")
	}
	if matchQuery("/docs/report.pdf", "   ") {
		t.Fatal("blank query should not match")
	}
}

func TestPrimarySearchKey(t *testing.T) {
	if got := primarySearchKey("年 报告"); got != "报告" {
		t.Fatalf("got %q", got)
	}
}

func TestRemoteToLogical(t *testing.T) {
	got := remoteToLogical("/资料盘", "/apps/gosocode", "/apps/gosocode/docs/a.pdf")
	if got != "/资料盘/docs/a.pdf" {
		t.Fatalf("got %q", got)
	}
	if remoteToLogical("/资料盘", "/apps/gosocode", "/apps/other/a.pdf") != "" {
		t.Fatal("outside mount should be empty")
	}
	if remoteToLogical("/资料盘", "/apps/gosocode", "/apps/gosocode") != "/资料盘" {
		t.Fatal("mount root should map to mount")
	}
}

func newSearchServer(t *testing.T) (*Server, *baidu.Client, models.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	client := baidu.New("", "")
	pool := account.NewPool(db, client)
	co, err := coord.New("", 50)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:    &config.Config{JWTSecret: "test-secret"},
		db:     db,
		pool:   pool,
		client: client,
		coord:  co,
	}
	var admin models.User
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	return s, client, admin
}

func stubBaiduSearch(client *baidu.Client, items []map[string]any) {
	client.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return searchJSONResp(r, map[string]any{"errno": 0, "list": items}), nil
	})})
}

func seedSearchFixture(t *testing.T, s *Server) models.BaiduAccount {
	t.Helper()
	acc := models.BaiduAccount{
		Nickname:    "资料盘",
		MountName:   "资料盘",
		MountPath:   "/apps/gosocode",
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(100 * 24 * time.Hour),
		Status:      "active",
	}
	if err := s.db.Create(&acc).Error; err != nil {
		t.Fatal(err)
	}
	files := []models.File{
		{
			BaiduAccountID: acc.ID, LogicalPath: "/资料盘/本地报告.pdf",
			RemotePath: "/apps/gosocode/本地报告.pdf", Size: 100, Status: "ready",
		},
		{
			BaiduAccountID: acc.ID, LogicalPath: "/资料盘/机密合同.pdf",
			RemotePath: "/apps/gosocode/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			RemoteUUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Size: 200, Status: "ready",
		},
	}
	for i := range files {
		if err := s.db.Create(&files[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	return acc
}

func defaultRemoteHits() []map[string]any {
	return []map[string]any{
		{"fs_id": 11, "path": "/apps/gosocode/docs/年度报告.pdf", "server_filename": "年度报告.pdf", "size": 50, "isdir": 0, "server_ctime": 1, "server_mtime": 1},
		{"fs_id": 12, "path": "/apps/gosocode/docs", "server_filename": "docs", "size": 0, "isdir": 1, "server_ctime": 1, "server_mtime": 1},
		{"fs_id": 13, "path": "/apps/gosocode/报告归档", "server_filename": "报告归档", "size": 0, "isdir": 1, "server_ctime": 1, "server_mtime": 1},
		{"fs_id": 14, "path": "/apps/gosocode/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "server_filename": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "size": 200, "isdir": 0},
		{"fs_id": 15, "path": "/apps/gosocode/.gsc-imports/hidden.txt", "server_filename": "hidden.txt", "size": 1, "isdir": 0},
		{"fs_id": 16, "path": "/apps/gosocode/.gsc-manifest.json", "server_filename": ".gsc-manifest.json", "size": 1, "isdir": 0},
		{"fs_id": 17, "path": "/apps/gosocode/本地报告.pdf", "server_filename": "本地报告.pdf", "size": 100, "isdir": 0},
	}
}

func doSearch(t *testing.T, s *Server, uid uint, role, q, pwd string) (int, []map[string]any, string) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	u := "/api/files/search?q=" + url.QueryEscape(q)
	c.Request = httptest.NewRequest(http.MethodGet, u, nil)
	if pwd != "" {
		c.Request.Header.Set("X-Access-Password", pwd)
	}
	c.Set("uid", uid)
	c.Set("role", role)
	c.Set("username", "tester")
	s.handleSearchFiles(c)
	var body struct {
		Files []map[string]any `json:"files"`
		Error string           `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return w.Code, body.Files, body.Error
}

func pathsOf(files []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, f := range files {
		p, _ := f["path"].(string)
		out[p] = f
	}
	return out
}

func TestSearchMergesBaiduLocalAndDirectories(t *testing.T) {
	s, client, admin := newSearchServer(t)
	seedSearchFixture(t, s)
	stubBaiduSearch(client, defaultRemoteHits())

	code, files, errMsg := doSearch(t, s, admin.ID, "admin", "报告", "")
	if code != http.StatusOK || errMsg != "" {
		t.Fatalf("code=%d err=%s", code, errMsg)
	}
	got := pathsOf(files)
	for _, want := range []string{"/资料盘/本地报告.pdf", "/资料盘/docs/年度报告.pdf", "/资料盘/报告归档"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing %s in %#v", want, keys(got))
		}
	}
	if !asBool(got["/资料盘/报告归档"]["is_dir"]) {
		t.Fatal("directory name hit should be a folder")
	}
	if _, ok := got["/资料盘/机密合同.pdf"]; ok {
		t.Fatal("UUID remote name must not match 报告")
	}
	if _, ok := got["/资料盘/.gsc-imports/hidden.txt"]; ok {
		t.Fatal("hidden imports must be omitted")
	}
	if _, ok := got["/资料盘/.gsc-manifest.json"]; ok {
		t.Fatal("manifest must be omitted")
	}

	code, files, _ = doSearch(t, s, admin.ID, "admin", "本地 报告", "")
	got = pathsOf(files)
	if _, ok := got["/资料盘/本地报告.pdf"]; !ok {
		t.Fatal("multi-token should keep 本地报告")
	}
	if _, ok := got["/资料盘/docs/年度报告.pdf"]; ok {
		t.Fatal("multi-token should drop 年度报告")
	}

	code, files, _ = doSearch(t, s, admin.ID, "admin", "合同", "")
	got = pathsOf(files)
	if _, ok := got["/资料盘/机密合同.pdf"]; !ok {
		t.Fatal("logical name should match even when remote is UUID")
	}

	_, files, _ = doSearch(t, s, admin.ID, "admin", "资料", "")
	got = pathsOf(files)
	if m, ok := got["/资料盘"]; !ok || !asBool(m["is_dir"]) || !asBool(m["mount"]) {
		t.Fatalf("mount directory should be searchable, got %#v", files)
	}
}

func TestSearchHidesProtectedDescendants(t *testing.T) {
	s, client, admin := newSearchServer(t)
	acc := seedSearchFixture(t, s)
	if err := s.db.Create(&models.AccessRule{
		Path:        "/资料盘/私密",
		IsDir:       true,
		OwnerUserID: admin.ID + 99,
		OwnerName:   "other",
		PasswordEnc: "unused",
	}).Error; err != nil {
		t.Fatal(err)
	}
	_ = acc
	hits := append(defaultRemoteHits(),
		map[string]any{"fs_id": 21, "path": "/apps/gosocode/私密", "server_filename": "私密", "size": 0, "isdir": 1},
		map[string]any{"fs_id": 22, "path": "/apps/gosocode/私密/秘密报告.pdf", "server_filename": "秘密报告.pdf", "size": 8, "isdir": 0},
	)
	stubBaiduSearch(client, hits)

	_, files, _ := doSearch(t, s, admin.ID, "admin", "报告", "")
	got := pathsOf(files)
	if _, ok := got["/资料盘/私密/秘密报告.pdf"]; ok {
		t.Fatal("must not leak names inside a locked folder")
	}
	_, files, _ = doSearch(t, s, admin.ID, "admin", "私密", "")
	got = pathsOf(files)
	if item, ok := got["/资料盘/私密"]; !ok || !asBool(item["protected"]) {
		t.Fatalf("locked folder itself should appear and be flagged, got %#v", files)
	}
	if _, ok := got["/资料盘/私密/秘密报告.pdf"]; ok {
		t.Fatal("descendant still hidden when searching the folder name")
	}
}

func TestSearchTemporaryGrantFilter(t *testing.T) {
	s, client, _ := newSearchServer(t)
	seedSearchFixture(t, s)
	stubBaiduSearch(client, defaultRemoteHits())

	tmp := models.User{Username: "guest", Role: "temporary", Status: "active"}
	if err := s.db.Create(&tmp).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&models.AccessGrant{UserID: tmp.ID, Path: "/资料盘/docs"}).Error; err != nil {
		t.Fatal(err)
	}

	_, files, _ := doSearch(t, s, tmp.ID, "temporary", "报告", "")
	got := pathsOf(files)
	if _, ok := got["/资料盘/docs/年度报告.pdf"]; !ok {
		t.Fatalf("granted path missing: %#v", keys(got))
	}
	if _, ok := got["/资料盘/本地报告.pdf"]; ok {
		t.Fatal("temporary user must not see outside grants")
	}
	if _, ok := got["/资料盘/报告归档"]; ok {
		t.Fatal("ungranted directory must be hidden")
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	s, _, admin := newSearchServer(t)
	code, files, _ := doSearch(t, s, admin.ID, "admin", "", "")
	if code != http.StatusOK || len(files) != 0 {
		t.Fatalf("code=%d n=%d", code, len(files))
	}
}

func TestSearchSkipsFailedNode(t *testing.T) {
	s, client, admin := newSearchServer(t)
	seedSearchFixture(t, s)
	client.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return searchJSONResp(r, map[string]any{"errno": 2, "errmsg": "fail"}), nil
	})})
	code, files, errMsg := doSearch(t, s, admin.ID, "admin", "报告", "")
	if code != http.StatusOK || errMsg != "" {
		t.Fatalf("failed remote search should not fail the API, code=%d err=%s", code, errMsg)
	}
	got := pathsOf(files)
	if _, ok := got["/资料盘/本地报告.pdf"]; !ok {
		t.Fatal("local metadata hits must still be returned")
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
