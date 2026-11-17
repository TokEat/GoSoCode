package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/config"
	"gosocode/internal/coord"
	"gosocode/internal/crypto"
	"gosocode/internal/models"
	"gosocode/internal/store"
)

func newNotesServer(t *testing.T) (*Server, models.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "notes.db"))
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
		cfg: &config.Config{
			JWTSecret: "test-secret",
			NotesDir:  filepath.Join(dir, "gsc-notes"),
		},
		db:     db,
		pool:   pool,
		client: client,
		coord:  co,
	}
	var admin models.User
	if err := db.Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	return s, admin
}

func notesCtx(s *Server, admin models.User, method, url string, body io.Reader, contentType string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(method, url, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.Request = req
	c.Set("uid", admin.ID)
	c.Set("role", "admin")
	c.Set("username", "admin")
	return c, w
}

func TestNotesPutGetDelete(t *testing.T) {
	s, admin := newNotesServer(t)
	body, _ := json.Marshal(map[string]any{
		"path":   "/资料盘/docs/a.pdf",
		"is_dir": false,
		"body":   "这是一份很长的备注\n\n第二段",
	})
	c, w := notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != http.StatusOK {
		t.Fatalf("put %d %s", w.Code, w.Body.String())
	}

	c, w = notesCtx(s, admin, http.MethodGet, "/api/notes?path="+urlQuery("/资料盘/docs/a.pdf"), nil, "")
	s.handleGetNote(c)
	if w.Code != http.StatusOK {
		t.Fatalf("get %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["exists"] != true || got["body"] != "这是一份很长的备注\n\n第二段" {
		t.Fatalf("got %+v", got)
	}

	c, w = notesCtx(s, admin, http.MethodGet, "/api/notes/index?dir="+urlQuery("/资料盘/docs"), nil, "")
	s.handleIndexNotes(c)
	var idx struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Paths) != 1 || idx.Paths[0] != "/资料盘/docs/a.pdf" {
		t.Fatalf("index %+v", idx.Paths)
	}

	c, w = notesCtx(s, admin, http.MethodDelete, "/api/notes?path="+urlQuery("/资料盘/docs/a.pdf"), nil, "")
	s.handleDeleteNote(c)
	if w.Code != http.StatusOK {
		t.Fatalf("delete %d %s", w.Code, w.Body.String())
	}
	c, w = notesCtx(s, admin, http.MethodGet, "/api/notes?path="+urlQuery("/资料盘/docs/a.pdf"), nil, "")
	s.handleGetNote(c)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["exists"] != false {
		t.Fatalf("expected gone, got %+v", got)
	}
}

func TestNotesEmptyBodyDeletes(t *testing.T) {
	s, admin := newNotesServer(t)
	body, _ := json.Marshal(map[string]any{"path": "/资料盘/readme.md", "body": "hello"})
	c, w := notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"path": "/资料盘/readme.md", "body": "   "})
	c, w = notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var n int64
	s.db.Model(&models.PathNote{}).Count(&n)
	if n != 0 {
		t.Fatalf("expected note removed, count=%d", n)
	}
}

