package clientd

import "testing"

func TestUploadMissingBytes(t *testing.T) {
	const slice = int64(4 << 20)
	size := 2*slice + 123
	if got, want := uploadMissingBytes(size, slice, []int{1, 2}), slice+123; got != want {
		t.Fatalf("uploadMissingBytes() = %d, want %d", got, want)
	}
	if got := uploadMissingBytes(size, slice, nil); got != 0 {
		t.Fatalf("uploadMissingBytes(nil) = %d", got)
	}
}
