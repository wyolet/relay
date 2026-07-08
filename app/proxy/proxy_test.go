package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
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

type recordingExtractor struct {
	saw  chan []byte
	toks sdkusage.Tokens
}

func (r *recordingExtractor) ExtractTokens(body []byte) sdkusage.Tokens {
	r.saw <- body
	return r.toks
}

// The proxy forwards Accept-Encoding verbatim, so the tee captures the
// body exactly as the upstream compressed it. The caller must receive
// those bytes untouched (transparency), while post-flight consumers —
// the extractor here — must see decoded plaintext.
func TestRunPostFlightDecodesCompressedUpstreamBody(t *testing.T) {
	const plaintext = `{"usage":{"prompt_tokens":8,"completion_tokens":16}}`
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write([]byte(plaintext))
	_ = gw.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gz.Bytes())
	}))
	defer upstream.Close()

	p := New(nil, nil, nil)
	p.Client = upstream.Client()
	// The client must not transparently gunzip, mirroring the real proxy
	// path where the forwarded Accept-Encoding disables that.
	headers := http.Header{}
	headers.Set("Accept-Encoding", "gzip")

	ex := &recordingExtractor{saw: make(chan []byte, 1)}
	res, err := p.Run(context.Background(), &Request{
		Method:       http.MethodPost,
		Path:         "/v1/chat/completions",
		Body:         strings.NewReader(`{}`),
		Headers:      headers,
		HostBaseURL:  upstream.URL,
		UpstreamAuth: "Bearer upstream-token",
		Extractor:    ex,
		Lifecycle:    lifecycle.NewContext("req-gz", "proxy", time.Now()),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wire, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()

	if !bytes.Equal(wire, gz.Bytes()) {
		t.Fatalf("caller must receive the compressed bytes verbatim; got %d bytes, want %d", len(wire), gz.Len())
	}
	select {
	case saw := <-ex.saw:
		if string(saw) != plaintext {
			t.Fatalf("extractor saw %q, want decoded plaintext", saw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight extractor never ran")
	}
}
