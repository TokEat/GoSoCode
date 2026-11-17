package baidu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func TestSearchSendsRecursiveQueryAndPages(t *testing.T) {
	var pages []int
	c := New("", "")
	c.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if q.Get("method") != "search" {
			t.Fatalf("method=%s", q.Get("method"))
		}
		if q.Get("key") != "报告" {
			t.Fatalf("key=%s", q.Get("key"))
		}
		if q.Get("dir") != "/apps/gosocode" {
			t.Fatalf("dir=%s", q.Get("dir"))
		}
		if q.Get("recursion") != "1" {
			t.Fatalf("recursion=%s", q.Get("recursion"))
		}
		page, _ := strconv.Atoi(q.Get("page"))
		pages = append(pages, page)
		if page == 1 {
			list := make([]map[string]any, 200)
			for i := 0; i < 200; i++ {
				list[i] = map[string]any{
					"fs_id":           i + 1,
					"path":            "/apps/gosocode/p1-" + strconv.Itoa(i),
					"server_filename": "p1-" + strconv.Itoa(i) + ".txt",
					"size":            10,
					"isdir":           0,
				}
			}
			b, _ := json.Marshal(map[string]any{"errno": 0, "list": list})
			return jsonResp(r, string(b)), nil
		}
		return jsonResp(r, `{"errno":0,"list":[{"fs_id":201,"path":"/apps/gosocode/docs/报告.pdf","server_filename":"报告.pdf","size":12,"isdir":0}]}`), nil
	})})

	items, err := c.Search(context.Background(), "tok", "/apps/gosocode", "报告")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0] != 1 || pages[1] != 2 {
		t.Fatalf("pages=%v", pages)
	}
	if len(items) != 201 {
		t.Fatalf("len=%d", len(items))
	}
	last := items[len(items)-1]
	if last.ServerFilename != "报告.pdf" || last.Path != "/apps/gosocode/docs/报告.pdf" {
		t.Fatalf("last=%+v", last)
	}
}

func TestSearchEmptyKey(t *testing.T) {
	c := New("", "")
	c.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("should not call API")
		return nil, nil
	})})
	items, err := c.Search(context.Background(), "tok", "/", "  ")
	if err != nil || items != nil {
		t.Fatalf("err=%v items=%v", err, items)
	}
}
