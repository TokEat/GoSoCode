package clientd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"testing"

	controlcrypto "gosocode/internal/crypto"
)

const testControlKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestServerJSONEncryptsControlRequestAndDecryptsResponse(t *testing.T) {
	codec, err := controlcrypto.New(testControlKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(controlcrypto.Header) != controlcrypto.Version {
			t.Fatal("missing control encryption marker")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := codec.OpenRequest(r.Method, r.URL.Path, raw)
		if err != nil {
			t.Fatal(err)
		}
		if request.Authorization != "Bearer test-token" || request.Query != "path=%2Fprivate" || string(request.Body) != `{"size":42}` {
			t.Fatalf("unexpected encrypted request: %#v", request)
		}
		response, err := codec.SealResponse(r.Method, r.URL.Path, http.StatusOK, []byte(`{"access_token":"secret"}`))
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set(controlcrypto.Header, controlcrypto.Version)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
	}))
	defer server.Close()

	daemon := &Daemon{
		api:     server.Client(),
		control: codec,
		cfg:     Config{Server: server.URL},
		proxy:   &httputil.ReverseProxy{},
	}
	status, raw, err := daemon.serverJSON(context.Background(), "Bearer test-token", http.MethodPost,
		"/api/agent/upload/init?path=%2Fprivate", []byte(`{"size":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || string(raw) != `{"access_token":"secret"}` {
		t.Fatalf("unexpected response status=%d body=%s", status, raw)
	}
}

func TestServerJSONPlaintextWithoutControlKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(controlcrypto.Header) != "" {
			t.Fatal("unexpected control encryption marker on plaintext request")
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("missing authorization header: %q", r.Header.Get("Authorization"))
		}
		if r.URL.RawQuery != "path=%2Fprivate" {
			t.Fatalf("unexpected query: %q", r.URL.RawQuery)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"size":42}` {
			t.Fatalf("unexpected body: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	daemon := &Daemon{
		api:   server.Client(),
		cfg:   Config{Server: server.URL},
		proxy: &httputil.ReverseProxy{},
	}
	status, raw, err := daemon.serverJSON(context.Background(), "Bearer test-token", http.MethodPost,
		"/api/agent/upload/init?path=%2Fprivate", []byte(`{"size":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || string(raw) != `{"ok":true}` {
		t.Fatalf("unexpected response status=%d body=%s", status, raw)
	}
}
