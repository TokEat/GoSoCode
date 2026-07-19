package crypto

import (
	"net/http"
	"testing"
)

func TestDeriveSessionCodecBothSidesMatch(t *testing.T) {
	serverPriv, err := GenerateHandshakeKey()
	if err != nil {
		t.Fatal(err)
	}
	clientPriv, err := GenerateHandshakeKey()
	if err != nil {
		t.Fatal(err)
	}
	serverPub := serverPriv.PublicKey().Bytes()
	clientPub := clientPriv.PublicKey().Bytes()

	serverCodec, err := DeriveSessionCodec(serverPriv, clientPub, serverPub, clientPub)
	if err != nil {
		t.Fatal(err)
	}
	clientCodec, err := DeriveSessionCodec(clientPriv, serverPub, serverPub, clientPub)
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := clientCodec.SealRequest(http.MethodPost, "/api/agent/upload/init", Request{
		Authorization: "Bearer tok", Body: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := serverCodec.OpenRequest(http.MethodPost, "/api/agent/upload/init", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Authorization != "Bearer tok" || string(opened.Body) != `{"a":1}` {
		t.Fatalf("request roundtrip mismatch: %#v", opened)
	}

	resp, err := serverCodec.SealResponse(http.MethodPost, "/api/agent/upload/init", 200, []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := clientCodec.OpenResponse(http.MethodPost, "/api/agent/upload/init", 200, resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != `{"ok":true}` {
		t.Fatalf("response roundtrip mismatch: %s", plain)
	}
}

func TestDeriveSessionCodecRejectsWrongPeer(t *testing.T) {
	a, _ := GenerateHandshakeKey()
	b, _ := GenerateHandshakeKey()
	mallory, _ := GenerateHandshakeKey()
	aPub, bPub := a.PublicKey().Bytes(), b.PublicKey().Bytes()

	good, err := DeriveSessionCodec(a, bPub, aPub, bPub)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := DeriveSessionCodec(mallory, bPub, aPub, bPub)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := good.SealRequest(http.MethodGet, "/x", Request{Authorization: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.OpenRequest(http.MethodGet, "/x", sealed); err == nil {
		t.Fatal("codec derived from a different key pair must not decrypt")
	}
}
