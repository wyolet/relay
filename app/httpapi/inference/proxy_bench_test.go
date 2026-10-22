package inference

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkProxyLargeBody is one authed, policy-routed proxy request with a coding-agent-sized body, from the shape route to the upstream's response, against an in-process upstream that drains the body.
func BenchmarkProxyLargeBody(b *testing.B) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"id":"m","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	b.Cleanup(up.Close)
	d, pr, _ := proxyStreamDeps(b, up.URL)

	for _, tc := range []struct {
		name      string
		modelLast bool
	}{{"model-first", false}, {"model-last", true}} {
		body := claudeCodeSizedBody(b, 350<<10, tc.modelLast)
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				w := serveProxyShape(d, proxyShapeCall{principal: pr, body: bytes.NewReader(body), length: int64(len(body))})
				if w.Code != http.StatusOK {
					b.Fatalf("status %d: %s", w.Code, w.Body)
				}
			}
		})
	}
}
