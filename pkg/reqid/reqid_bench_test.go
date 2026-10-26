package reqid

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type discardWriter struct{ header http.Header }

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// BenchmarkMiddleware is the per-request cost of minting the id for a request that never logs.
func BenchmarkMiddleware(b *testing.B) {
	h := Middleware(slog.Default())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(HeaderInbound, "client-1")
	w := &discardWriter{header: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(w, r)
	}
}
