package clientd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gosocode/internal/baidu"
)

func TestLimitsForVipTiers(t *testing.T) {
	tests := []struct {
		name    string
		vipType int
		slice   int64
		window  int
		conns   int
		seg     int64
	}{
		{"normal", 0, 4 << 20, 8, 4, 4 << 20},
		{"vip", 1, 16 << 20, 6, 8, 8 << 20},
		{"svip", 2, 32 << 20, 4, 10, 16 << 20},
		{"svip higher levels use svip profile", 5, 32 << 20, 4, 10, 16 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lim := baidu.LimitsForVip(tt.vipType)
			if lim.SliceSize != tt.slice || lim.UploadWindow != tt.window || lim.DownloadConns != tt.conns || lim.DownloadSeg != tt.seg {
				t.Fatalf("LimitsForVip(%d) = %+v", tt.vipType, lim)
			}
			// tier must be recoverable from the planned slice size
			if got := baidu.LimitsForSliceSize(lim.SliceSize); got != lim {
				t.Fatalf("LimitsForSliceSize(%d) = %+v, want %+v", lim.SliceSize, got, lim)
			}
		})
	}
}

func TestFetchSegmentRetriesTruncatedResponse(t *testing.T) {
	payload := bytes.Repeat([]byte("retry-range-"), 16*1024)
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Length", "196608")
		w.WriteHeader(http.StatusPartialContent)
		if n == 1 {
			_, _ = w.Write(payload[:len(payload)/2])
			return
		}
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	task := &Task{ID: "retry", Kind: "download"}
	src := &dlSource{
		d: &Daemon{baidu: baidu.New("", "")}, task: task,
		info: &dlInfo{Dlink: ts.URL, Token: "token"},
	}
	var progress []int64
	got, err := src.fetchSegment(context.Background(), 0, int64(len(payload)-1), func(n int64) {
		progress = append(progress, n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("retried payload mismatch")
	}
	if requests.Load() < 2 {
		t.Fatalf("requests = %d, want at least 2", requests.Load())
	}
	zeroes := 0
	for _, n := range progress {
		if n == 0 {
			zeroes++
		}
	}
	if zeroes < 2 {
		t.Fatalf("progress was not reset before retry: %v", progress)
	}
}

func TestFetchSegmentReportsProgressWhileReading(t *testing.T) {
	payload := bytes.Repeat([]byte("gsc-progress-"), 32*1024)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-425983" {
			t.Errorf("Range = %q", got)
		}
		w.Header().Set("Content-Length", "425984")
		w.WriteHeader(http.StatusPartialContent)
		for off := 0; off < len(payload); off += 64 * 1024 {
			end := off + 64*1024
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = w.Write(payload[off:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(time.Millisecond)
		}
	}))
	defer ts.Close()

	task := &Task{ID: "test", Kind: "download"}
	src := &dlSource{
		d: &Daemon{baidu: baidu.New("", "")}, task: task,
		info: &dlInfo{Dlink: ts.URL, Token: "token"},
	}
	var progress []int64
	got, err := src.fetchSegment(context.Background(), 0, int64(len(payload)-1), func(n int64) {
		progress = append(progress, n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("downloaded payload mismatch")
	}
	intermediate := false
	for _, n := range progress {
		if n > 0 && n < int64(len(payload)) {
			intermediate = true
			break
		}
	}
	if !intermediate {
		t.Fatalf("progress did not include an intermediate byte count: %v", progress)
	}
}
