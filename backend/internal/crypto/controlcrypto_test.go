package crypto

import "testing"

func TestRequestAndResponseRoundTrip(t *testing.T) {
	codec, err := New("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	want := Request{Authorization: "Bearer token", Query: "path=%2Fprivate", Body: []byte(`{"size":42}`)}
	raw, err := codec.SealRequest("POST", "/api/agent/upload/init", want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.OpenRequest("POST", "/api/agent/upload/init", raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Authorization != want.Authorization || got.Query != want.Query || string(got.Body) != string(want.Body) {
		t.Fatalf("request round trip mismatch: %#v", got)
	}

	response := []byte(`{"access_token":"secret"}`)
	raw, err = codec.SealResponse("POST", "/api/agent/upload/init", 200, response)
	if err != nil {
		t.Fatal(err)
	}
	gotResponse, err := codec.OpenResponse("POST", "/api/agent/upload/init", 200, raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotResponse) != string(response) {
		t.Fatalf("response round trip mismatch: %q", gotResponse)
	}
}

func TestRejectsDifferentRoute(t *testing.T) {
	codec, err := New("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := codec.SealRequest("GET", "/api/agent/download/resolve", Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.OpenRequest("GET", "/api/agent/upload/session", raw); err == nil {
		t.Fatal("expected route-bound ciphertext to be rejected")
	}
}
