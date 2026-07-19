package crypto

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// SessionHeader carries the negotiated control-session id so the server can
// look up the per-session codec derived during the handshake.
const SessionHeader = "X-GSC-Control-Session"

// GenerateHandshakeKey returns a fresh X25519 key pair for the ECDH
// control-plane handshake.
func GenerateHandshakeKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// DeriveSessionCodec runs X25519 ECDH against the peer public key and derives
// the AES-256-GCM session codec via HKDF-SHA256. The info binds both public
// keys in a fixed server/client order so both sides derive the same key.
func DeriveSessionCodec(priv *ecdh.PrivateKey, peerPubRaw, serverPubRaw, clientPubRaw []byte) (*Codec, error) {
	peer, err := ecdh.X25519().NewPublicKey(peerPubRaw)
	if err != nil {
		return nil, fmt.Errorf("invalid handshake public key: %w", err)
	}
	shared, err := priv.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("ecdh failed: %w", err)
	}
	info := append(append([]byte("gsc-control-handshake-v1"), serverPubRaw...), clientPubRaw...)
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, nil, info), key); err != nil {
		return nil, err
	}
	return NewFromKey(key)
}
