package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestRunForwardsCapturedUpstreamAuthorization(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	p := New(nil, nil, nil)
	p.Client = upstream.Client()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer stale-forwarded-header")
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Custom-Caller-Header", "keep-me")

	res, err := p.Run(context.Background(), &Request{
		Method:       http.MethodPost,
		Path:         "/v1/responses",
		Body:         strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`),
		Headers:      headers,
		HostBaseURL:  upstream.URL,
		UpstreamAuth: "Bearer upstream-token",
		Lifecycle:    lifecycle.NewContext("req-test", "proxy", time.Now()),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)

	if got.Get("Authorization") != "Bearer upstream-token" {
		t.Fatalf("Authorization: got %q", got.Get("Authorization"))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type: got %q", got.Get("Content-Type"))
	}
	if got.Get("X-Custom-Caller-Header") != "keep-me" {
		t.Fatalf("X-Custom-Caller-Header: got %q", got.Get("X-Custom-Caller-Header"))
	}
}
