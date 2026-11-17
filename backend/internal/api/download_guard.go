package api

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"gosocode/internal/crypto"
	"gosocode/internal/models"
)

// User-facing download abort messages. They must not include key material.
var (
	errDownloadKeys       = errors.New("文件已标记为加密存储，但缺少可用的解密密钥，已中止下载")
	errCiphertextDownload = errors.New("文件内容疑似加密密文且无法解密，已中止下载")
)

// ensureDecryptable fails closed before any upstream download when the file is
// marked encrypted but the wrapped key/IV cannot be unwrapped. Nil or
// plaintext files pass through.
func (s *Server) ensureDecryptable(f *models.File) error {
	if f == nil || !f.Encrypted {
		return nil
	}
	if _, _, err := s.fileEncKeys(f); err != nil {
		log.Printf("[download] cannot unwrap file keys file=%d: %v", f.ID, err)
		return errDownloadKeys
	}
	return nil
}

// guardedDownloadBody peeks at the opening bytes of an undecrypted stream and
// refuses to return a reader when they look like ciphertext. Encrypted files
// that are already being decrypted, and ranged reads that do not start at
// byte 0, are left untouched.
func (s *Server) guardedDownloadBody(body io.Reader, f *models.File, rangeStart int64) (io.Reader, error) {
	if f != nil && f.Encrypted {
		return body, nil
	}
	if rangeStart > 0 {
		return body, nil
	}
	out, err := crypto.GuardPlaintextPrefix(body)
	if err != nil {
		if errors.Is(err, crypto.ErrLooksLikeCiphertext) {
			return nil, errCiphertextDownload
		}
		return nil, err
	}
	return out, nil
}

func contentRangeStart(resp *http.Response) int64 {
	if resp == nil {
		return 0
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		var off int64
		if _, err := fmt.Sscanf(cr, "bytes %d-", &off); err == nil {
			return off
		}
	}
	return 0
}

func (s *Server) rejectDownload(c *gin.Context, logLabel string, accID uint, err error) {
	log.Printf("[download] aborted %s node=%d: %v", logLabel, accID, err)
	msg := err.Error()
	if errors.Is(err, crypto.ErrLooksLikeCiphertext) {
		msg = errCiphertextDownload.Error()
	}
	c.JSON(http.StatusConflict, gin.H{"error": msg})
}
