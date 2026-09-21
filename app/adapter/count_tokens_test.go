package adapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
)

// A spec that declares no CountPath has no counting endpoint to offer, and the capability assertion must fail so the caller estimates instead.
func TestPipelineAdapter_NoCountPath_IsNotATokenCounter(t *testing.T) {
	a := (&Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions"}).Build().PipelineAdapter()
	if _, ok := a.(TokenCounter); ok {
		t.Fatal("a spec without CountPath must not satisfy TokenCounter")
	}
}

func TestCountTokens_PostsToCountPathWithAuth(t *testing.T) {
	var gotPath, gotAuth, gotBeta, gotForwarded, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-version")
		gotForwarded = r.Header.Get("anthropic-beta")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"input_tokens":1234}`))
	}))
	defer srv.Close()

	spec := (&Spec{
		Name:        adapters.Anthropic,
		DefaultPath: "/v1/messages",
		CountPath:   "/v1/messages/count_tokens",
		Auth: AuthStrategy{
			Header:       "x-api-key",
			ExtraHeaders: map[string]string{"anthropic-version": "2023-06-01"},
		},
	}).Build()

	counter, ok := spec.PipelineAdapter().(TokenCounter)
	if !ok {
		t.Fatal("a spec with CountPath must satisfy TokenCounter")
	}

	hdr := http.Header{}
	hdr.Set("anthropic-beta", "some-beta")
	n, err := counter.CountTokens(t.Context(), srv.URL, "sk-test", []byte(`{"model":"m"}`), hdr, false)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}

	if n != 1234 {
		t.Errorf("count = %d, want 1234", n)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "sk-test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotBeta != "2023-06-01" {
		t.Errorf("spec extra header missing, got %q", gotBeta)
	}
	if gotForwarded != "some-beta" {
		t.Errorf("caller header not forwarded, got %q", gotForwarded)
	}
	if gotBody != `{"model":"m"}` {
		t.Errorf("body = %q", gotBody)
	}
}

// An upstream that rejects the count must surface as an error — never as a number the caller would take for a measurement.
func TestCountTokens_UpstreamErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()

	counter := (&Spec{Name: adapters.Anthropic, CountPath: "/count"}).Build().PipelineAdapter().(TokenCounter)
	n, err := counter.CountTokens(t.Context(), srv.URL, "k", []byte(`{}`), nil, false)
	if err == nil {
		t.Fatalf("want an error, got count %d", n)
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("error should carry the upstream wording, got %v", err)
	}
}

func TestCountTokens_UnparseableResponseIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"something_else":1}`))
	}))
	defer srv.Close()

	counter := (&Spec{Name: adapters.Anthropic, CountPath: "/count"}).Build().PipelineAdapter().(TokenCounter)
	if _, err := counter.CountTokens(t.Context(), srv.URL, "k", []byte(`{}`), nil, false); err == nil {
		t.Fatal("a response with no input_tokens must be an error")
	}
}
