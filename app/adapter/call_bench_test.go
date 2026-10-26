package adapter_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkCallSmallBody is one non-streamed upstream call with a typical chat-sized body and forwarded headers, against an in-process upstream that drains the body.
func BenchmarkCallSmallBody(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	b.Cleanup(srv.Close)

	a := newBearerSpec(b, "/v1/chat/completions").PipelineAdapter()
	body := bytes.Repeat([]byte("x"), 3<<10)
	hdr := http.Header{
		"Accept":           {"application/json"},
		"User-Agent":       {"bench/1.0"},
		"X-Client-Os":      {"Linux"},
		"X-Client-Version": {"1.2.3"},
	}

	b.ReportAllocs()
	for b.Loop() {
		resp, err := a.Call(context.Background(), srv.URL, nil, "sk-bench", body, hdr, "", false, false)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}
