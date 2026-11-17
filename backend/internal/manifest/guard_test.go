package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gosocode/internal/account"
	"gosocode/internal/baidu"
	"gosocode/internal/coord"
	"gosocode/internal/models"
	"gosocode/internal/store"
)

func TestDecideWrite(t *testing.T) {
	empty := &Doc{Version: 1, Entries: nil}
	emptyArr := &Doc{Version: 1, Entries: []Entry{}}
	remote := &Doc{Version: 1, Entries: []Entry{{Path: "secret.bin", Encrypted: true}}}
	local := &Doc{Version: 1, Entries: []Entry{{Path: "a.bin"}, {Path: "b.bin"}}}
	smaller := &Doc{Version: 1, Entries: []Entry{{Path: "a.bin"}}}

	if err := DecideWrite(local, remote, true); err != nil {
		t.Fatalf("non-empty local should write: %v", err)
	}
	if err := DecideWrite(smaller, remote, true); err != nil {
		t.Fatalf("legitimate shrink should write: %v", err)
	}
	if err := DecideWrite(local, nil, false); err != nil {
		t.Fatalf("first non-empty write should proceed: %v", err)
	}
	if !errors.Is(DecideWrite(empty, remote, true), ErrEmptyManifestRefused) {
		t.Fatal("empty must not replace a non-empty remote")
	}
	if !errors.Is(DecideWrite(emptyArr, remote, true), ErrEmptyManifestRefused) {
		t.Fatal("empty array must not replace a non-empty remote")
	}
	if !errors.Is(DecideWrite(empty, nil, true), ErrEmptyManifestRefused) {
		t.Fatal("present but unread remote must be refused")
	}
	if !errors.Is(DecideWrite(empty, empty, true), ErrEmptyManifestSkipped) {
		t.Fatal("empty replacing already-empty remote should skip")
	}
	if !errors.Is(DecideWrite(empty, nil, false), ErrEmptyManifestSkipped) {
		t.Fatal("first-time empty write should be skipped")
	}
}

