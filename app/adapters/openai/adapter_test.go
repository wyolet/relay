package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/keypool"
)

// TestCall_URLAndHeaders verifies that Call constructs the correct URL,
// sets Authorization and Content-Type, and forwards extra headers.
func TestCall_URLAndHeaders(t *testing.T) {
	var gotURL, gotAuth, gotCT, gotForwarded string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotForwarded = r.Header.Get("X-Custom")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test"}`))
	}))
	defer srv.Close()

	a := New(WithClient(srv.Client()))

	hdr := http.Header{}
	hdr.Set("X-Custom", "relay-test")

	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	resp, err := a.Call(t.Context(), srv.URL, "sk-test", body, hdr)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	defer resp.Body.Close()

	if gotURL != "/v1/chat/completions" {
		t.Errorf("URL path: want /v1/chat/completions, got %s", gotURL)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization: want 'Bearer sk-test', got %s", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type: want application/json, got %s", gotCT)
	}
	if gotForwarded != "relay-test" {
		t.Errorf("X-Custom: want relay-test, got %s", gotForwarded)
	}
	if string(gotBody) != string(body) {
		t.Errorf("body mismatch: want %s, got %s", body, gotBody)
	}
}

// TestCall_BodyForwarded verifies byte-equivalent body forwarding.
func TestCall_BodyForwarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(b) // echo back
	}))
	defer srv.Close()

	a := New(WithClient(srv.Client()))
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	resp, err := a.Call(t.Context(), srv.URL, "key", body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("echoed body mismatch")
	}
}

// TestRetryable covers the key status-code classifications.
func TestRetryable(t *testing.T) {
	a := New()

	cases := []struct {
		name        string
		status      int
		retryAfter  string
		wantRetry   bool
		wantKind    keypool.FailureKind
		wantRARange string // "short" | "long" | ""
	}{
		{"200 OK", 200, "", false, 0, ""},
		{"400 bad request", 400, "", false, 0, ""},
		{"401 unauthorized", 401, "", true, keypool.FailureAuth, ""},
		{"403 forbidden", 403, "", true, keypool.FailureAuth, ""},
		{"429 no retry-after", 429, "", true, keypool.FailureRateLimitShort, "short"},
		{"429 short retry-after", 429, "3", true, keypool.FailureRateLimitShort, "short"},
		{"429 long retry-after", 429, "30", true, keypool.FailureRateLimitLong, "long"},
		{"500 server error", 500, "", true, keypool.FailureServerError, ""},
		{"503 server error", 503, "", true, keypool.FailureServerError, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := http.Header{}
			if tc.retryAfter != "" {
				hdr.Set("Retry-After", tc.retryAfter)
			}
			resp := &http.Response{StatusCode: tc.status, Header: hdr}
			retry, kind, ra := a.Retryable(resp)
			if retry != tc.wantRetry {
				t.Errorf("retry: want %v, got %v", tc.wantRetry, retry)
			}
			if retry && kind != tc.wantKind {
				t.Errorf("kind: want %v, got %v", tc.wantKind, kind)
			}
			if tc.wantRARange == "short" && ra > 5*time.Second {
				t.Errorf("expected short retryAfter, got %v", ra)
			}
			if tc.wantRARange == "long" && ra <= 5*time.Second {
				t.Errorf("expected long retryAfter, got %v", ra)
			}
		})
	}
}

// TestRetryable_NilResp ensures nil response is handled defensively.
func TestRetryable_NilResp(t *testing.T) {
	a := New()
	retry, _, _ := a.Retryable(nil)
	if retry {
		t.Error("nil resp should not be retryable")
	}
}

// TestExtractTokens delegates to pkg/api/openai.ExtractTokens; just verify
// the round-trip through our adapter method.
func TestExtractTokens(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc",
		"choices": [],
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"total_tokens": 150,
			"prompt_tokens_details": {"cached_tokens": 20}
		}
	}`)

	a := New()
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
	if tok["cache_read"] != 20 {
		t.Errorf("cache_read: want 20, got %d", tok["cache_read"])
	}
}

// TestExtractTokens_NoUsage ensures nil is returned for chunks with no usage.
func TestExtractTokens_NoUsage(t *testing.T) {
	a := New()
	tok := a.ExtractTokens([]byte(`{"id":"x","choices":[{"delta":{"content":"hi"}}]}`))
	if tok != nil {
		t.Errorf("expected nil for mid-stream chunk, got %v", tok)
	}
}

// TestCall_NoAPIKey ensures no Authorization header is set when apiKey is empty.
func TestCall_NoAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := New(WithClient(srv.Client()))
	resp, err := a.Call(t.Context(), srv.URL, "", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if strings.HasPrefix(gotAuth, "Bearer") {
		t.Errorf("expected no auth header for empty key, got %q", gotAuth)
	}
}
