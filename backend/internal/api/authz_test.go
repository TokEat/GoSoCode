package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/config"
	"gosocode/internal/coord"
	"gosocode/internal/crypto"
	"gosocode/internal/models"
	"gosocode/internal/store"
)

type authzFixture struct {
	s     *Server
	admin models.User
	user  models.User
	temp  models.User
}

func newAuthzFixture(t *testing.T) *authzFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "authz.db"))
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
			JWTSecret:    "test-secret",
			AgentEnabled: true,
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
	hash, err := bcrypt.GenerateFromPassword([]byte("user-pass-1"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "alice", PasswordHash: string(hash), Role: "user", Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	temp := models.User{Username: "temp", PasswordHash: string(hash), Role: "temporary", Status: "active", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&temp).Error; err != nil {
		t.Fatal(err)
	}
	return &authzFixture{s: s, admin: admin, user: user, temp: temp}
}

func (f *authzFixture) token(id uint) string {
	return f.s.issueToken(id)
}

func (f *authzFixture) do(method, path, token, pwd string, body any) *httptest.ResponseRecorder {
	var rdr *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewBuffer(b)
	} else {
		rdr = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if pwd != "" {
		req.Header.Set("X-Access-Password", pwd)
	}
	w := httptest.NewRecorder()
	f.s.Router().ServeHTTP(w, req)
	return w
}

func TestUnauthorizedAPIRejected(t *testing.T) {
	f := newAuthzFixture(t)
	for _, path := range []string{"/api/files", "/api/notes?path=/x", "/api/jobs", "/api/admin/users", "/api/me"} {
		w := f.do(http.MethodGet, path, "", "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestAdminRoutesForbiddenToUser(t *testing.T) {
	f := newAuthzFixture(t)
	w := f.do(http.MethodGet, "/api/admin/users", f.token(f.user.ID), "", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestParseTokenRejectsNonHMAC(t *testing.T) {
	f := newAuthzFixture(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS384, jwt.MapClaims{
		"sub": float64(f.admin.ID),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString([]byte(f.s.cfg.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.s.parseToken(raw); ok {
		t.Fatal("non-HS256 token should be rejected")
	}
	if _, ok := f.s.parseToken(f.token(f.admin.ID)); !ok {
		t.Fatal("HS256 token should be accepted")
	}
}

func TestAccessPasswordIgnoredInQuery(t *testing.T) {
	f := newAuthzFixture(t)
	master := f.s.systemConfig().MasterKey
	enc, err := crypto.Wrap(master, "folder-secret")
	if err != nil {
		t.Fatal(err)
	}
	rule := models.AccessRule{
		Path: "/box/locked", IsDir: true, OwnerUserID: f.admin.ID, OwnerName: "admin", PasswordEnc: enc,
	}
	if err := f.s.db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}

	// Query parameter must not grant access.
	req := httptest.NewRequest(http.MethodGet, "/api/notes?path=/box/locked/a.txt&pwd=folder-secret", nil)
	req.Header.Set("Authorization", "Bearer "+f.token(f.user.ID))
	w := httptest.NewRecorder()
	f.s.Router().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("query password should be ignored, got %d %s", w.Code, w.Body.String())
	}

	w = f.do(http.MethodGet, "/api/notes?path=/box/locked/a.txt", f.token(f.user.ID), "folder-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("header password should work, got %d %s", w.Code, w.Body.String())
	}
}

func TestTemporaryUserRootListingFiltered(t *testing.T) {
	f := newAuthzFixture(t)
	granted := models.BaiduAccount{Nickname: "granted", MountName: "granted", Status: "active"}
	other := models.BaiduAccount{Nickname: "other", MountName: "other", Status: "active"}
	if err := f.s.db.Create(&granted).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Create(&models.AccessGrant{UserID: f.temp.ID, Path: "/granted/docs", GrantedBy: f.admin.ID, CreatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	w := f.do(http.MethodGet, "/api/files?path=/", f.token(f.temp.ID), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, file := range body.Files {
		paths[file.Path] = true
	}
	if !paths["/granted"] {
		t.Fatalf("expected granted mount, got %#v", paths)
	}
	if paths["/other"] {
		t.Fatalf("ungranted mount leaked: %#v", paths)
	}
}

func TestTemporaryUserCannotProtectOrUseAgent(t *testing.T) {
	f := newAuthzFixture(t)
	if err := f.s.db.Create(&models.AccessGrant{UserID: f.temp.ID, Path: "/granted/docs", GrantedBy: f.admin.ID, CreatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	w := f.do(http.MethodPost, "/api/files/protect", f.token(f.temp.ID), "", map[string]any{
		"path": "/granted/docs", "is_dir": true, "password": "abcd",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("protect: got %d %s", w.Code, w.Body.String())
	}

	w = f.do(http.MethodPost, "/api/agent/handshake", f.token(f.temp.ID), "", map[string]any{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("agent handshake: got %d %s", w.Code, w.Body.String())
	}
}

func TestUploadInitDeniedWithoutAccessPassword(t *testing.T) {
	f := newAuthzFixture(t)
	master := f.s.systemConfig().MasterKey
	enc, err := crypto.Wrap(master, "folder-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Create(&models.AccessRule{
		Path: "/box/locked", IsDir: true, OwnerUserID: f.admin.ID, OwnerName: "admin", PasswordEnc: enc,
	}).Error; err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"name": "a.txt", "path": "/box/locked/a.txt", "size": 1}
	w := f.do(http.MethodPost, "/api/files/upload/init", f.token(f.user.ID), "", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing password: got %d %s", w.Code, w.Body.String())
	}
	w = f.do(http.MethodPost, "/api/files/upload/init", f.token(f.user.ID), "folder-secret", body)
	if w.Code != http.StatusOK {
		t.Fatalf("with password: got %d %s", w.Code, w.Body.String())
	}
}

func TestJobsAndTransfersAreScopedToCaller(t *testing.T) {
	f := newAuthzFixture(t)
	if err := f.s.db.Create(&models.Job{ID: "job-admin", UserID: f.admin.ID, Type: "upload", Status: "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Create(&models.TransferLog{UserID: f.admin.ID, Kind: "download", Name: "secret.bin", Path: "/secret.bin"}).Error; err != nil {
		t.Fatal(err)
	}

	w := f.do(http.MethodGet, "/api/jobs", f.token(f.user.ID), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("jobs %d %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("job-admin")) {
		t.Fatalf("other user's job leaked: %s", w.Body.String())
	}

	w = f.do(http.MethodGet, "/api/transfers/log", f.token(f.user.ID), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("transfers %d %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("secret.bin")) {
		t.Fatalf("other user's transfer leaked: %s", w.Body.String())
	}

	w = f.do(http.MethodPost, "/api/jobs/job-admin/cancel", f.token(f.user.ID), "", nil)
	if w.Code == http.StatusOK {
		t.Fatal("user should not cancel another user's job")
	}
}
