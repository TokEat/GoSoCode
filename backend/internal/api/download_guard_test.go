package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/config"
	"gosocode/internal/coord"
	cryptoutil "gosocode/internal/crypto"
	"gosocode/internal/metrics"
	"gosocode/internal/models"
	"gosocode/internal/store"
)

func newDownloadServer(t *testing.T) (*Server, *baidu.Client, models.User, models.BaiduAccount) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "dl.db"))
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
		cfg:    &config.Config{JWTSecret: "test-secret", AgentEnabled: true},
		db:     db,
		pool:   pool,
		client: client,
		coord:  co,
		meter:  metrics.New(),
	}
	var admin models.User
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	acc := models.BaiduAccount{
		Nickname:    "资料盘",
		MountName:   "资料盘",
		MountPath:   "/apps/gosocode",
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(100 * 24 * time.Hour),
		Status:      "active",
	}
	if err := db.Create(&acc).Error; err != nil {
		t.Fatal(err)
	}
	return s, client, admin, acc
}

type downloadStub struct {
	body        []byte
	opens       atomic.Int32
	filemetas   atomic.Int32
	status      int
	contentType string
}

func (s *downloadStub) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL
	method := u.Query().Get("method")
	jsonOK := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(b)),
			Request:    r,
		}
	}
	if method == "filemetas" || strings.Contains(u.Path, "multimedia") && method == "filemetas" || method == "filemetas" {
		s.filemetas.Add(1)
		return jsonOK(map[string]any{"errno": 0, "list": []map[string]any{{
			"fs_id": 42, "filename": "doc.txt", "size": len(s.body),
			"dlink": "https://dlink.test/file",
		}}}), nil
	}
	if strings.Contains(u.Host, "dlink.test") {
		s.opens.Add(1)
		st := s.status
		if st == 0 {
			st = http.StatusOK
		}
		h := http.Header{"Content-Type": []string{"application/octet-stream"}}
		h.Set("Content-Length", strconv.Itoa(len(s.body)))
		return &http.Response{
			StatusCode: st,
			Header:     h,
			Body:       io.NopCloser(bytes.NewReader(s.body)),
			Request:    r,
		}, nil
	}
	if method == "list" {
		return jsonOK(map[string]any{"errno": 0, "list": []map[string]any{{
			"fs_id": 42, "path": "/apps/gosocode/doc.txt", "server_filename": "doc.txt",
			"size": len(s.body), "isdir": 0,
		}}}), nil
	}
	return jsonOK(map[string]any{"errno": 0}), nil
}

func doDownload(t *testing.T, s *Server, admin models.User, fileID uint) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(fileID), 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/files/"+strconv.FormatUint(uint64(fileID), 10)+"/download", nil)
	c.Set("uid", admin.ID)
	c.Set("role", "admin")
	c.Set("username", "admin")
	s.handleDownload(c)
	return w
}

func seedFile(t *testing.T, s *Server, acc models.BaiduAccount, name string, enc bool, key, iv string) models.File {
	t.Helper()
	f := models.File{
		BaiduAccountID: acc.ID,
		LogicalPath:    "/资料盘/" + name,
		RemotePath:     "/apps/gosocode/" + name,
		FsID:           42,
		Size:           32,
		Status:         "ready",
		Encrypted:      enc,
		EncKey:         key,
		EncIV:          iv,
	}
	if err := s.db.Create(&f).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDownloadFailsFastWhenEncryptedKeysMissing(t *testing.T) {
	s, client, admin, acc := newDownloadServer(t)
	f := seedFile(t, s, acc, "secret.bin", true, "", "")
	stub := &downloadStub{body: bytes.Repeat([]byte("X"), 512)}
	client.SetHTTPClient(&http.Client{Transport: stub})

	w := doDownload(t, s, admin, f.ID)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "缺少可用的解密密钥") {
		t.Fatalf("error message: %s", w.Body.String())
	}
	if stub.opens.Load() != 0 || stub.filemetas.Load() != 0 {
		t.Fatalf("must not contact Baidu before failing, opens=%d metas=%d", stub.opens.Load(), stub.filemetas.Load())
	}
}

