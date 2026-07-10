package api

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gosocode/internal/models"
)

func TestResolveMountUsesConfiguredRemoteRoot(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "mount.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.BaiduAccount{}); err != nil {
		t.Fatal(err)
	}
	acc := models.BaiduAccount{
		Nickname: "百度账号", MountName: "团队盘", MountPath: "/团队/归档", Status: "active",
	}
	if err := db.Create(&acc).Error; err != nil {
		t.Fatal(err)
	}

	s := &Server{db: db}
	gotAcc, remote, logical, err := s.resolveMount("/团队盘/文档/报告.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if gotAcc.ID != acc.ID {
		t.Fatalf("account id = %d, want %d", gotAcc.ID, acc.ID)
	}
	if remote != "/团队/归档/文档/报告.pdf" {
		t.Fatalf("remote path = %q", remote)
	}
	if logical != "/团队盘/文档/报告.pdf" {
		t.Fatalf("logical path = %q", logical)
	}
}

func TestNormalizeMountPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
		err   bool
	}{
		{"", "/", false},
		{"资料/项目/", "/资料/项目", false},
		{"\\资料\\项目", "/资料/项目", false},
		{"/资料/../私密", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeMountPath(tt.input)
		if (err != nil) != tt.err {
			t.Fatalf("normalizeMountPath(%q) error = %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("normalizeMountPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveMountRejectsDuplicateNames(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "duplicate.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.BaiduAccount{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Create(&models.BaiduAccount{Nickname: "同名账号", Status: "active"}).Error; err != nil {
			t.Fatal(err)
		}
	}

	_, _, _, err = (&Server{db: db}).resolveMount("/同名账号/文件.txt")
	if err == nil || err.Error() != "挂载名称重复，请由管理员重命名节点" {
		t.Fatalf("unexpected error: %v", err)
	}
}