func TestParseDocEmptyJSON(t *testing.T) {
	raw, err := json.MarshalIndent(Doc{Version: 1, GeneratedAt: time.Now()}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDoc(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !IsEmptyDoc(doc) {
		t.Fatalf("expected empty, bytes=%d json=%s", len(raw), raw)
	}
	if _, err := ParseDoc([]byte("not-json")); err == nil {
		t.Fatal("invalid JSON must not be treated as an empty doc")
	}
}

func TestBuildOmitsPassthroughAndIsEmpty(t *testing.T) {
	s, acc := newManifestFixture(t)
	if err := s.db.Create(&models.File{
		BaiduAccountID: acc.ID, LogicalPath: "/资料盘/readme.txt",
		RemotePath: "/apps/gosocode/readme.txt", Size: 10, Status: "ready",
	}).Error; err != nil {
		t.Fatal(err)
	}
	data, err := Build(s.db, acc)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDoc(data)
	if err != nil {
		t.Fatal(err)
	}
	if !IsEmptyDoc(doc) {
		t.Fatalf("passthrough-only node should build an empty manifest, got %s", data)
	}
}

func TestSyncNowRefusesEmptyOverwrite(t *testing.T) {
	s, acc := newManifestFixture(t)
	remote := Doc{Version: 1, GeneratedAt: time.Now(), Entries: []Entry{
		{Path: "secret.bin", RemotePath: "/apps/gosocode/secret.bin", Encrypted: true, EncKey: "wrapped"},
	}}
	body, _ := json.MarshalIndent(remote, "", " ")
	stub := newManifestHTTPStub(body)
	s.client.SetHTTPClient(&http.Client{Transport: stub})

	err := s.SyncNow(context.Background(), acc.ID)
	if !errors.Is(err, ErrEmptyManifestRefused) {
		t.Fatalf("got %v", err)
	}
	if stub.writes() != 0 {
		t.Fatalf("empty overwrite must not upload, writes=%d", stub.writes())
	}
	if stub.listCalls == 0 {
		t.Fatal("should have listed the remote mount to inspect the existing manifest")
	}
}

func TestSyncNowSkipsFirstTimeEmpty(t *testing.T) {
	s, acc := newManifestFixture(t)
	stub := newManifestHTTPStub(nil) // no remote manifest
	s.client.SetHTTPClient(&http.Client{Transport: stub})

	err := s.SyncNow(context.Background(), acc.ID)
	if !errors.Is(err, ErrEmptyManifestSkipped) {
		t.Fatalf("got %v", err)
	}
	if stub.writes() != 0 {
		t.Fatalf("first-time empty write must be skipped, writes=%d", stub.writes())
	}
}

func TestSyncNowFailClosedWhenRemoteUnreadable(t *testing.T) {
	s, acc := newManifestFixture(t)
	stub := &manifestHTTPStub{listErr: true}
	s.client.SetHTTPClient(&http.Client{Transport: stub})

	err := s.SyncNow(context.Background(), acc.ID)
	if err == nil || errors.Is(err, ErrEmptyManifestSkipped) || errors.Is(err, ErrEmptyManifestRefused) {
		t.Fatalf("expected fail-closed fetch error, got %v", err)
	}
	if stub.writes() != 0 {
		t.Fatalf("must not upload when remote state is unknown, writes=%d", stub.writes())
	}
}

func TestSyncNowWritesNonEmpty(t *testing.T) {
	s, acc := newManifestFixture(t)
	if err := s.db.Create(&models.File{
		BaiduAccountID: acc.ID, LogicalPath: "/资料盘/secret.bin",
		RemotePath: "/apps/gosocode/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		RemoteUUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Encrypted:  true, Size: 32, Status: "ready",
	}).Error; err != nil {
		t.Fatal(err)
	}
	stub := newManifestHTTPStub(nil)
	s.client.SetHTTPClient(&http.Client{Transport: stub})

	if err := s.SyncNow(context.Background(), acc.ID); err != nil {
		t.Fatal(err)
	}
	if stub.writes() == 0 {
		t.Fatal("non-empty manifest should be uploaded")
	}
	if !strings.Contains(string(stub.lastUpload), `"secret.bin"`) {
		t.Fatalf("uploaded document missing entry: %s", stub.lastUpload)
	}
}

func newManifestFixture(t *testing.T) (*Syncer, *models.BaiduAccount) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	client := baidu.New("", "")
	pool := account.NewPool(db, client)
	co, err := coord.New("", 50)
	if err != nil {
		t.Fatal(err)
	}
	s := &Syncer{
		db: db, pool: pool, client: client, coord: co,
		dirty: map[uint]bool{}, kick: make(chan struct{}, 1),
		lastSync: map[uint]time.Time{},
	}
	acc := &models.BaiduAccount{
		Nickname:    "资料盘",
		MountName:   "资料盘",
		MountPath:   "/apps/gosocode",
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(100 * 24 * time.Hour),
		Status:      "active",
	}
	if err := db.Create(acc).Error; err != nil {
		t.Fatal(err)
	}
	return s, acc
}

type manifestHTTPStub struct {
	mu         sync.Mutex
	remoteBody []byte
	listErr    bool
	listCalls  int
	precreates int
	uploads    int
	creates    int
	lastUpload []byte
}

func newManifestHTTPStub(remoteBody []byte) *manifestHTTPStub {
	return &manifestHTTPStub{remoteBody: remoteBody}
}

func (s *manifestHTTPStub) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.precreates + s.uploads + s.creates
}

func (s *manifestHTTPStub) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := r.URL
	q := u.Query()
	method := q.Get("method")
	path := u.Path
	jsonOK := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(b)),
			Request:    r,
		}
	}
	switch {
	case method == "list":
		s.listCalls++
		if s.listErr {
			return nil, errors.New("simulated list failure")
		}
		var list []map[string]any
		if s.remoteBody != nil {
			list = append(list, map[string]any{
				"fs_id": 99, "path": "/apps/gosocode/.gsc-manifest.json",
				"server_filename": ".gsc-manifest.json", "size": len(s.remoteBody), "isdir": 0,
			})
		}
		return jsonOK(map[string]any{"errno": 0, "list": list}), nil
	case method == "filemetas":
		return jsonOK(map[string]any{"errno": 0, "list": []map[string]any{{
			"fs_id": 99, "filename": ".gsc-manifest.json", "size": len(s.remoteBody),
			"dlink": "https://dlink.test/gsc-manifest",
		}}}), nil
	case strings.Contains(u.Host, "dlink.test"):
		body := s.remoteBody
		if body == nil {
			body = []byte(`{}`)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	case method == "precreate":
		s.precreates++
		return jsonOK(map[string]any{"errno": 0, "uploadid": "u1", "return_type": 1}), nil
	case strings.Contains(path, "superfile2") || method == "upload":
		s.uploads++
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			if i := bytes.Index(b, []byte("{")); i >= 0 {
				s.lastUpload = append([]byte(nil), b[i:]...)
			}
		}
		return jsonOK(map[string]any{"errno": 0, "md5": "abc"}), nil
	case method == "create":
		s.creates++
		return jsonOK(map[string]any{"errno": 0, "fs_id": 1, "size": 1}), nil
	default:
		return jsonOK(map[string]any{"errno": 0}), nil
	}
}