func TestDownloadFailsFastWhenKeysCannotUnwrap(t *testing.T) {
	s, client, admin, acc := newDownloadServer(t)
	// Wrapped with a different master key, so Unwrap with the DB key fails.
	other, _ := cryptoutil.Wrap(strings.Repeat("ab", 32), strings.Repeat("00", 32))
	otherIV, _ := cryptoutil.Wrap(strings.Repeat("ab", 32), strings.Repeat("11", 16))
	f := seedFile(t, s, acc, "secret.bin", true, other, otherIV)
	stub := &downloadStub{body: bytes.Repeat([]byte("X"), 512)}
	client.SetHTTPClient(&http.Client{Transport: stub})

	w := doDownload(t, s, admin, f.ID)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if stub.opens.Load() != 0 {
		t.Fatal("must not start the upstream download")
	}
}

func TestDownloadRejectsCiphertextLookingContent(t *testing.T) {
	s, client, admin, acc := newDownloadServer(t)
	body := make([]byte, 512)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	f := seedFile(t, s, acc, "maybe.bin", false, "", "")
	f.Size = int64(len(body))
	s.db.Model(&f).Update("size", f.Size)
	stub := &downloadStub{body: body}
	client.SetHTTPClient(&http.Client{Transport: stub})

	w := doDownload(t, s, admin, f.ID)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "疑似加密密文") {
		t.Fatalf("error message: %s", w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), body[:32]) {
		t.Fatal("ciphertext bytes must not be delivered to the client")
	}
	if stub.opens.Load() == 0 {
		t.Fatal("expected a short peek of the upstream body")
	}
}

func TestDownloadPlaintextStillWorks(t *testing.T) {
	s, client, admin, acc := newDownloadServer(t)
	plain := []byte("hello, this is a normal plaintext file for download tests\n")
	f := seedFile(t, s, acc, "readme.txt", false, "", "")
	s.db.Model(&f).Update("size", int64(len(plain)))
	stub := &downloadStub{body: plain}
	client.SetHTTPClient(&http.Client{Transport: stub})

	w := doDownload(t, s, admin, f.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatalf("got %q", w.Body.Bytes())
	}
}

func TestDownloadDecryptsEncryptedHappyPath(t *testing.T) {
	s, client, admin, acc := newDownloadServer(t)
	plain := []byte("secret message that must come back as plaintext after CTR decrypt\n")
	key := make([]byte, 32)
	iv := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	cipher := append([]byte(nil), plain...)
	if err := cryptoutil.XORAt(cipher, key, iv, 0); err != nil {
		t.Fatal(err)
	}

	var sc models.SystemConfig
	if err := s.db.First(&sc, 1).Error; err != nil {
		t.Fatal(err)
	}
	wk, err := cryptoutil.Wrap(sc.MasterKey, hex.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	wiv, err := cryptoutil.Wrap(sc.MasterKey, hex.EncodeToString(iv))
	if err != nil {
		t.Fatal(err)
	}
	f := seedFile(t, s, acc, "secret.txt", true, wk, wiv)
	s.db.Model(&f).Update("size", int64(len(plain)))
	stub := &downloadStub{body: cipher}
	client.SetHTTPClient(&http.Client{Transport: stub})

	w := doDownload(t, s, admin, f.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatalf("got %q want %q", w.Body.Bytes(), plain)
	}
}

func TestAgentResolveFailsFastWhenEncryptedKeysMissing(t *testing.T) {
	s, client, admin, acc := newDownloadServer(t)
	f := seedFile(t, s, acc, "secret.bin", true, "not-valid-wrapped-key", "not-valid-wrapped-iv")
	stub := &downloadStub{body: bytes.Repeat([]byte("X"), 64)}
	client.SetHTTPClient(&http.Client{Transport: stub})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/agent/download/resolve?id="+strconv.FormatUint(uint64(f.ID), 10), nil)
	c.Set("uid", admin.ID)
	c.Set("role", "admin")
	c.Set("username", "admin")
	s.handleAgentDownloadResolve(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "缺少可用的解密密钥") {
		t.Fatalf("error message: %s", w.Body.String())
	}
	if stub.filemetas.Load() != 0 || stub.opens.Load() != 0 {
		t.Fatalf("must not resolve a dlink when keys are missing, metas=%d opens=%d", stub.filemetas.Load(), stub.opens.Load())
	}
}
