package adapter_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/keypool"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// newBearerSpec returns a minimal built Spec for the openai name.
func newBearerSpec(t *testing.T, path string, opts ...func(*adapter.Spec)) *adapter.Spec {
	t.Helper()
	s := &adapter.Spec{
		Name:         adapters.OpenAI,
		UpstreamPath: path,
		Auth: adapter.AuthStrategy{
			Header: "Authorization",
			Scheme: "Bearer",
		},
	}
	for _, o := range opts {
		o(s)
	}
	return s.Build()
}

func TestSpecAdapter_Call_URLAndAuth(t *testing.T) {
	var gotPath, gotAuth, gotCT string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()

	resp, err := a.Call(context.Background(), srv.URL, "sk-test", []byte(`{}`), nil, "", false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path: want /v1/chat/completions, got %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth: want 'Bearer sk-test', got %s", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type: want application/json, got %s", gotCT)
	}
}

func TestSpecAdapter_Call_HeaderForwarding(t *testing.T) {
	var gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCustom = r.Header.Get("X-Relay-Test")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()

	hdr := http.Header{"X-Relay-Test": []string{"hello"}}
	resp, err := a.Call(context.Background(), srv.URL, "key", []byte(`{}`), hdr, "", false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	if gotCustom != "hello" {
		t.Errorf("X-Relay-Test: want hello, got %s", gotCustom)
	}
}

func TestSpecAdapter_Call_ExtraHeaders(t *testing.T) {
	var gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("Anthropic-Version")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &adapter.Spec{
		Name:         adapters.Anthropic,
		UpstreamPath: "/v1/messages",
		Auth: adapter.AuthStrategy{
			Header: "x-api-key",
			ExtraHeaders: map[string]string{
				"Anthropic-Version": "2023-06-01",
			},
		},
	}
	s.Build()
	a := s.PipelineAdapter()

	resp, err := a.Call(context.Background(), srv.URL, "key", []byte(`{}`), nil, "", false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	if gotVer != "2023-06-01" {
		t.Errorf("Anthropic-Version: want 2023-06-01, got %s", gotVer)
	}
}

func TestSpecAdapter_Call_ExtraHeaders_NotOverrideForwarded(t *testing.T) {
	// Forwarded header takes priority over ExtraHeaders default.
	var gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("Anthropic-Version")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &adapter.Spec{
		Name:         adapters.Anthropic,
		UpstreamPath: "/v1/messages",
		Auth: adapter.AuthStrategy{
			Header: "x-api-key",
			ExtraHeaders: map[string]string{
				"Anthropic-Version": "2023-06-01",
			},
		},
	}
	s.Build()
	a := s.PipelineAdapter()

	hdr := http.Header{"Anthropic-Version": []string{"2024-12-01"}}
	resp, err := a.Call(context.Background(), srv.URL, "key", []byte(`{}`), hdr, "", false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	// Forwarded header wins over ExtraHeaders default.
	if gotVer != "2024-12-01" {
		t.Errorf("Anthropic-Version: want forwarded 2024-12-01, got %s", gotVer)
	}
}

func TestSpecAdapter_Call_EmptyAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()

	resp, err := a.Call(context.Background(), srv.URL, "", []byte(`{}`), nil, "", false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "" {
		t.Errorf("expected no auth header for empty key, got %q", gotAuth)
	}
}

func TestSpecAdapter_Call_BodyForwarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(b)
	}))
	defer srv.Close()

	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()

	want := []byte(`{"model":"gpt-4o","messages":[]}`)
	resp, err := a.Call(context.Background(), srv.URL, "key", want, nil, "", false)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !bytes.Equal(got, want) {
		t.Errorf("body mismatch: want %s, got %s", want, got)
	}
}

func TestSpecAdapter_Retryable(t *testing.T) {
	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()

	cases := []struct {
		status    int
		wantRetry bool
		wantKind  keypool.FailureKind
	}{
		{200, false, 0},
		{400, false, 0},
		{401, true, keypool.FailureAuth},
		{403, true, keypool.FailureAuth},
		{429, true, keypool.FailureRateLimitShort},
		{500, true, keypool.FailureServerError},
		{529, true, keypool.FailureServerError},
	}

	for _, tc := range cases {
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
		retry, kind, _ := a.Retryable(resp)
		if retry != tc.wantRetry {
			t.Errorf("status %d: retry want %v got %v", tc.status, tc.wantRetry, retry)
		}
		if retry && kind != tc.wantKind {
			t.Errorf("status %d: kind want %v got %v", tc.status, tc.wantKind, kind)
		}
	}
}

func TestSpecAdapter_Retryable_LongRetryAfter(t *testing.T) {
	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()

	hdr := http.Header{"Retry-After": []string{"30"}}
	resp := &http.Response{StatusCode: 429, Header: hdr}
	retry, kind, ra := a.Retryable(resp)
	if !retry {
		t.Fatal("429 should be retryable")
	}
	if kind != keypool.FailureRateLimitLong {
		t.Errorf("kind: want FailureRateLimitLong, got %v", kind)
	}
	if ra <= 5*time.Second {
		t.Errorf("retryAfter should be > 5s for 30s Retry-After, got %v", ra)
	}
}

func TestSpecAdapter_Retryable_NilResp(t *testing.T) {
	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()
	retry, _, _ := a.Retryable(nil)
	if retry {
		t.Error("nil resp should not be retryable")
	}
}

func TestSpecAdapter_ExtractTokens_Nil(t *testing.T) {
	// Spec with no ExtractTokens should return nil.
	s := newBearerSpec(t, "/v1/chat/completions")
	a := s.PipelineAdapter()
	tok := a.ExtractTokens([]byte(`{"id":"x"}`))
	if tok != nil {
		t.Errorf("expected nil when ExtractTokens unset, got %v", tok)
	}
}

func TestSpecAdapter_ExtractTokens_Custom(t *testing.T) {
	called := false
	s := &adapter.Spec{
		Name:         adapters.OpenAI,
		UpstreamPath: "/v1/chat/completions",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		ExtractTokens: func(_ []byte) pkgusage.Tokens {
			called = true
			return pkgusage.Tokens{"input": 10, "output": 5}
		},
	}
	s.Build()
	a := s.PipelineAdapter()
	tok := a.ExtractTokens([]byte(`{}`))
	if !called {
		t.Error("ExtractTokens was not called")
	}
	if tok["input"] != 10 { //nolint:gomnd
		t.Errorf("input: want 10, got %d", tok["input"])
	}
}
