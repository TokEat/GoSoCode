package clientd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// handshakeStub is a minimal server implementing /api/agent/handshake plus one
// encrypted control endpoint, mirroring the real negotiation protocol.
type handshakeStub struct {
	t          *testing.T
	sessions   map[string]*controlcrypto.Codec
	handshakes int
	nextID     int
}

func (h *handshakeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/agent/handshake":
		h.handshakes++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			h.t.Fatalf("handshake missing authorization: %q", r.Header.Get("Authorization"))
		}
		var in struct {
			ClientPub string `json:"client_pub"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			h.t.Fatal(err)
		}
		clientPub, err := base64.StdEncoding.DecodeString(in.ClientPub)
		if err != nil {
			h.t.Fatal(err)
		}
		priv, err := controlcrypto.GenerateHandshakeKey()
		if err != nil {
			h.t.Fatal(err)
		}
		serverPub := priv.PublicKey().Bytes()
		codec, err := controlcrypto.DeriveSessionCodec(priv, clientPub, serverPub, clientPub)
		if err != nil {
			h.t.Fatal(err)
		}
		h.nextID++
		sid := fmt.Sprintf("sess-%d", h.nextID)
		h.sessions[sid] = codec
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"session_id": sid,
			"server_pub": base64.StdEncoding.EncodeToString(serverPub),
		})
	case "/api/agent/upload/init":
		if r.Header.Get(controlcrypto.Header) != controlcrypto.Version {
			h.t.Fatal("missing control encryption marker")
		}
		codec := h.sessions[r.Header.Get(controlcrypto.SessionHeader)]
		if codec == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"控制面会话无效或已过期，请重新协商密钥"}`))
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			h.t.Fatal(err)
		}
		request, err := codec.OpenRequest(r.Method, r.URL.Path, raw)
		if err != nil {
			h.t.Fatal(err)
		}
		if request.Authorization != "Bearer test-token" || string(request.Body) != `{"size":42}` {
			h.t.Fatalf("unexpected decrypted request: %#v", request)
		}
		response, err := codec.SealResponse(r.Method, r.URL.Path, http.StatusOK, []byte(`{"ok":true}`))
		if err != nil {
			h.t.Fatal(err)
		}
		w.Header().Set(controlcrypto.Header, controlcrypto.Version)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
	default:
		h.t.Fatalf("unexpected path %s", r.URL.Path)
	}
}

func TestServerJSONNegotiatesSessionKey(t *testing.T) {
	stub := &handshakeStub{t: t, sessions: map[string]*controlcrypto.Codec{}}
	server := httptest.NewServer(stub)
	defer server.Close()

	daemon := &Daemon{
		api:   server.Client(),
		cfg:   Config{Server: server.URL},
		proxy: &httputil.ReverseProxy{},
	}
	for i := 0; i < 2; i++ {
		status, raw, err := daemon.serverJSON(context.Background(), "Bearer test-token", http.MethodPost,
			"/api/agent/upload/init", []byte(`{"size":42}`))
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK || string(raw) != `{"ok":true}` {
			t.Fatalf("unexpected response status=%d body=%s", status, raw)
		}
	}
	if stub.handshakes != 1 {
		t.Fatalf("expected exactly one handshake for two calls, got %d", stub.handshakes)
	}
}

func TestServerJSONRenegotiatesExpiredSession(t *testing.T) {
	stub := &handshakeStub{t: t, sessions: map[string]*controlcrypto.Codec{}}
	server := httptest.NewServer(stub)
	defer server.Close()

	daemon := &Daemon{
		api:   server.Client(),
		cfg:   Config{Server: server.URL},
		proxy: &httputil.ReverseProxy{},
	}
	if _, _, err := daemon.serverJSON(context.Background(), "Bearer test-token", http.MethodPost,
		"/api/agent/upload/init", []byte(`{"size":42}`)); err != nil {
		t.Fatal(err)
	}
	// Simulate a server restart: all sessions gone; the client must
	// transparently renegotiate and retry once.
	stub.sessions = map[string]*controlcrypto.Codec{}
	status, raw, err := daemon.serverJSON(context.Background(), "Bearer test-token", http.MethodPost,
		"/api/agent/upload/init", []byte(`{"size":42}`))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || string(raw) != `{"ok":true}` {
		t.Fatalf("unexpected response status=%d body=%s", status, raw)
	}
	if stub.handshakes != 2 {
		t.Fatalf("expected renegotiation (2 handshakes), got %d", stub.handshakes)
	}
}
