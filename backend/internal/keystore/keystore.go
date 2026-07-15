// Package keystore manages the per-storage-node encryption keys. Each Baidu
// account (node) gets one AES-256 key, generated on first use and stored
// twice: wrapped with the master key on the BaiduAccount row, and as a raw-hex
// file under the keys directory. Either copy restores the other, so losing one
// store never loses access to encrypted data.
package keystore

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gorm.io/gorm"

	"gosocode/internal/crypto"
	"gosocode/internal/models"
)

// mu serializes generation/recovery so two concurrent uploads on a fresh node
// can't mint two different keys.
var mu sync.Mutex

// NodeKey returns the node's 32-byte AES key, creating and double-storing it
// on first use, and self-healing whichever copy (DB or file) is missing.
func NodeKey(db *gorm.DB, masterKeyHex, dir string, accID uint) ([]byte, error) {
	mu.Lock()
	defer mu.Unlock()

	var acc models.BaiduAccount
	if err := db.First(&acc, accID).Error; err != nil {
		return nil, fmt.Errorf("storage node %d not found: %w", accID, err)
	}
	file := filepath.Join(dir, fmt.Sprintf("node-%d.key", accID))

	// 1. DB copy (authoritative when present).
	if acc.EncKey != "" {
		if hexKey, err := crypto.Unwrap(masterKeyHex, acc.EncKey); err == nil {
			if key, derr := hex.DecodeString(hexKey); derr == nil && len(key) == 32 {
				writeKeyFile(dir, file, hexKey) // restore the file copy if missing
				return key, nil
			}
		}
		log.Printf("[keystore] node=%d DB key unreadable, trying local key file", accID)
	}

	// 2. Local file copy: restore the DB from it.
	if raw, err := os.ReadFile(file); err == nil {
		hexKey := strings.TrimSpace(string(raw))
		if key, derr := hex.DecodeString(hexKey); derr == nil && len(key) == 32 {
			if wrapped, werr := crypto.Wrap(masterKeyHex, hexKey); werr == nil {
				db.Model(&models.BaiduAccount{}).Where("id = ?", accID).Update("enc_key", wrapped)
				log.Printf("[keystore] node=%d key restored into DB from %s", accID, file)
			}
			return key, nil
		}
		log.Printf("[keystore] node=%d local key file invalid, generating a new key", accID)
	}

	// 3. First use: generate and store both copies.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	hexKey := hex.EncodeToString(key)
	wrapped, err := crypto.Wrap(masterKeyHex, hexKey)
	if err != nil {
		return nil, err
	}
	if err := db.Model(&models.BaiduAccount{}).Where("id = ?", accID).Update("enc_key", wrapped).Error; err != nil {
		return nil, err
	}
	if err := writeKeyFile(dir, file, hexKey); err != nil {
		// The DB copy exists, so encryption can proceed; the file backup will be
		// re-attempted on the next key lookup.
		log.Printf("[keystore] node=%d WARNING: local key file write failed: %v", accID, err)
	} else {
		log.Printf("[keystore] node=%d key generated (DB + %s)", accID, file)
	}
	return key, nil
}

func writeKeyFile(dir, file, hexKey string) error {
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(hexKey+"\n"), 0o600)
}
