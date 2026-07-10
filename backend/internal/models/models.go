package models

import "time"

// BaiduAccount is one shared Baidu Netdisk account in the pool. Multiple
// platform users operate through it; the access token is centrally managed.
type BaiduAccount struct {
	ID           uint   `gorm:"primaryKey"`
	Nickname     string `gorm:"size:128"` // name reported by Baidu (or an authorization-time alias)
	MountName    string `gorm:"size:128"` // user-facing mount name; empty means use Nickname
	Remark       string `gorm:"size:512"`
	MountPath    string `gorm:"size:1024;default:/"` // remote directory exposed as this node's root
	AccessToken  string `gorm:"size:512"`
	RefreshToken string `gorm:"size:512"`
	ExpiresAt    time.Time
	QuotaTotal   int64
	QuotaUsed    int64
	CurrentLoad  int    // number of in-flight jobs, used for load balancing
	BytesUp      int64  // bytes uploaded through this node
	BytesDown    int64  // bytes downloaded through this node
	VipType      int    // 0 normal / 1 VIP / 2 SVIP; decides upload slice size (4/16/32MB)
	Status       string `gorm:"size:32;default:active"` // active | disabled | banned
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// User is a platform user. All users share one logical filesystem stored across
// the pooled Baidu accounts (storage nodes).
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"size:64;uniqueIndex"`
	PasswordHash string `gorm:"size:255"`
	Role         string `gorm:"size:16;default:user"`   // admin | user
	Status       string `gorm:"size:16;default:active"` // active | disabled
	BytesUp      int64  // lifetime uploaded bytes (traffic accounting)
	BytesDown    int64  // lifetime downloaded bytes
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// BaiduAppConfig is the singleton (ID=1) holding the platform's Baidu OpenAPI
// application credentials. Managed from the admin UI so no env vars are needed.
type BaiduAppConfig struct {
	ID          uint   `gorm:"primaryKey"`
	AppKey      string `gorm:"size:128"`
	SecretKey   string `gorm:"size:128"`
	SignKey     string `gorm:"size:128"`
	RedirectURI string `gorm:"size:512"`
	UpdatedAt   time.Time
}

// TransferLog records a successfully completed transfer for the user's history
// view (uploads are logged by the worker, downloads by the client on finish).
type TransferLog struct {
	ID     uint   `gorm:"primaryKey"`
	UserID uint   `gorm:"index"`
	Kind   string `gorm:"size:16"` // upload | download
	Name   string `gorm:"size:512"`
	Path   string `gorm:"size:1024"`
	Size   int64
	At     time.Time `gorm:"index"`
}

// AuditLog records notable actions for the ops log view.
type AuditLog struct {
	ID       uint      `gorm:"primaryKey"`
	At       time.Time `gorm:"index"`
	UserID   uint      `gorm:"index"`
	Username string    `gorm:"size:64"`
	Action   string    `gorm:"size:64"`
	Detail   string    `gorm:"size:512"`
	IP       string    `gorm:"size:64"`
}

// File is the authoritative metadata record for the shared filesystem. All
// listing goes through this table, never through Baidu's flat namespace.
type File struct {
	ID             uint   `gorm:"primaryKey"`
	OwnerUserID    uint   `gorm:"index"`           // who uploaded it (accounting/audit only)
	BaiduAccountID uint   `gorm:"index"`           // which pooled account holds it
	LogicalPath    string `gorm:"size:1024;index"` // user-facing path, e.g. /docs/a.pdf
	RemotePath     string `gorm:"size:1024"`       // real path in Baidu sandbox
	FsID           uint64 `gorm:"index"`
	Size           int64
	MD5            string `gorm:"size:64"`
	IsDir          bool   `gorm:"default:false"`             // metadata-only folder marker
	Status         string `gorm:"size:32;default:uploading"` // uploading/ready/deleted
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UploadSession backs a resumable (chunked) browser->server upload. The bytes
// accumulate in a temp file; Offset marks how much has arrived so a paused or
// interrupted upload can continue from where it stopped.
type UploadSession struct {
	ID          string `gorm:"primaryKey;size:36"`
	UserID      uint   `gorm:"index"`
	Name        string `gorm:"size:512"`
	LogicalPath string `gorm:"size:1024"`
	Size        int64
	Offset      int64
	TmpPath     string `gorm:"size:512"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StreamSession backs a pipelined (streaming) upload: browser slices are pushed
// to Baidu (superfile2) as they arrive instead of accumulating in a temp file.
// DoneBitmap is a string of '0'/'1' per slice marking cloud-synced slices, so a
// resumed client only re-sends the missing ones (superfile2 is idempotent).
type StreamSession struct {
	ID             string `gorm:"primaryKey;size:36"`
	UserID         uint   `gorm:"index"`
	BaiduAccountID uint
	FileID         uint
	Name           string `gorm:"size:512"`
	LogicalPath    string `gorm:"size:1024"`
	RemotePath     string `gorm:"size:1024"`
	Size           int64
	SliceSize      int64
	TotalSlices    int
	UploadID       string `gorm:"size:256"`
	BlockList      string // JSON array of per-slice hex md5
	DoneBitmap     string
	DoneCount      int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Job is an async long-running operation (upload/download).
type Job struct {
	ID             string `gorm:"primaryKey;size:36"`
	UserID         uint   `gorm:"index"`
	Type           string `gorm:"size:16"` // upload/download
	FileID         uint
	BaiduAccountID uint
	Status         string `gorm:"size:16;default:pending"` // pending/running/done/failed
	Progress       int    // 0-100
	Error          string `gorm:"size:512"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
