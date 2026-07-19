// Package controlcrypto encrypts the sensitive CLI control-plane messages.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const (
	Header  = "X-GSC-Control-Encryption"
	Version = "v1"
)

type Codec struct {
	aead cipher.AEAD
}

type Envelope struct {
	Payload string `json:"payload"`
}

type Request struct {
	Authorization string `json:"authorization"`
	Query         string `json:"query,omitempty"`
	Body          []byte `json:"body,omitempty"`
}

// New creates an AES-256-GCM codec from a 64-character hexadecimal key.
// An empty key returns nil so callers fall back to the negotiated (ECDH)
// control encryption instead of a static shared secret.
func New(keyHex string) (*Codec, error) {
	if keyHex == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("control key must be 64 hexadecimal characters")
	}
	return NewFromKey(key)
}

// NewFromKey creates an AES-256-GCM codec from a raw 32-byte key.
func NewFromKey(key []byte) (*Codec, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("control key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead}, nil
}

func RequestAAD(method, path string) []byte {
	return []byte("gsc-control-v1 request\n" + method + "\n" + path)
}

func ResponseAAD(method, path string, status int) []byte {
	return []byte(fmt.Sprintf("gsc-control-v1 response\n%s\n%s\n%d", method, path, status))
}

func (c *Codec) SealRequest(method, path string, request Request) ([]byte, error) {
	plain, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return c.seal(plain, RequestAAD(method, path))
}

func (c *Codec) OpenRequest(method, path string, raw []byte) (Request, error) {
	plain, err := c.open(raw, RequestAAD(method, path))
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err := json.Unmarshal(plain, &request); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (c *Codec) SealResponse(method, path string, status int, plain []byte) ([]byte, error) {
	return c.seal(plain, ResponseAAD(method, path, status))
}

func (c *Codec) OpenResponse(method, path string, status int, raw []byte) ([]byte, error) {
	return c.open(raw, ResponseAAD(method, path, status))
}

func (c *Codec) seal(plain, additionalData []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := c.aead.Seal(nonce, nonce, plain, additionalData)
	return json.Marshal(Envelope{Payload: base64.RawStdEncoding.EncodeToString(sealed)})
}

func (c *Codec) open(raw, additionalData []byte) ([]byte, error) {
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	sealed, err := base64.RawStdEncoding.DecodeString(envelope.Payload)
	if err != nil || len(sealed) < c.aead.NonceSize() {
		return nil, fmt.Errorf("invalid encrypted control message")
	}
	return c.aead.Open(nil, sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():], additionalData)
}
