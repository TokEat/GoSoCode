package models

import "time"

// ImportsDirName is the hidden per-node staging directory (directly under the
// mount root) that share-link imports (转存) land in. It is filtered from
// listings; imported files stay here and are exposed via LogicalPath only.
const ImportsDirName = ".gsc-imports"

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
	EncKey       string `gorm:"size:512"`               // node encryption key, wrapped with the master key (backup copy lives in KeysDir)
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
	// explicit column name: gorm's initialism handling would otherwise map
	// OIDCSubject to o_id_c_subject and break raw `oidc_subject = ?` lookups
	OIDCSubject  string `gorm:"column:oidc_subject;size:128;index"` // OIDC `sub` claim; non-empty = account provisioned via SSO
	Role         string `gorm:"size:16;default:user"`   // admin | user | temporary
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

// SystemConfig is the singleton (ID=1) holding platform-wide settings managed
// from the admin UI. MasterKey wraps per-file encryption keys and stored access
// passwords; it is generated once and never leaves the server.
type SystemConfig struct {
	ID             uint   `gorm:"primaryKey"`
	EncryptStorage bool   `gorm:"default:false"` // new uploads are AES-encrypted at rest
	// explicit column name: gorm's initialism handling would otherwise map
	// UUIDStorage to u_u_i_d_storage
	UUIDStorage   bool   `gorm:"column:uuid_storage;default:false"` // new uploads/dirs get UUID names on the netdisk
	MasterKey     string `gorm:"size:128"`                          // hex, 32 bytes
	LogMaxEntries int    `gorm:"default:500"`                       // audit log retention cap

	ShareImportEnabled bool `gorm:"default:true"` // allow importing external Baidu share links (转存)

	// Unified OIDC (e.g. Keycloak) single sign-on. When enabled, the login
	// page offers SSO alongside local password login; both methods coexist.
	OIDCEnabled      bool   `gorm:"default:false"`
	OIDCIssuer       string `gorm:"size:512"` // e.g. https://kc.example.com/realms/myrealm
	OIDCClientID     string `gorm:"size:256"`
	OIDCClientSecret string `gorm:"size:512"`
	OIDCRedirectURL  string `gorm:"size:512"` // optional override; default derives from the request host

	UpdatedAt time.Time
}

// AccessRule protects a logical path (file or directory) with a password. Only
// the rule owner accesses freely; everyone else — admins included — must
// present the password. Owner and admins may view/manage the password itself.
type AccessRule struct {
	ID          uint   `gorm:"primaryKey"`
	Path        string `gorm:"size:1024;uniqueIndex"` // logical path
	IsDir       bool   `gorm:"default:false"`
	OwnerUserID uint   `gorm:"index"`
	OwnerName   string `gorm:"size:64"`
	PasswordEnc string `gorm:"size:512"` // wrapped with SystemConfig.MasterKey
	CreatedAt   time.Time
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

// TransferEvent records fine-grained per-stage transfer events for uploads,
// downloads and imports. It is best-effort and must not affect the main flow.
type TransferEvent struct {
	ID          uint      `gorm:"primaryKey"`
	UserID      uint      `gorm:"index"`
	Kind        string    `gorm:"size:16"`   // upload | download | import
	Stage       string    `gorm:"size:32"`   // init | start | chunk | slice | progress | finish | cancel | failure
	RefType     string    `gorm:"size:32"`   // upload_session | stream | job | file | agent | share | client
	RefID       string    `gorm:"size:128"`
	Name        string    `gorm:"size:512"`
	Path        string    `gorm:"size:1024"`
	AccountID   uint      `gorm:"index"`
	OffsetStart int64
	OffsetEnd   int64
	Bytes       int64
	Error       string    `gorm:"size:512"`
	At          time.Time `gorm:"index"`
}

// AccessGrant authorizes a temporary user to access a specific logical path
// or any descendant of it. Non-temporary users are never restricted by grants.
type AccessGrant struct {
	ID        uint       `gorm:"primaryKey"`
	UserID    uint       `gorm:"index;uniqueIndex:idx_grant_user_path"`
	Path      string     `gorm:"size:1024;index;uniqueIndex:idx_grant_user_path"`
	GrantedBy uint
	CreatedAt time.Time
	ExpiresAt *time.Time
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
	// non-empty = the remote name is this UUID (obfuscated storage); display
	// name always comes from LogicalPath
	RemoteUUID string `gorm:"column:remote_uuid;size:64;index"`
	FsID       uint64 `gorm:"index"`
	Size           int64
	MD5            string `gorm:"size:64"`
	Encrypted      bool   `gorm:"default:false"`             // stored bytes are AES-256-CTR ciphertext
	EncKey         string `gorm:"size:512"`                  // per-file key, wrapped with the master key
	EncIV          string `gorm:"size:512"`                  // per-file CTR IV, wrapped with the master key
	IsDir          bool   `gorm:"default:false"`             // metadata-only folder marker
	Status         string `gorm:"size:32;default:uploading"` // uploading/ready/deleted
	DeletedBy      string `gorm:"size:64"`                   // e.g. daily_reconcile, user_delete
	DeletedAt      *time.Time
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
	Completed      bool `gorm:"default:false"` // completion receipt; makes finish idempotent across lost responses
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