func TestNotesRejectsOversizedBody(t *testing.T) {
	s, admin := newNotesServer(t)
	big := strings.Repeat("啊", maxNoteBodyBytes+8)
	body, _ := json.Marshal(map[string]any{"path": "/资料盘/x", "body": big})
	c, w := notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestNotesImageUploadAndGC(t *testing.T) {
	s, admin := newNotesServer(t)
	pngBytes := tinyPNG(t)
	id, url := uploadNotePNG(t, s, admin, "/资料盘/pic/folder", pngBytes)
	if url != "/api/notes/images/"+itoa(id) && !strings.HasSuffix(url, itoa(id)) {
		t.Fatalf("url %s id %d", url, id)
	}

	md := "封面\n\n![示意图](" + url + ")\n"
	body, _ := json.Marshal(map[string]any{"path": "/资料盘/pic/folder", "is_dir": true, "body": md})
	c, w := notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}

	c, w = notesCtx(s, admin, http.MethodGet, "/api/notes/images/"+itoa(id), nil, "")
	c.Params = gin.Params{{Key: "id", Value: itoa(id)}}
	s.handleGetNoteImage(c)
	if w.Code != 200 {
		t.Fatalf("get image %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type %s", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), pngBytes) {
		t.Fatal("image bytes mismatch")
	}

	// Saving without the image reference garbage-collects the file.
	body, _ = json.Marshal(map[string]any{"path": "/资料盘/pic/folder", "is_dir": true, "body": "只留文字"})
	c, w = notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var count int64
	s.db.Model(&models.NoteImage{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected image GC, count=%d", count)
	}
	ents, _ := os.ReadDir(s.notesDir())
	if len(ents) != 0 {
		t.Fatalf("expected disk cleaned, got %d files", len(ents))
	}
}

func TestNotesRejectsNonImage(t *testing.T) {
	s, admin := newNotesServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("path", "/资料盘/a")
	fw, _ := mw.CreateFormFile("file", "x.txt")
	_, _ = fw.Write([]byte("hello world this is not an image"))
	_ = mw.Close()
	c, w := notesCtx(s, admin, http.MethodPost, "/api/notes/images", &buf, mw.FormDataContentType())
	s.handleUploadNoteImage(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestNotesMoveAndDrop(t *testing.T) {
	s, admin := newNotesServer(t)
	body, _ := json.Marshal(map[string]any{"path": "/资料盘/docs/a.pdf", "body": "file note"})
	c, w := notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"path": "/资料盘/docs/sub", "is_dir": true, "body": "dir note"})
	c, w = notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)

	s.movePathNotes("/资料盘/docs", "/资料盘/归档/docs")

	var paths []string
	s.db.Model(&models.PathNote{}).Order("path").Pluck("path", &paths)
	want := []string{"/资料盘/归档/docs/a.pdf", "/资料盘/归档/docs/sub"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("after move paths=%v", paths)
	}

	s.dropPathNotes("/资料盘/归档/docs", true)
	var n int64
	s.db.Model(&models.PathNote{}).Count(&n)
	if n != 0 {
		t.Fatalf("expected recursive drop, count=%d", n)
	}
}

func TestNotesHiddenInLockedFolder(t *testing.T) {
	s, admin := newNotesServer(t)
	master := s.systemConfig().MasterKey
	if master == "" {
		t.Fatal("expected system master key")
	}
	enc, err := crypto.Wrap(master, "secret-pw")
	if err != nil {
		t.Fatal(err)
	}
	// A different user owns the lock.
	other := models.User{Username: "alice", Role: "user"}
	if err := s.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	rule := models.AccessRule{Path: "/资料盘/secret", IsDir: true, OwnerUserID: other.ID, OwnerName: "alice", PasswordEnc: enc}
	if err := s.db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"path": "/资料盘/secret/a.pdf", "body": "hidden"})
	// Put as owner of the lock (alice) would work; put as admin should be denied without password.
	c, w := notesCtx(s, admin, http.MethodPut, "/api/notes", bytes.NewReader(body), "application/json")
	s.handlePutNote(c)
	if w.Code != http.StatusForbidden {
		t.Fatalf("admin without pwd should be denied, got %d %s", w.Code, w.Body.String())
	}

	// Index of the parent must not leak the locked child's path.
	_ = s.db.Create(&models.PathNote{Path: "/资料盘/open.txt", Body: "ok"}).Error
	_ = s.db.Create(&models.PathNote{Path: "/资料盘/secret/a.pdf", Body: "hidden"}).Error
	c, w = notesCtx(s, admin, http.MethodGet, "/api/notes/index?dir="+urlQuery("/资料盘"), nil, "")
	s.handleIndexNotes(c)
	var idx struct {
		Paths []string `json:"paths"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &idx)
	for _, p := range idx.Paths {
		if strings.HasPrefix(p, "/资料盘/secret") {
			t.Fatalf("locked path leaked in index: %v", idx.Paths)
		}
	}
}

func urlQuery(p string) string {
	return url.QueryEscape(p)
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadNotePNG(t *testing.T, s *Server, admin models.User, path string, pngBytes []byte) (uint, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("path", path)
	fw, err := mw.CreateFormFile("file", "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(pngBytes); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()
	c, w := notesCtx(s, admin, http.MethodPost, "/api/notes/images", &buf, mw.FormDataContentType())
	s.handleUploadNoteImage(c)
	if w.Code != http.StatusOK {
		t.Fatalf("upload %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ID  uint   `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == 0 {
		t.Fatal("missing image id")
	}
	return out.ID, out.URL
}
