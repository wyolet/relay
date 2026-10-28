package adapter

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/adapters"
)

// Failover hands the same forwarded headers to every attempt; each attempt sets its own credential.
func TestNewRequestCopiesHeadersPerAttempt(t *testing.T) {
	s := (&Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions", Auth: AuthStrategy{Header: "Authorization", Scheme: "Bearer"}}).Build()
	a := &specAdapter{spec: s}
	hdr := http.Header{"Accept": {"application/json"}, "X-Multi": {"a", "b"}}

	first, release1, err := a.newRequest(context.Background(), "http://first.invalid/", "key-1", []byte(`{}`), hdr, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release1()
	second, release2, err := a.newRequest(context.Background(), "http://second.invalid/", "key-2", []byte(`{}`), hdr, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()

	first.Header["X-Multi"] = append(first.Header["X-Multi"], "c")
	first.Header.Set("Accept", "text/plain")

	if got := second.Header.Get("Authorization"); got != "Bearer key-2" {
		t.Errorf("second Authorization = %q, want Bearer key-2", got)
	}
	if got := second.Header.Values("X-Multi"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("second X-Multi = %q, want [a b]", got)
	}
	if got := second.Header.Get("Accept"); got != "application/json" {
		t.Errorf("second Accept = %q, want application/json", got)
	}
	if len(hdr) != 2 || hdr.Get("Authorization") != "" || hdr.Get("Content-Type") != "" || len(hdr["X-Multi"]) != 2 || hdr.Get("Accept") != "application/json" {
		t.Errorf("forwarded headers changed: %v", hdr)
	}
}

func TestNewRequestNilHeaders(t *testing.T) {
	s := (&Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions", Auth: AuthStrategy{Header: "Authorization", Scheme: "Bearer"}}).Build()
	req, release, err := (&specAdapter{spec: s}).newRequest(context.Background(), "http://upstream.invalid/", "key", []byte(`{}`), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if req.Header.Get("Authorization") != "Bearer key" || req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", req.Header)
	}
}
