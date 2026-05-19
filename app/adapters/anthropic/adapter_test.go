package anthropic_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters/anthropic"
	"github.com/wyolet/relay/app/keypool"
)

// newTestAdapter returns an Adapter pointed at the given test server.
func newTestAdapter(srv *httptest.Server) *anthropic.Adapter {
	return anthropic.New(anthropic.WithClient(srv.Client()))
}

// --- Call ---

func TestCall_BasicRequest(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	body := []byte(`{"model":"claude-3-5-haiku-latest","messages":[]}`)
	resp, err := a.Call(context.Background(), srv.URL, "sk-test-key", body, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/v1/messages" {
		t.Errorf("path: want /v1/messages, got %q", gotPath)
	}
	if gotAPIKey != "sk-test-key" {
		t.Errorf("x-api-key: want sk-test-key, got %q", gotAPIKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version: want 2023-06-01, got %q", gotVersion)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", gotCT)
	}
}

func TestCall_VersionPassthrough(t *testing.T) {
	// If caller passes anthropic-version it should not be overridden.
	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	hdr := http.Header{"Anthropic-Version": []string{"2024-01-01"}}
	resp, err := a.Call(context.Background(), srv.URL, "key", []byte(`{}`), hdr)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	if gotVersion != "2024-01-01" {
		t.Errorf("anthropic-version should be forwarded from hdr, got %q", gotVersion)
	}
}

func TestCall_PassthroughHeaders(t *testing.T) {
	var gotBeta, gotUserAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("Anthropic-Beta")
		gotUserAgent = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	hdr := http.Header{
		"Anthropic-Beta": []string{"tools-2024-04-04"},
		"User-Agent":     []string{"myapp/1.0"},
	}
	resp, err := a.Call(context.Background(), srv.URL, "key", []byte(`{}`), hdr)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	if gotBeta != "tools-2024-04-04" {
		t.Errorf("anthropic-beta: want tools-2024-04-04, got %q", gotBeta)
	}
	if gotUserAgent != "myapp/1.0" {
		t.Errorf("User-Agent: want myapp/1.0, got %q", gotUserAgent)
	}
}

func TestCall_BodyForwarded(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	want := []byte(`{"model":"claude-3-haiku","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := a.Call(context.Background(), srv.URL, "key", want, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	resp.Body.Close()

	if string(gotBody) != string(want) {
		t.Errorf("body mismatch: want %q, got %q", want, gotBody)
	}
}

// --- Retryable ---

func TestRetryable_NilResponse(t *testing.T) {
	a := anthropic.New()
	retry, _, _ := a.Retryable(nil)
	if retry {
		t.Error("nil response should not be retryable")
	}
}

func TestRetryable_200(t *testing.T) {
	a := anthropic.New()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	retry, _, _ := a.Retryable(resp)
	if retry {
		t.Error("200 should not be retryable")
	}
}

func TestRetryable_401(t *testing.T) {
	a := anthropic.New()
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}}
	retry, kind, _ := a.Retryable(resp)
	if !retry {
		t.Error("401 should be retryable")
	}
	if kind != keypool.FailureAuth {
		t.Errorf("kind: want FailureAuth, got %v", kind)
	}
}

func TestRetryable_403(t *testing.T) {
	a := anthropic.New()
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	retry, kind, _ := a.Retryable(resp)
	if !retry {
		t.Error("403 should be retryable")
	}
	if kind != keypool.FailureAuth {
		t.Errorf("kind: want FailureAuth, got %v", kind)
	}
}

func TestRetryable_429_ShortRetryAfter(t *testing.T) {
	a := anthropic.New()
	hdr := http.Header{"Retry-After": []string{"3"}}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: hdr}
	retry, kind, ra := a.Retryable(resp)
	if !retry {
		t.Error("429 should be retryable")
	}
	if kind != keypool.FailureRateLimitShort {
		t.Errorf("kind: want FailureRateLimitShort, got %v", kind)
	}
	if ra != 3*time.Second {
		t.Errorf("retryAfter: want 3s, got %v", ra)
	}
}

func TestRetryable_429_LongRetryAfter(t *testing.T) {
	a := anthropic.New()
	hdr := http.Header{"Retry-After": []string{strconv.Itoa(60)}}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: hdr}
	retry, kind, ra := a.Retryable(resp)
	if !retry {
		t.Error("429 should be retryable")
	}
	if kind != keypool.FailureRateLimitLong {
		t.Errorf("kind: want FailureRateLimitLong, got %v", kind)
	}
	if ra != 60*time.Second {
		t.Errorf("retryAfter: want 60s, got %v", ra)
	}
}

func TestRetryable_500(t *testing.T) {
	a := anthropic.New()
	resp := &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}}
	retry, kind, _ := a.Retryable(resp)
	if !retry {
		t.Error("500 should be retryable")
	}
	if kind != keypool.FailureServerError {
		t.Errorf("kind: want FailureServerError, got %v", kind)
	}
}

func TestRetryable_529_Overloaded(t *testing.T) {
	// 529 is Anthropic's "Overloaded" status — should be treated as server error.
	a := anthropic.New()
	resp := &http.Response{StatusCode: 529, Header: http.Header{}}
	retry, kind, _ := a.Retryable(resp)
	if !retry {
		t.Error("529 should be retryable")
	}
	if kind != keypool.FailureServerError {
		t.Errorf("kind: want FailureServerError, got %v", kind)
	}
}

// --- ExtractTokens ---

func TestExtractTokens_NonStreaming(t *testing.T) {
	a := anthropic.New()
	body := []byte(`{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-4-7",
		"content": [],
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"cache_creation_input_tokens": 25,
			"cache_read_input_tokens": 10
		}
	}`)
	tok := a.ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil Tokens")
	}
	if tok["input"] != 100 {
		t.Errorf("input: want 100, got %d", tok["input"])
	}
	if tok["output"] != 50 {
		t.Errorf("output: want 50, got %d", tok["output"])
	}
	if tok["cache_creation"] != 25 {
		t.Errorf("cache_creation: want 25, got %d", tok["cache_creation"])
	}
	if tok["cache_read"] != 10 {
		t.Errorf("cache_read: want 10, got %d", tok["cache_read"])
	}
}

func TestExtractTokens_NoUsage(t *testing.T) {
	a := anthropic.New()
	body := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)
	tok := a.ExtractTokens(body)
	if tok != nil {
		t.Errorf("expected nil for no-usage chunk, got %v", tok)
	}
}

func TestExtractTokens_Malformed(t *testing.T) {
	a := anthropic.New()
	tok := a.ExtractTokens([]byte(`{bad json`))
	if tok != nil {
		t.Errorf("expected nil for malformed JSON, got %v", tok)
	}
}
