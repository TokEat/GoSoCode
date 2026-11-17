package crypto

import (
	"bytes"
	"errors"
	"io"
	"math"
	"unicode/utf8"
)

// ErrLooksLikeCiphertext is returned when a stream's opening bytes look like
// high-entropy ciphertext rather than a recognizable plaintext format.
// GoSoCode at-rest files are raw AES-256-CTR with no magic header, so this
// detector is conservative: known file signatures and valid text are always
// allowed; only sufficiently long, high-entropy, unrecognized prefixes fail.
var ErrLooksLikeCiphertext = errors.New("content looks like ciphertext")

const sniffLen = 512

// LooksLikeCiphertext reports whether prefix appears to be this system's
// (or similarly unstructured) ciphertext rather than ordinary file data.
func LooksLikeCiphertext(prefix []byte) bool {
	if len(prefix) < 256 {
		return false
	}
	if hasKnownMagic(prefix) || mostlyText(prefix) {
		return false
	}
	return shannon(prefix) >= 7.5
}

// GuardPlaintextPrefix peeks at the start of r and returns an error instead of
// a reader when the bytes look like ciphertext. On success the returned reader
// yields the peeked bytes followed by the rest of r.
func GuardPlaintextPrefix(r io.Reader) (io.Reader, error) {
	if r == nil {
		return bytes.NewReader(nil), nil
	}
	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(r, buf)
	prefix := buf[:n]
	if LooksLikeCiphertext(prefix) {
		return nil, ErrLooksLikeCiphertext
	}
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(prefix), r), nil
}

func hasKnownMagic(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	starts := [][]byte{
		[]byte("%PDF"),
		{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a},
		{0xff, 0xd8, 0xff},
		[]byte("GIF87a"),
		[]byte("GIF89a"),
		[]byte("PK\x03\x04"),
		[]byte("PK\x05\x06"),
		[]byte("PK\x07\x08"),
		[]byte("Rar!\x1a"),
		{0x1f, 0x8b},
		[]byte("\x7fELF"),
		[]byte("MZ"),
		[]byte("OggS"),
		[]byte("RIFF"),
		[]byte("fLaC"),
		{0x37, 0x7a, 0xbc, 0xaf, 0x27, 0x1c}, // 7z
		{0xfd, '7', 'z', 'X', 'Z', 0x00},
		[]byte("BZh"),
		[]byte("SQLite format 3"),
		{0x1a, 0x45, 0xdf, 0xa3},
		{0x00, 0x00, 0x01, 0x00},
		[]byte("BM"),
		{0xca, 0xfe, 0xba, 0xbe},
		{0xfe, 0xed, 0xfa, 0xce},
		{0xfe, 0xed, 0xfa, 0xcf},
		{0xcf, 0xfa, 0xed, 0xfe},
		{0x00, 'a', 's', 'm'},
		{0xef, 0xbb, 0xbf},
		[]byte("ID3"),
		[]byte("#!"),
		[]byte("<?xml"),
		[]byte("<html"),
		[]byte("<!DOCTYPE"),
		[]byte("<!doctype"),
	}
	for _, m := range starts {
		if len(b) >= len(m) && bytes.Equal(b[:len(m)], m) {
			return true
		}
	}
	if len(b) >= 8 && bytes.Equal(b[4:8], []byte("ftyp")) {
		return true
	}
	return false
}

func mostlyText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	if len(b) == 0 {
		return true
	}
	ok := 0
	for _, c := range b {
		if c == '\t' || c == '\n' || c == '\r' || (c >= 0x20 && c != 0x7f) {
			ok++
		}
	}
	return float64(ok) >= 0.85*float64(len(b))
}

func shannon(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var freq [256]int
	for _, c := range b {
		freq[c]++
	}
	n := float64(len(b))
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
