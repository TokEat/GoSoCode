package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func TestLooksLikeCiphertextPlaintextAndMagic(t *testing.T) {
	cases := [][]byte{
		[]byte("hello, this is a normal plaintext file\nwith a second line\n"),
		bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 8),
		append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("stream data "), 40)...),
		append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0x01, 0x02}, 200)...),
		append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0x11, 0x22, 0x33}, 100)...),
	}
	// fix the zip magic — the slice above accidentally escaped. rebuild:
	cases[4] = append([]byte{'P', 'K', 0x03, 0x04}, bytes.Repeat([]byte{0x11, 0x22, 0x33}, 100)...)
	for i, c := range cases {
		if LooksLikeCiphertext(c) {
			t.Fatalf("case %d should be treated as plaintext", i)
		}
	}
}

func TestLooksLikeCiphertextRandom(t *testing.T) {
	buf := make([]byte, 512)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !LooksLikeCiphertext(buf) {
		t.Fatal("high-entropy prefix with no magic should look like ciphertext")
	}
	if LooksLikeCiphertext(buf[:64]) {
		t.Fatal("short prefixes must not be classified")
	}
}

func TestGuardPlaintextPrefix(t *testing.T) {
	plain := []byte("hello world, this is ordinary text that should download fine\n")
	r, err := GuardPlaintextPrefix(bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch %q", got)
	}

	cipher := make([]byte, 512)
	if _, err := rand.Read(cipher); err != nil {
		t.Fatal(err)
	}
	if _, err := GuardPlaintextPrefix(bytes.NewReader(cipher)); !errors.Is(err, ErrLooksLikeCiphertext) {
		t.Fatalf("ciphertext prefix: %v", err)
	}
}
