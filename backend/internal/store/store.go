package store

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"gosocode/internal/models"
)

// Open initializes the SQLite database and runs migrations. SQLite keeps the
// MVP dependency-free; swap the driver for Postgres in production.
func Open(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(
		&models.BaiduAccount{},
		&models.BaiduAppConfig{},
		&models.User{},
		&models.File{},
		&models.Job{},
		&models.UploadSession{},
		&models.StreamSession{},
		&models.TransferLog{},
		&models.AuditLog{},
		&models.SystemConfig{},
		&models.AccessRule{},
	); err != nil {
		return nil, err
	}
	if err := seedAdmin(db); err != nil {
		return nil, err
	}
	if err := seedSystemConfig(db); err != nil {
		return nil, err
	}
	return db, nil
}

// seedSystemConfig creates the settings singleton with a fresh random master
// key the first time the database is initialized.
func seedSystemConfig(db *gorm.DB) error {
	var count int64
	db.Model(&models.SystemConfig{}).Where("id = ?", 1).Count(&count)
	if count > 0 {
		return nil
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	return db.Create(&models.SystemConfig{
		ID: 1, MasterKey: hex.EncodeToString(raw[:]), LogMaxEntries: 500, UpdatedAt: time.Now(),
	}).Error
}

// seedAdmin creates the default administrator (admin / admin@qwe123) the first
// time the database is empty, so the platform is usable out of the box.
func seedAdmin(db *gorm.DB) error {
	var count int64
	db.Model(&models.User{}).Count(&count)
	if count > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("admin@qwe123"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return db.Create(&models.User{
		Username: "admin", PasswordHash: string(hash), Role: "admin", Status: "active",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error
}
