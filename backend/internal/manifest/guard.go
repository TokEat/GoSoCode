package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gosocode/internal/models"
)

// ErrEmptyManifestRefused is returned when a write would replace an existing
// non-empty remote manifest with an empty one (0 entries). That is the failure
// that previously overwrote live metadata with a ~91-byte empty document.
var ErrEmptyManifestRefused = errors.New("拒绝写入空清单：网盘上已有非空元数据清单，放弃覆盖以免丢失映射与密钥")

// ErrEmptyManifestSkipped is returned when the local snapshot has no mapping
// entries and writing an empty manifest would be pointless or unsafe
// (including a first-time empty file when none exists remotely).
var ErrEmptyManifestSkipped = errors.New("跳过写入空清单：本地没有可写入的映射条目")

// ErrNotFound is returned by Fetch when the node's mount root has no
// .gsc-manifest.json.
var ErrNotFound = errors.New("网盘上未找到元数据清单")

// IsEmptyDoc reports a missing document or one with zero entries (including
// JSON "entries": null).
func IsEmptyDoc(doc *Doc) bool {
	return doc == nil || len(doc.Entries) == 0
}

// ParseDoc decodes a manifest document. Invalid JSON is an error, not empty.
func ParseDoc(data []byte) (*Doc, error) {
	var doc Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("清单格式无效: %w", err)
	}
	return &doc, nil
}

// DecideWrite reports whether local may be uploaded in place of the observed
// remote manifest. remoteExists is true only when the remote file was found;
// if the remote state could not be observed, the caller must fail closed and
// not call this. Non-empty local snapshots are always allowed, including
// legitimate shrinks after a file delete. Empty local snapshots are never
// uploaded: they refuse when the remote is non-empty (or present but
// unreadable as a document), and are skipped when the remote is missing or
// already empty.
func DecideWrite(local, remote *Doc, remoteExists bool) error {
	if !IsEmptyDoc(local) {
		return nil
	}
	if remoteExists && (remote == nil || !IsEmptyDoc(remote)) {
		n := 0
		if remote != nil {
			n = len(remote.Entries)
		}
		return fmt.Errorf("%w（远程 %d 条）", ErrEmptyManifestRefused, n)
	}
	return ErrEmptyManifestSkipped
}

// fetchIfExists downloads the remote manifest when present.
// exists is false and doc is nil when the file is absent. Any other Fetch
// error (network, HTTP, invalid JSON) is returned so the caller can fail closed.
func (s *Syncer) fetchIfExists(ctx context.Context, acc *models.BaiduAccount) (*Doc, bool, error) {
	doc, err := s.Fetch(ctx, acc)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return doc, true, nil
}
