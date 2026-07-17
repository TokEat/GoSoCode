package keystore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gosocode/internal/models"
)

func setup(t *testing.T) (*gorm.DB, string, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.BaiduAccount{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.BaiduAccount{Nickname: "n1"}).Error; err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return db, hex.EncodeToString(raw), t.TempDir()
}

func TestNodeKeyDoubleStoreAndRecovery(t *testing.T) {
	db, master, dir := setup(t)
	file := filepath.Join(dir, "node-1.key")

	key, err := NodeKey(db, master, dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("key length %d", len(key))
	}
	// Both stores must be populated.
	var acc models.BaiduAccount
	db.First(&acc, 1)
	if acc.EncKey == "" {
		t.Fatal("DB copy missing")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("file copy missing")
	}

	// Stable across calls.
	again, err := NodeKey(db, master, dir, 1)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatalf("key changed on second lookup: %v", err)
	}

	// DB copy lost -> recovered from the file, and written back to the DB.
	db.Model(&models.BaiduAccount{}).Where("id = ?", 1).Update("enc_key", "")
	rec, err := NodeKey(db, master, dir, 1)
	if err != nil || !bytes.Equal(key, rec) {
		t.Fatalf("file recovery failed: %v", err)
	}
	db.First(&acc, 1)
	if acc.EncKey == "" {
		t.Fatal("DB copy not restored from file")
	}

	// File copy lost -> recovered from the DB, and the file rewritten.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	rec2, err := NodeKey(db, master, dir, 1)
	if err != nil || !bytes.Equal(key, rec2) {
		t.Fatalf("DB recovery failed: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("file copy not restored from DB")
	}

	// A different node gets a different key.
	if err := db.Create(&models.BaiduAccount{Nickname: "n2"}).Error; err != nil {
		t.Fatal(err)
	}
	other, err := NodeKey(db, master, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(key, other) {
		t.Fatal("two nodes share one key")
	}
}
