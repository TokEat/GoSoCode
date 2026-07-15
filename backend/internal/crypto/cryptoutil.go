// Package cryptoutil implements the at-rest encryption primitives: a seekable
// AES-256-CTR stream (same transform for encrypt and decrypt, so ranged
// downloads can start mid-file) and AES-GCM wrapping for keys/passwords stored
// in the database.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// NewFileKey returns a fresh random 32-byte AES key and 16-byte CTR IV.
func NewFileKey() (key, iv []byte, err error) {
	key = make([]byte, 32)
	iv = make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return nil, nil, err
	}
	if _, err = rand.Read(iv); err != nil {
		return nil, nil, err
	}
	return key, iv, nil
}

// NewIV returns a fresh random 16-byte CTR IV. With a shared per-node key,
// every file must still get its own IV.
func NewIV() ([]byte, error) {
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	return iv, nil
}

type ctrReader struct {
	r      io.Reader
	stream cipher.Stream
}

func (c *ctrReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.stream.XORKeyStream(p[:n], p[:n])
	}
	return n, err
}

// NewCTRReader wraps r with an AES-256-CTR transform positioned at the given
// plaintext/ciphertext byte offset. CTR is symmetric: the same reader encrypts
// plaintext and decrypts ciphertext.
func NewCTRReader(r io.Reader, key, iv []byte, offset int64) (io.Reader, error) {
	stream, err := ctrAt(key, iv, offset)
	if err != nil {
		return nil, err
	}
	return &ctrReader{r: r, stream: stream}, nil
}

// XORAt applies the CTR keystream to data in place, treating data as the bytes
// at the given stream offset. Used where whole segments are already in memory.
func XORAt(data, key, iv []byte, offset int64) error {
	stream, err := ctrAt(key, iv, offset)
	if err != nil {
		return err
	}
	stream.XORKeyStream(data, data)
	return nil
}

func ctrAt(key, iv []byte, offset int64) (cipher.Stream, error) {
	if offset < 0 {
		return nil, fmt.Errorf("negative offset")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	// Advance the 128-bit counter by offset/16 blocks.
	ctr := make([]byte, 16)
	copy(ctr, iv)
	addCounter(ctr, uint64(offset/16))
	stream := cipher.NewCTR(block, ctr)
	// Burn the partial block's keystream so we land mid-block.
	if skip := offset % 16; skip > 0 {
		waste := make([]byte, skip)
		stream.XORKeyStream(waste, waste)
	}
	return stream, nil
}

func addCounter(ctr []byte, n uint64) {
	for i := len(ctr) - 1; i >= 0 && n > 0; i-- {
		n += uint64(ctr[i])
		ctr[i] = byte(n)
		n >>= 8
	}
}

// Wrap encrypts plain with the hex master key (AES-256-GCM, random nonce) and
// returns base64(nonce|ciphertext). Used for per-file keys and access passwords.
func Wrap(masterKeyHex, plain string) (string, error) {
	gcm, err := gcmFor(masterKeyHex)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Unwrap reverses Wrap.
func Unwrap(masterKeyHex, wrapped string) (string, error) {
	gcm, err := gcmFor(masterKeyHex)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("wrapped data too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func gcmFor(masterKeyHex string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("invalid master key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
