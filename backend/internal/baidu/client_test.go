package baidu

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUploadChunkToProgressStreamsAndKeepsContentLength(t *testing.T) {
	chunk := bytes.Repeat([]byte{0xAB}, 256<<10)
	var gotLen int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength <= 0 {
			t.Fatalf("expected explicit Content-Length, got %d (chunked?)", r.ContentLength)
		}
		gotLen = r.ContentLength
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, chunk) {
			t.Fatal("multipart body does not contain the chunk payload")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"md5":"x"}`))
	}))
	defer server.Close()

	var reports []int64
	c := New("", "")
	err := c.UploadChunkToProgress(context.Background(), server.URL, "tok", "/apps/x/f", "uid", 0, chunk, func(sent int64) {
		reports = append(reports, sent)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) == 0 {
		t.Fatal("no progress reports")
	}
	for i := 1; i < len(reports); i++ {
		if reports[i] < reports[i-1] {
			t.Fatalf("progress went backwards: %v", reports)
		}
	}
	final := reports[len(reports)-1]
	if final != gotLen {
		t.Fatalf("final progress %d != request body length %d", final, gotLen)
	}
	if final < int64(len(chunk)) {
		t.Fatalf("final progress %d smaller than chunk %d", final, len(chunk))
	}
}
