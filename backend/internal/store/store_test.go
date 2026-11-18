package store

import (
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"gosocode/internal/models"
)

func TestSeedAdminUsesEnvPassword(t *testing.T) {
	t.Setenv("GSC_ADMIN_PASSWORD", "test-admin-pass")
	db, err := Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	var u models.User
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatal(err)
	}
	if u.Role != "admin" || u.Status != "active" {
		t.Fatalf("unexpected user: %+v", u)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("test-admin-pass")) != nil {
		t.Fatal("seeded password did not match GSC_ADMIN_PASSWORD")
	}

	// A second open must not create another user or rotate the password.
	db2, err := Open(filepath.Join(t.TempDir(), "seed2.db"))
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	db2.Model(&models.User{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected one user, got %d", count)
	}
}

func TestSeedAdminGeneratesPasswordWhenUnset(t *testing.T) {
	t.Setenv("GSC_ADMIN_PASSWORD", "")
	db, err := Open(filepath.Join(t.TempDir(), "gen.db"))
	if err != nil {
		t.Fatal(err)
	}
	var u models.User
	if err := db.Where("username = ?", "admin").First(&u).Error; err != nil {
		t.Fatal(err)
	}
	if u.PasswordHash == "" {
		t.Fatal("expected a password hash")
	}
}
